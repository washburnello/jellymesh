// Package mount presents the generated root to Jellyfin through FUSE
// (design-spec section 11, A-17). It shows the generated root read-only,
// with each film descriptor (package filmfile) shown as the film itself: a
// file of the film's size whose bytes come from the daemon. Everything else,
// folders, NFOs, posters, and subtitles, is shown as it is on disk.
//
// The mount is built so that nothing it does can hang Jellyfin:
//
//   - listings, sizes, and small files come from local disk, so a library
//     scan never waits on a source;
//   - every film read has a hard deadline and fails with an I/O error;
//   - the kernel is asked to abort the connection if any request waits
//     longer than RequestTimeout, so even a frozen process cannot leave
//     Jellyfin's threads blocked;
//   - a watchdog reads a health file through the mount and reports when it
//     stops answering, so the process can exit and be restarted, and start
//     detaches whatever mount a crashed predecessor left.
//
// Film contents are served only to the allowed uids (Jellyfin's). Anyone
// else sees names and sizes but cannot read a film.
package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"jellymesh/internal/filmfile"
)

// Reader fetches film bytes; SocketReader reads them from the daemon.
type Reader interface {
	// Read fills dest from offset, returning fewer bytes only at the end of
	// the film. It must honour ctx.
	Read(ctx context.Context, film filmfile.Descriptor, dest []byte, offset int64) (int, error)
	// Failed reports, best effort, that a read of film failed, so that the
	// daemon retries it and marks it changed once it is readable (A-17).
	Failed(film filmfile.Descriptor)
}

// HealthName is the health file at the mount's root. Jellyfin ignores
// dot-files, and only the mount's own user may open it.
const HealthName = ".jellymesh-health"

// Options configure a mount.
type Options struct {
	// Backing is the generated root the materializer writes.
	Backing string
	// Mountpoint is where the films are shown.
	Mountpoint string
	Reader     Reader
	// AllowedUIDs may read film contents.
	AllowedUIDs []uint32
	// ReadDeadline bounds one film read (default 10 s).
	ReadDeadline time.Duration
	// RequestTimeout is the kernel's abort timeout (default 20 s).
	RequestTimeout time.Duration
	// CacheTimeout is how long the kernel keeps names and attributes
	// (default one minute). Changes on disk show within it.
	CacheTimeout time.Duration
	// WatchEvery and WatchPatience pace the watchdog (defaults 5 s, 15 s).
	WatchEvery, WatchPatience time.Duration
	// AllowOther lets other users use the mount; it is always on in
	// deployment, where Jellyfin runs as another user.
	AllowOther bool
	// AllowWithoutRequestTimeout permits a kernel that does not offer
	// request timeouts. Only tests set it; deployment refuses to run.
	AllowWithoutRequestTimeout bool
	Logger                     *log.Logger
}

// ErrNoRequestTimeout means the kernel cannot abort a frozen mount, so the
// mount refuses to run (design-spec section 11).
var ErrNoRequestTimeout = errors.New("this kernel does not offer FUSE request timeouts (Linux 6.14 or later is required)")

// offersRequestTimeout reports whether the kernel offered request timeouts
// when the mount started. Tests replace it to stand in for an older kernel.
var offersRequestTimeout = func(server *fuse.Server) bool {
	return server.KernelSettings().Flags64()&fuse.CAP_REQUEST_TIMEOUT != 0
}

// Stats count what the mount has served.
type Stats struct {
	Reads, ReadErrors, Denied int64
}

// Mount is a running mount.
type Mount struct {
	options Options
	server  *fuse.Server
	allowed map[uint32]bool
	self    uint32
	inodes  inodeNumbers
	frozen  atomic.Bool

	reads, readErrors, denied atomic.Int64

	started        time.Time
	requestTimeout bool

	unhealthy chan error
	stop      chan struct{}
	stopOnce  sync.Once
}

func (options *Options) defaults() {
	if options.ReadDeadline == 0 {
		options.ReadDeadline = 10 * time.Second
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 20 * time.Second
	}
	if options.CacheTimeout == 0 {
		options.CacheTimeout = time.Minute
	}
	if options.WatchEvery == 0 {
		options.WatchEvery = 5 * time.Second
	}
	if options.WatchPatience == 0 {
		options.WatchPatience = 15 * time.Second
	}
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}
}

