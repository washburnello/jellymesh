// Command mount presents remote films to Jellyfin as ordinary files (spike
// #60). It is deliberately small: the directory tree, file sizes, and small
// files such as NFOs come from a local manifest and never touch the network,
// so a scan never waits on a source. Only a film's bytes are fetched, from
// the local reader, each read under a hard deadline, so a slow or dead source
// becomes an I/O error rather than a hung Jellyfin. Contents are served only
// to allowed users; anyone else sees names and sizes but cannot open a film.
//
// On start it detaches a mount a previous, crashed instance left behind, so
// recovery needs no manual step.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Manifest is the local state the tree is built from.
type Manifest struct {
	Files []Entry `json:"files"`
}

// Entry is one file: either remote media (Ref set) or small inline content.
type Entry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size,omitempty"`
	Mtime   int64  `json:"mtime,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Content string `json:"content,omitempty"`
}

var (
	readerURL      string
	deadline       time.Duration
	requestTimeout time.Duration
	allowedUIDs    = map[uint32]bool{}
	client         = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second}}

	reads, readErrors, denied atomic.Int64

	// frozen, set by SIGUSR2 in tests, makes every file handler block
	// forever: a deadlock inside a live process.
	frozen atomic.Bool
)

func freezeIfAsked() {
	for frozen.Load() {
		time.Sleep(time.Second)
	}
}

func allowed(ctx context.Context) bool {
	caller, ok := fuse.FromContext(ctx)
	return ok && allowedUIDs[caller.Uid]
}

type root struct {
	fs.Inode
	manifest Manifest
}

func (r *root) OnAdd(ctx context.Context) {
	r.AddChild(healthName, r.NewPersistentInode(ctx, &healthFile{}, fs.StableAttr{}), false)
	for _, entry := range r.manifest.Files {
		dir, name := path.Split(strings.Trim(entry.Path, "/"))
		parent := &r.Inode
		for _, component := range strings.Split(strings.Trim(dir, "/"), "/") {
			if component == "" {
				continue
			}
			child := parent.GetChild(component)
			if child == nil {
				child = parent.NewPersistentInode(ctx, &fs.Inode{}, fs.StableAttr{Mode: fuse.S_IFDIR})
				parent.AddChild(component, child, false)
			}
			parent = child
		}
		var node fs.InodeEmbedder
		if entry.Ref != "" {
			node = &remoteFile{entry: entry}
		} else {
			node = &smallFile{entry: entry}
		}
		parent.AddChild(name, parent.NewPersistentInode(ctx, node, fs.StableAttr{}), false)
	}
}

// healthName is read by this process's own watchdog. Jellyfin ignores
// dot-files, and only root (the mount's own user) may open it.
const healthName = ".jellymesh-health"

// healthFile always goes to the server (direct I/O), so reading it proves
// the mount is answering and not merely that the kernel has it cached.
type healthFile struct{ fs.Inode }

func (h *healthFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o400
	out.Size = 2
	return 0
}

func (h *healthFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if caller, ok := fuse.FromContext(ctx); !ok || caller.Uid != 0 {
		return nil, 0, syscall.EACCES
	}
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (h *healthFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	freezeIfAsked()
	if off > 0 {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData([]byte("ok")), 0
}

// watchdog exits the process when its own mount stops answering, whether the
// kernel has aborted the connection (after RequestTimeout) or the handlers
// are stuck. The container's restart policy then mounts afresh, and startup
// detaches the dead mount, so recovery needs no one.
func watchdog(mountpoint string, every, patience time.Duration) {
	health := path.Join(mountpoint, healthName)
	for {
		time.Sleep(every)
		done := make(chan error, 1)
		go func() {
			_, err := os.ReadFile(health)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				log.Printf("watchdog: mount unhealthy (%v); exiting to be restarted", err)
				os.Exit(3)
			}
		case <-time.After(patience):
			log.Printf("watchdog: mount did not answer within %s; exiting to be restarted", patience)
			os.Exit(3)
		}
	}
}

// smallFile is generated metadata held in the manifest.
type smallFile struct {
	fs.Inode
	entry Entry
}

var _ = (fs.NodeGetattrer)((*smallFile)(nil))
var _ = (fs.NodeOpener)((*smallFile)(nil))
var _ = (fs.NodeReader)((*smallFile)(nil))

func (f *smallFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o444
	out.Size = uint64(len(f.entry.Content))
	mtime := time.Unix(f.entry.Mtime, 0)
	out.SetTimes(nil, &mtime, &mtime)
	return 0
}

func (f *smallFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *smallFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	content := f.entry.Content
	if off >= int64(len(content)) {
		return fuse.ReadResultData(nil), 0
	}
	end := off + int64(len(dest))
	if end > int64(len(content)) {
		end = int64(len(content))
	}
	return fuse.ReadResultData([]byte(content[off:end])), 0
}

// remoteFile is a film whose bytes come from the reader.
type remoteFile struct {
	fs.Inode
	entry Entry

	// generation moves the reported modification time forward once a film
	// whose read failed is readable again. Jellyfin probes a file only when
	// it changes, so without this a film whose probe failed would stay
	// without media information forever.
	generation atomic.Int64
}

var (
	failedMutex sync.Mutex
	failed      = map[*remoteFile]bool{}
)

func (f *remoteFile) readFailed() {
	failedMutex.Lock()
	failed[f] = true
	failedMutex.Unlock()
}

// healFailed retries each failed film with a one-byte read; when one
// succeeds, its modification time moves on and the kernel's cached
// attributes are dropped, so the next scan probes it again.
func healFailed(every time.Duration) {
	for {
		time.Sleep(every)
		failedMutex.Lock()
		pending := make([]*remoteFile, 0, len(failed))
		for f := range failed {
			pending = append(pending, f)
		}
		failedMutex.Unlock()
		for _, f := range pending {
			if !readable(f.entry.Ref) {
				continue
			}
			f.generation.Add(1)
			f.NotifyContent(0, 0)
			failedMutex.Lock()
			delete(failed, f)
			failedMutex.Unlock()
			log.Printf("readable again, marked changed: %s", f.entry.Path)
		}
	}
}

func readable(ref string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/read?ref=%s&off=0&len=1", readerURL, ref), nil)
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

var _ = (fs.NodeGetattrer)((*remoteFile)(nil))
var _ = (fs.NodeOpener)((*remoteFile)(nil))
var _ = (fs.NodeReader)((*remoteFile)(nil))

func (f *remoteFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	freezeIfAsked()
	out.Mode = 0o444
	out.Size = uint64(f.entry.Size)
	mtime := time.Unix(f.entry.Mtime+3600*f.generation.Load(), 0)
	out.SetTimes(nil, &mtime, &mtime)
	return 0
}

func (f *remoteFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	freezeIfAsked()
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	if !allowed(ctx) {
		denied.Add(1)
		return nil, 0, syscall.EACCES
	}
	return nil, 0, 0
}

func (f *remoteFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	freezeIfAsked()
	if !allowed(ctx) {
		denied.Add(1)
		return nil, syscall.EACCES
	}
	if off >= f.entry.Size {
		return fuse.ReadResultData(nil), 0
	}
	length := int64(len(dest))
	if off+length > f.entry.Size {
		length = f.entry.Size - off
	}
	reads.Add(1)
	// The kernel cancels ctx if the reader is interrupted; the deadline
	// bounds everything else.
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/read?ref=%s&off=%d&len=%d", readerURL, f.entry.Ref, off, length), nil)
	response, err := client.Do(request)
	if err != nil {
		readErrors.Add(1)
		f.readFailed()
		return nil, syscall.EIO
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		readErrors.Add(1)
		f.readFailed()
		return nil, syscall.EIO
	}
	n, err := io.ReadFull(response.Body, dest[:length])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		readErrors.Add(1)
		f.readFailed()
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func main() {
	manifestPath := flag.String("manifest", "/state/manifest.json", "local manifest")
	mountpoint := flag.String("mountpoint", "/share/mnt", "where to mount")
	flag.StringVar(&readerURL, "reader", "http://fusespike-reader:8200", "reader base URL")
	flag.DurationVar(&deadline, "deadline", 10*time.Second, "hard deadline for one read")
	flag.DurationVar(&requestTimeout, "request-timeout", 20*time.Second, "kernel aborts the mount if any request waits this long")
	uids := flag.String("allow-uids", "7777", "comma-separated uids that may read contents")
	flag.Parse()
	for _, uid := range strings.Split(*uids, ",") {
		value, err := strconv.ParseUint(strings.TrimSpace(uid), 10, 32)
		if err != nil {
			log.Fatalf("bad uid %q", uid)
		}
		allowedUIDs[uint32(value)] = true
	}

	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		log.Fatalf("manifest: %v", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		log.Fatalf("manifest: %v", err)
	}

	// A crashed predecessor leaves a dead mount ("transport endpoint is not
	// connected"); detach it so this instance can mount in its place.
	_ = syscall.Unmount(*mountpoint, syscall.MNT_DETACH)
	if err := os.MkdirAll(*mountpoint, 0o755); err != nil {
		log.Fatalf("mountpoint: %v", err)
	}

	long := time.Hour
	negative := 5 * time.Second
	server, err := fs.Mount(*mountpoint, &root{manifest: manifest}, &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther:   true,
			DirectMount:  true,
			FsName:       "jellymesh",
			Name:         "jellymesh",
			MaxReadAhead: 1 << 20,
			// If this process ever freezes, the kernel aborts the
			// connection rather than leave Jellyfin waiting forever.
			RequestTimeout: uint16(requestTimeout / time.Second),
			MaxWrite:       1 << 20,
		},
		EntryTimeout:    &long,
		AttrTimeout:     &long,
		NegativeTimeout: &negative,
	})
	if err != nil {
		log.Fatalf("mount: %v", err)
	}
	log.Printf("mounted %d files at %s", len(manifest.Files), *mountpoint)

	go watchdog(*mountpoint, 5*time.Second, 15*time.Second)
	go healFailed(10 * time.Second)
	go func() {
		for range time.Tick(time.Minute) {
			log.Printf("reads=%d errors=%d denied=%d", reads.Load(), readErrors.Load(), denied.Load())
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for received := range signals {
			switch received {
			case syscall.SIGUSR1: // test: crash without unmounting
				log.Printf("simulated crash")
				os.Exit(2)
			case syscall.SIGUSR2: // test: deadlock every file handler
				log.Printf("simulated deadlock")
				frozen.Store(true)
			default:
				_ = server.Unmount()
			}
		}
	}()
	server.Wait()
}
