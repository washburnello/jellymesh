package daemon

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellymesh/internal/config"
	"jellymesh/internal/filmfile"
	"jellymesh/internal/jellyfin"
	"jellymesh/internal/jellyfin/jellyfintest"
	"jellymesh/internal/mount"
)

// findDescriptor returns the path, relative to root, of the one film
// descriptor whose name contains label.
func findDescriptor(t *testing.T, root string, label string) string {
	t.Helper()
	found := ""
	filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, filmfile.Suffix) && strings.Contains(filepath.Base(path), label) {
			found, _ = filepath.Rel(root, path)
		}
		return nil
	})
	return found
}

// C-FS-8: a remote film plays through the whole chain in FUSE presentation:
// the source publishes it, the destination writes a descriptor, the mount
// shows it as a file of the film's size beside its subtitle and poster, and
// reading it through the mount returns the source's bytes, fetched through
// the read socket; opting out stops reads at once, before any catalog pass,
// and a healed film's time moves an hour.
func TestARemoteFilmPlaysThroughTheMount(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil && os.Geteuid() != 0 {
		t.Skip("no fusermount3")
	}
	cedarJellyfin, walnutJellyfin := jellyfintest.New(), jellyfintest.New()
	defer cedarJellyfin.Close()
	defer walnutJellyfin.Close()
	cedarJellyfin.AddLibrary("lib-movies", "Movies", "movies")
	cedarJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-movies")
	cedarJellyfin.AddItem("lib-movies", jellyfin.Item{ID: "movie-1", Name: "Probe Film", ProductionYear: 2001, Type: "Movie",
		Path: "/media/movies/probe.mkv", ProviderIDs: map[string]string{"Tmdb": "603"}})
	media := bytes.Repeat([]byte("frame-of-film"), 250_000) // a little over 3 MiB
	cedarJellyfin.SetMedia("movie-1", media)
	cedarJellyfin.AddSubtitle("movie-1", 2, "eng", "1\n00:00:01,000 --> 00:00:02,000\nHello\n")
	cedarJellyfin.SetImage("movie-1", []byte("poster"))
	walnutJellyfin.AddLibrary("lib-docs", "Documentaries", "movies")
	walnutJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-docs")
	walnutJellyfin.AddItem("lib-docs", jellyfin.Item{ID: "doc-1", Name: "Oceans", Type: "Movie", Path: "/media/docs/oceans.mkv"})

	cedar := startDaemonWith(t, "cedar", t.TempDir(), "127.0.0.1:0", withServiceUser(cedarJellyfin))
	walnutDir := t.TempDir()
	walnut := startDaemonWith(t, "walnut", walnutDir, "127.0.0.1:0", func(cfg *config.Config) {
		withServiceUser(walnutJellyfin)(cfg)
		cfg.Presentation = config.PresentFUSE
		cfg.ReadCacheBytes = 64 << 20
	})
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	cedar.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-movies", Roots: []string{"/media/movies"}}, nil)
	walnut.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-docs", Roots: []string{"/media/docs"}}, nil)
	joinOffering(t, cedar, walnut, cedar, nil)
	cedar.catalogSync()
	if result := walnut.catalogSync(); result.Materialized.Written != 1 {
		t.Fatalf("walnut should materialize cedar's film: %+v", result.Materialized)
	}

	root := filepath.Join(walnutDir, "generated")
	relative := findDescriptor(t, root, " - cedar")
	if relative == "" {
		t.Fatal("no descriptor for cedar's film")
	}
	if _, err := os.Stat(filepath.Join(root, strings.TrimSuffix(relative, ".mkv"+filmfile.Suffix)+".eng.srt")); err != nil {
		t.Fatalf("the subtitle should sit beside the film: %v", err)
	}

	// The read socket, as `jellymesh serve` serves it.
	socket := filepath.Join(t.TempDir(), "read.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: walnut.node.ReadHandler()}
	go server.Serve(listener)
	defer server.Close()

	mountpoint := filepath.Join(t.TempDir(), "presented")
	m, err := mount.Start(mount.Options{Backing: root, Mountpoint: mountpoint, Reader: mount.NewSocketReader(socket),
		AllowedUIDs: []uint32{uint32(os.Getuid())}, CacheTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer m.Unmount()

	shown := filepath.Join(mountpoint, strings.TrimSuffix(relative, filmfile.Suffix))
	info, err := os.Stat(shown)
	if err != nil || info.Size() != int64(len(media)) {
		t.Fatalf("the film should be shown at its size: %v, %v", info, err)
	}
	if poster, err := os.ReadFile(filepath.Join(filepath.Dir(shown), "poster.jpg")); err != nil || string(poster) != "poster" {
		t.Fatalf("the poster should be shown: %v", err)
	}
	if content, err := os.ReadFile(shown); err != nil || !bytes.Equal(content, media) {
		t.Fatalf("the film through the mount: %d of %d bytes, %v", len(content), len(media), err)
	}

	// A healed film moves its time an hour, through the daemon's wiring.
	before, _ := os.Stat(shown)
	descriptor, _ := filmfile.Decode(mustRead(t, filepath.Join(root, relative)))
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	response, err := client.Post("http://jellymesh"+mount.FilmPath(descriptor.Reference)+"/failed", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	walnut.node.reads.HealOnce(context.Background())
	time.Sleep(300 * time.Millisecond)
	if after, err := os.Stat(shown); err != nil || after.ModTime().Sub(before.ModTime()) != time.Hour {
		t.Fatalf("a healed film's time should move an hour: %v to %v, %v", before.ModTime(), after.ModTime(), err)
	}

	// Opting out stops reads at once, before a pass removes the film.
	walnut.must(http.MethodPut, "/admin/v1/optouts/"+cedar.node.NodeID()+"/lib-movies", nil, nil)
	file, err := os.Open(shown)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buffer := make([]byte, 4096)
	_, err = file.ReadAt(buffer, 3<<20) // a chunk not yet held
	file.Close()
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("an opted-out film must not be readable: %v", err)
	}
	walnut.catalogSync()
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(shown); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an opted-out film should leave the mount: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