// Start mounts and starts the watchdog. It first detaches any mount left at
// the mountpoint by a crashed predecessor.
func Start(options Options) (*Mount, error) {
	options.defaults()
	if options.Reader == nil {
		return nil, errors.New("a reader is required")
	}
	if len(options.AllowedUIDs) == 0 {
		return nil, errors.New("at least one uid must be allowed to read films")
	}
	if info, err := os.Stat(options.Backing); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("the generated root %q is not a readable directory", options.Backing)
	}
	m := &Mount{options: options, allowed: map[uint32]bool{}, self: uint32(os.Geteuid()),
		inodes: inodeNumbers{numbers: map[string]uint64{}, next: 2}, unhealthy: make(chan error, 1), stop: make(chan struct{})}
	for _, uid := range options.AllowedUIDs {
		m.allowed[uid] = true
	}

	// A crashed predecessor leaves a dead mount ("transport endpoint is not
	// connected"); detach it so this one can take its place.
	detach(options.Mountpoint)
	if err := os.MkdirAll(options.Mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("mountpoint: %w", err)
	}

	timeout := options.CacheTimeout
	negative := 5 * time.Second
	if negative > timeout {
		negative = timeout
	}
	server, err := fs.Mount(options.Mountpoint, &node{mount: m, kind: kindDir}, &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther:   options.AllowOther,
			DirectMount:  true,
			FsName:       "jellymesh",
			Name:         "jellymesh",
			Options:      []string{"ro"},
			MaxReadAhead: 1 << 20,
			MaxWrite:     1 << 20,
			// Replies come from memory, so splicing gains nothing, and a
			// 1 MiB reply does not fit the default pipe.
			DisableSplice:  true,
			RequestTimeout: uint16(options.RequestTimeout / time.Second),
		},
		EntryTimeout:    &timeout,
		AttrTimeout:     &timeout,
		NegativeTimeout: &negative,
	})
	if err != nil {
		return nil, fmt.Errorf("mount: %w", err)
	}
	m.server, m.started = server, time.Now()
	m.requestTimeout = offersRequestTimeout(server)
	if !m.requestTimeout && !options.AllowWithoutRequestTimeout {
		_ = server.Unmount()
		return nil, ErrNoRequestTimeout
	}
	go m.watchdog()
	return m, nil
}

// Wait blocks until the mount is unmounted, returning nil, or stops
// answering, returning why. A caller in deployment then exits, so that the
// container is restarted and mounts afresh.
func (m *Mount) Wait() error {
	done := make(chan struct{})
	go func() {
		m.server.Wait()
		close(done)
	}()
	select {
	case <-done:
		m.halt()
		return nil
	case err := <-m.unhealthy:
		m.halt()
		return err
	}
}

// Unmount unmounts.
func (m *Mount) Unmount() error {
	m.halt()
	return m.server.Unmount()
}

func (m *Mount) halt() { m.stopOnce.Do(func() { close(m.stop) }) }

// Stats returns the counters.
func (m *Mount) Stats() Stats {
	return Stats{Reads: m.reads.Load(), ReadErrors: m.readErrors.Load(), Denied: m.denied.Load()}
}

// Freeze makes every handler block, for tests of the kernel's request
// timeout and the watchdog: a deadlock inside a live process.
func (m *Mount) Freeze() { m.frozen.Store(true) }

func (m *Mount) freezeIfAsked() {
	for m.frozen.Load() {
		time.Sleep(100 * time.Millisecond)
	}
}

// watchdog reads the health file through the mount. Reading goes to this
// process (direct I/O), so it proves the mount answers, not merely that the
// kernel has it cached.
func (m *Mount) watchdog() {
	health := filepath.Join(m.options.Mountpoint, HealthName)
	ticker := time.NewTicker(m.options.WatchEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}
		done := make(chan error, 1)
		go func() {
			_, err := os.ReadFile(health)
			done <- err
		}()
		var failure error
		select {
		case err := <-done:
			if err != nil {
				failure = fmt.Errorf("the mount is unhealthy: %w", err)
			}
		case <-time.After(m.options.WatchPatience):
			failure = fmt.Errorf("the mount did not answer within %s", m.options.WatchPatience)
		case <-m.stop:
			return
		}
		if failure != nil {
			select {
			case m.unhealthy <- failure:
			default:
			}
			return
		}
	}
}

func detach(mountpoint string) {
	if err := syscall.Unmount(mountpoint, syscall.MNT_DETACH); err == nil || os.Geteuid() == 0 {
		return
	}
	// Without privilege, a mount made through fusermount is detached with
	// it.
	for _, name := range []string{"fusermount3", "fusermount"} {
		if binary, err := exec.LookPath(name); err == nil {
			_ = exec.Command(binary, "-u", "-z", mountpoint).Run()
			return
		}
	}
}

// inodeNumbers gives each shown path a stable inode number for the life of
// the process. Numbers from the backing disk are not used: the disk reuses
// a deleted file's number, which would let a new file inherit an old one's
// node.
type inodeNumbers struct {
	mutex   sync.Mutex
	numbers map[string]uint64
	next    uint64
}

func (numbers *inodeNumbers) of(key string) uint64 {
	numbers.mutex.Lock()
	defer numbers.mutex.Unlock()
	if number, ok := numbers.numbers[key]; ok {
		return number
	}
	number := numbers.next
	numbers.next++
	numbers.numbers[key] = number
	return number
}

