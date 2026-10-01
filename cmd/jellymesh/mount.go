package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"jellymesh/internal/mount"
)

// runMount runs the mount process (design-spec section 11, A-17): it shows
// the generated root at the mountpoint, films as files, until it is stopped
// or stops answering. It exits 3 when its watchdog finds the mount dead, so
// that the container's restart policy mounts afresh.
//
// It reads only JELLYMESH_MOUNT_* and JELLYMESH_READ_SOCKET, never the
// node's configuration: the mount holds no key, token, or address.
func runMount(args []string) {
	flags := flag.NewFlagSet("mount", flag.ExitOnError)
	backing := flags.String("generated", env("JELLYMESH_MOUNT_GENERATED", "/generated"), "the generated root the node writes")
	mountpoint := flags.String("mountpoint", env("JELLYMESH_MOUNT_POINT", "/presented"), "where to show the films")
	socket := flags.String("socket", env("JELLYMESH_READ_SOCKET", "/run/jellymesh/read.sock"), "the node's read socket")
	uids := flags.String("allow-uids", env("JELLYMESH_MOUNT_ALLOW_UIDS", ""), "comma-separated uids that may read films (Jellyfin's)")
	faults := flags.Bool("test-faults", env("JELLYMESH_MOUNT_TEST_FAULTS", "") == "1", "SIGUSR1 crashes, SIGUSR2 freezes (for tests)")
	check := flags.Bool("check", false, "test whether this host can show films through FUSE, print why, and exit")
	flags.Parse(args)

	if *check {
		findings, ok := mount.Check(*mountpoint)
		for _, finding := range findings {
			mark := "ok  "
			if !finding.OK {
				mark = "FAIL"
			}
			fmt.Printf("%s %-16s %s\n", mark, finding.Check, finding.Detail)
		}
		if !ok {
			fmt.Println("\nThis host cannot present films through FUSE; keep JELLYMESH_PRESENTATION=strm.")
			os.Exit(1)
		}
		fmt.Println("\nThis host can present films through FUSE (JELLYMESH_PRESENTATION=fuse).")
		return
	}

	var allowed []uint32
	for _, field := range strings.Split(*uids, ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		uid, err := strconv.ParseUint(field, 10, 32)
		if err != nil {
			log.Fatalf("mount: JELLYMESH_MOUNT_ALLOW_UIDS must list uids: %q", field)
		}
		allowed = append(allowed, uint32(uid))
	}
	if len(allowed) == 0 {
		log.Fatal("mount: set JELLYMESH_MOUNT_ALLOW_UIDS to the uid Jellyfin runs as")
	}

	reader := mount.NewSocketReader(*socket)
	m, err := mount.Start(mount.Options{
		Backing: *backing, Mountpoint: *mountpoint, Reader: reader,
		AllowedUIDs: allowed, AllowOther: true, Logger: log.Default(),
	})
	if err != nil {
		log.Fatalf("mount: %v", err)
	}
	log.Printf("mount: showing %s at %s for uids %v, reading films through %s", *backing, *mountpoint, allowed, *socket)

	go func() {
		// Report at once, then on every interval, so status shows the mount.
		for tick := 0; ; tick++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := reader.Report(ctx, m.Report()); err != nil && tick%10 == 0 {
				log.Printf("mount: reporting to the node: %v", err)
			}
			cancel()
			if tick%2 == 1 {
				stats := m.Stats()
				log.Printf("mount: reads=%d errors=%d denied=%d", stats.Reads, stats.ReadErrors, stats.Denied)
			}
			time.Sleep(mount.ReportInterval)
		}
	}()
	var stopping atomic.Bool
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for received := range signals {
			switch {
			case received == syscall.SIGUSR1 && *faults:
				log.Print("mount: simulated crash")
				os.Exit(2)
			case received == syscall.SIGUSR2 && *faults:
				log.Print("mount: simulated deadlock")
				m.Freeze()
			case received == syscall.SIGTERM || received == syscall.SIGINT:
				stopping.Store(true)
				if err := m.Unmount(); err != nil {
					log.Printf("mount: unmount: %v", err)
					os.Exit(1)
				}
			}
		}
	}()
	if err := m.Wait(); err != nil {
		log.Printf("mount: %v; exiting to be restarted", err)
		os.Exit(3)
	}
	if !stopping.Load() {
		// The kernel aborted the connection (its request timeout) or it
		// was unmounted from outside: not a stop anyone asked for, so exit
		// as a failure, for any restart policy.
		log.Print("mount: the mount ended without being asked to; exiting to be restarted")
		os.Exit(3)
	}
}

func env(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// listenReadSocket opens the read socket for the mount. Only this user and
// root, the mount's, may connect.
func listenReadSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove a stale read socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func serveReads(handler http.Handler, path string) (func(), error) {
	listener, err := listenReadSocket(path)
	if err != nil {
		return nil, fmt.Errorf("read socket: %w", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("read socket: %v", err)
		}
	}()
	return func() { server.Close() }, nil
}