type kind int

const (
	kindDir kind = iota
	kindFile
	kindFilm
	kindHealth
)

// node is one shown entry. It holds only its path; everything else is read
// from disk when asked, so a change on disk is shown once the kernel's
// cache expires.
type node struct {
	fs.Inode
	mount    *Mount
	kind     kind
	relative string // shown path relative to the root, "" for the root
}

var (
	_ = (fs.NodeLookuper)((*node)(nil))
	_ = (fs.NodeReaddirer)((*node)(nil))
	_ = (fs.NodeGetattrer)((*node)(nil))
	_ = (fs.NodeOpener)((*node)(nil))
	_ = (fs.NodeReader)((*node)(nil))
)

// backing returns the path on disk of a shown path: the descriptor for a
// film, the file itself otherwise.
func (n *node) backing() string {
	target := filepath.Join(n.mount.options.Backing, filepath.FromSlash(n.relative))
	if n.kind == kindFilm {
		target += filmfile.Suffix
	}
	return target
}

func (n *node) descriptor() (filmfile.Descriptor, syscall.Errno) {
	file, err := os.Open(n.backing())
	if err != nil {
		return filmfile.Descriptor{}, toErrno(err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, filmfile.MaxSize+1))
	if err != nil {
		return filmfile.Descriptor{}, syscall.EIO
	}
	film, err := filmfile.Decode(content)
	if err != nil {
		return filmfile.Descriptor{}, syscall.EIO
	}
	return film, 0
}

func toErrno(err error) syscall.Errno {
	if errors.Is(err, os.ErrNotExist) {
		return syscall.ENOENT
	}
	return fs.ToErrno(err)
}

// hidden reports names never shown: dot-files, including the materializer's
// temporary files, and descriptors under their own names.
func hidden(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, filmfile.Suffix)
}

func (n *node) child(ctx context.Context, name string, kind kind, out *fuse.EntryOut) *fs.Inode {
	relative := path.Join(n.relative, name)
	child := &node{mount: n.mount, kind: kind, relative: relative}
	mode := uint32(fuse.S_IFREG)
	if kind == kindDir {
		mode = fuse.S_IFDIR
	}
	var attributes fuse.AttrOut
	if errno := child.Getattr(ctx, nil, &attributes); errno != 0 {
		return nil
	}
	out.Attr = attributes.Attr
	ino := n.mount.inodes.of(fmt.Sprintf("%d/%s", kind, relative))
	return n.NewInode(ctx, child, fs.StableAttr{Mode: mode, Ino: ino})
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.mount.freezeIfAsked()
	if n.kind != kindDir {
		return nil, syscall.ENOTDIR
	}
	if n.relative == "" && name == HealthName {
		child := &node{mount: n.mount, kind: kindHealth}
		var attributes fuse.AttrOut
		child.Getattr(ctx, nil, &attributes)
		out.Attr = attributes.Attr
		return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG, Ino: 1 << 62}), 0
	}
	if hidden(name) {
		return nil, syscall.ENOENT
	}
	directory := filepath.Join(n.mount.options.Backing, filepath.FromSlash(n.relative))
	if info, err := os.Lstat(filepath.Join(directory, name+filmfile.Suffix)); err == nil && info.Mode().IsRegular() {
		if inode := n.child(ctx, name, kindFilm, out); inode != nil {
			return inode, 0
		}
		return nil, syscall.EIO
	}
	info, err := os.Lstat(filepath.Join(directory, name))
	if err != nil {
		return nil, toErrno(err)
	}
	var found kind
	switch {
	case info.IsDir():
		found = kindDir
	case info.Mode().IsRegular():
		found = kindFile
	default:
		return nil, syscall.ENOENT // links and devices are never shown
	}
	inode := n.child(ctx, name, found, out)
	if inode == nil {
		return nil, syscall.ENOENT
	}
	return inode, 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	n.mount.freezeIfAsked()
	entries, err := os.ReadDir(filepath.Join(n.mount.options.Backing, filepath.FromSlash(n.relative)))
	if err != nil {
		return nil, toErrno(err)
	}
	shown := make([]fuse.DirEntry, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if film, ok := filmfile.Shown(name); ok && entry.Type().IsRegular() {
			seen[film] = true
			shown = append(shown, fuse.DirEntry{Name: film, Mode: fuse.S_IFREG,
				Ino: n.mount.inodes.of(fmt.Sprintf("%d/%s", kindFilm, path.Join(n.relative, film)))})
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if hidden(name) || seen[name] {
			continue
		}
		switch {
		case entry.IsDir():
			shown = append(shown, fuse.DirEntry{Name: name, Mode: fuse.S_IFDIR,
				Ino: n.mount.inodes.of(fmt.Sprintf("%d/%s", kindDir, path.Join(n.relative, name)))})
		case entry.Type().IsRegular():
			shown = append(shown, fuse.DirEntry{Name: name, Mode: fuse.S_IFREG,
				Ino: n.mount.inodes.of(fmt.Sprintf("%d/%s", kindFile, path.Join(n.relative, name)))})
		}
	}
	return fs.NewListDirStream(shown), 0
}

func (n *node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mount.freezeIfAsked()
	switch n.kind {
	case kindHealth:
		out.Mode = fuse.S_IFREG | 0o400
		out.Size = 2
		return 0
	case kindFilm:
		film, errno := n.descriptor()
		if errno != 0 {
			return errno
		}
		modified := film.ModifiedTime()
		out.Mode = fuse.S_IFREG | 0o444
		out.Size = uint64(film.Size)
		out.Blocks = (out.Size + 511) / 512
		out.SetTimes(&modified, &modified, &modified)
		return 0
	}
	info, err := os.Lstat(n.backing())
	if err != nil {
		return toErrno(err)
	}
	modified := info.ModTime()
	out.SetTimes(&modified, &modified, &modified)
	if n.kind == kindDir {
		out.Mode = fuse.S_IFDIR | 0o555
		out.Nlink = 2
		return 0
	}
	out.Mode = fuse.S_IFREG | 0o444
	out.Size = uint64(info.Size())
	out.Blocks = (out.Size + 511) / 512
	return 0
}

func (n *node) caller(ctx context.Context) (uint32, bool) {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return 0, false
	}
	return caller.Uid, true
}

// filmHandle is an open film: the descriptor read when it was opened.
type filmHandle struct{ film filmfile.Descriptor }

// fileHandle is an open small file on disk.
type fileHandle struct{ file *os.File }

var _ = (fs.FileReleaser)((*fileHandle)(nil))

func (handle *fileHandle) Release(ctx context.Context) syscall.Errno {
	handle.file.Close()
	return 0
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.mount.freezeIfAsked()
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	switch n.kind {
	case kindHealth:
		if uid, ok := n.caller(ctx); !ok || uid != n.mount.self {
			return nil, 0, syscall.EACCES
		}
		return nil, fuse.FOPEN_DIRECT_IO, 0
	case kindFilm:
		if uid, ok := n.caller(ctx); !ok || !n.mount.allowed[uid] {
			n.mount.denied.Add(1)
			return nil, 0, syscall.EACCES
		}
		film, errno := n.descriptor()
		if errno != 0 {
			return nil, 0, errno
		}
		return &filmHandle{film: film}, 0, 0
	case kindFile:
		file, err := os.Open(n.backing())
		if err != nil {
			return nil, 0, toErrno(err)
		}
		return &fileHandle{file: file}, 0, 0
	}
	return nil, 0, syscall.EISDIR
}

func (n *node) Read(ctx context.Context, fh fs.FileHandle, dest []byte, offset int64) (fuse.ReadResult, syscall.Errno) {
	n.mount.freezeIfAsked()
	switch handle := fh.(type) {
	case *fileHandle:
		count, err := handle.file.ReadAt(dest, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, syscall.EIO
		}
		return fuse.ReadResultData(dest[:count]), 0
	case *filmHandle:
		return n.readFilm(ctx, handle.film, dest, offset)
	}
	if n.kind == kindHealth {
		if offset > 0 {
			return fuse.ReadResultData(nil), 0
		}
		return fuse.ReadResultData([]byte("ok")), 0
	}
	return nil, syscall.EBADF
}

func (n *node) readFilm(ctx context.Context, film filmfile.Descriptor, dest []byte, offset int64) (fuse.ReadResult, syscall.Errno) {
	// The handle was opened by an allowed user, but a descriptor passed to
	// another process is read under that process's uid: check every read.
	if uid, ok := n.caller(ctx); !ok || !n.mount.allowed[uid] {
		n.mount.denied.Add(1)
		return nil, syscall.EACCES
	}
	if offset >= film.Size {
		return fuse.ReadResultData(nil), 0
	}
	length := int64(len(dest))
	if offset+length > film.Size {
		length = film.Size - offset
	}
	n.mount.reads.Add(1)
	// The kernel cancels ctx if the reader is interrupted; the deadline
	// bounds everything else.
	ctx, cancel := context.WithTimeout(ctx, n.mount.options.ReadDeadline)
	defer cancel()
	count, err := n.mount.options.Reader.Read(ctx, film, dest[:length], offset)
	if err != nil || int64(count) != length {
		n.mount.readErrors.Add(1)
		if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// A failure, not the reader giving up: retry and mark changed.
			go n.mount.options.Reader.Failed(film)
		}
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:count]), 0
}
