package mount

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"jellymesh/internal/filmfile"
)

const reference = "0123456789abcdef0123456789abcdef"

type reader struct {
	content []byte
	block   atomic.Bool
	failed  chan string
	reads   atomic.Int64
}

func (r *reader) Read(ctx context.Context, film filmfile.Descriptor, dest []byte, offset int64) (int, error) {
	r.reads.Add(1)
	if r.block.Load() {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if film.Reference != reference {
		return 0, errors.New("unknown film")
	}
	return copy(dest, r.content[offset:]), nil
}

func (r *reader) Failed(film filmfile.Descriptor) { r.failed <- film.Reference }

type fixture struct {
	backing, mountpoint string
	reader              *reader
	mount               *Mount
}

func requireFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil && os.Geteuid() != 0 {
			t.Skip("no fusermount")
		}
	}
}

func write(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T, configure func(*Options)) *fixture {
	t.Helper()
	requireFUSE(t)
	base := t.TempDir()
	content := make([]byte, 3<<20+77)
	for index := range content {
		content[index] = byte(index * 13 % 253)
	}
	f := &fixture{backing: filepath.Join(base, "generated"), mountpoint: filepath.Join(base, "presented"),
		reader: &reader{content: content, failed: make(chan string, 8)}}
	folder := filepath.Join(f.backing, "Movies", "Film (2001) [jmid-1]")
	write(t, filepath.Join(folder, "Film (2001) [jmid-1] - Cedar.mkv"+filmfile.Suffix),
		filmfile.Descriptor{Reference: reference, Size: int64(len(content)), Modified: 1_700_000_000}.Encode())
	write(t, filepath.Join(folder, "Film (2001) [jmid-1] - Cedar.eng.srt"), []byte("1\n00:00:01,000 --> 00:00:02,000\nhello\n"))
	write(t, filepath.Join(folder, "movie.nfo"), []byte("<movie><title>Film</title></movie>"))
	write(t, filepath.Join(folder, ".jellymesh-tmp-123"), []byte("half written"))
	os.MkdirAll(filepath.Join(f.backing, "TV Shows"), 0o755)

	options := Options{Backing: f.backing, Mountpoint: f.mountpoint, Reader: f.reader,
		AllowedUIDs: []uint32{uint32(os.Getuid())}, ReadDeadline: 500 * time.Millisecond,
		CacheTimeout: 100 * time.Millisecond, WatchEvery: 100 * time.Millisecond, WatchPatience: 500 * time.Millisecond}
	if configure != nil {
		configure(&options)
	}
	mount, err := Start(options)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mount = mount
	t.Cleanup(func() {
		mount.frozen.Store(false)
		mount.Unmount()
	})
	return f
}

func (f *fixture) shown(relative string) string {
	return filepath.Join(f.mountpoint, filepath.FromSlash(relative))
}

const filmPath = "Movies/Film (2001) [jmid-1]/Film (2001) [jmid-1] - Cedar.mkv"

// C-FS-2: the mount shows the generated root read-only, each descriptor as
// its film with the film's size and time, and hides descriptors and
// temporary files.
func TestTheMountShowsFilmsAsFiles(t *testing.T) {
	f := start(t, nil)
	entries, err := os.ReadDir(f.shown("Movies/Film (2001) [jmid-1]"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	want := []string{"Film (2001) [jmid-1] - Cedar.eng.srt", "Film (2001) [jmid-1] - Cedar.mkv", "movie.nfo"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("listing %q, want %q", names, want)
	}
	info, err := os.Stat(f.shown(filmPath))
	if err != nil || info.Size() != int64(len(f.reader.content)) || info.ModTime().Unix() != 1_700_000_000 || info.Mode().Perm() != 0o444 {
		t.Fatalf("film attributes: %v, %v", info, err)
	}
	if _, err := os.Stat(f.shown(filmPath + filmfile.Suffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a descriptor is shown under its own name: %v", err)
	}
	if _, err := os.Stat(f.shown("Movies/Film (2001) [jmid-1]/.jellymesh-tmp-123")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a temporary file is shown: %v", err)
	}
	if nfo, err := os.ReadFile(f.shown("Movies/Film (2001) [jmid-1]/movie.nfo")); err != nil || string(nfo) != "<movie><title>Film</title></movie>" {
		t.Fatalf("nfo: %q, %v", nfo, err)
	}
	film, err := os.ReadFile(f.shown(filmPath))
	if err != nil || !bytes.Equal(film, f.reader.content) {
		t.Fatalf("film bytes differ (%d of %d), %v", len(film), len(f.reader.content), err)
	}
	file, err := os.Open(f.shown(filmPath))
	if err != nil {
		t.Fatal(err)
	}
	middle := make([]byte, 4096)
	if _, err := file.ReadAt(middle, 2<<20+5); err != nil || !bytes.Equal(middle, f.reader.content[2<<20+5:2<<20+5+4096]) {
		t.Fatalf("a seek read differs: %v", err)
	}
	file.Close()

	for _, attempt := range []func() error{
		func() error { _, err := os.OpenFile(f.shown(filmPath), os.O_WRONLY, 0); return err },
		func() error { return os.WriteFile(f.shown("Movies/new.nfo"), []byte("x"), 0o644) },
		func() error { return os.Remove(f.shown(filmPath)) },
		func() error { return os.Mkdir(f.shown("Movies/new"), 0o755) },
	} {
		if err := attempt(); !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("a change through the mount should be refused: %v", err)
		}
	}
}

// C-FS-2: changes on disk are shown once the kernel's cache expires, without
// a remount.
func TestChangesOnDiskAreShown(t *testing.T) {
	f := start(t, nil)
	if _, err := os.Stat(f.shown(filmPath)); err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(f.backing, filepath.FromSlash(filmPath)+filmfile.Suffix)
	write(t, descriptor, filmfile.Descriptor{Reference: reference, Size: 1000, Modified: 1_700_003_600}.Encode())
	write(t, filepath.Join(f.backing, "Movies", "Second (2002) [jmid-2]", "Second (2002) [jmid-2] - Cedar.mp4"+filmfile.Suffix),
		filmfile.Descriptor{Reference: reference, Size: 10, Modified: 1}.Encode())
	time.Sleep(300 * time.Millisecond)
	info, err := os.Stat(f.shown(filmPath))
	if err != nil || info.Size() != 1000 || info.ModTime().Unix() != 1_700_003_600 {
		t.Fatalf("a changed descriptor should be shown: %v, %v", info, err)
	}
	if _, err := os.Stat(f.shown("Movies/Second (2002) [jmid-2]/Second (2002) [jmid-2] - Cedar.mp4")); err != nil {
		t.Fatalf("a new film should be shown: %v", err)
	}
	os.Remove(descriptor)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(f.shown(filmPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a removed film should go: %v", err)
	}
}

// C-FS-2: film contents are only for the allowed uids. Anyone else sees the
// film's name and size, and reads its metadata, but cannot open it.
func TestOnlyAllowedUsersReadFilms(t *testing.T) {
	f := start(t, func(options *Options) { options.AllowedUIDs = []uint32{uint32(os.Getuid()) + 1} })
	if _, err := os.Stat(f.shown(filmPath)); err != nil {
		t.Fatalf("the name and size are visible to all: %v", err)
	}
	if _, err := os.ReadFile(f.shown(filmPath)); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("a film was readable by a user not allowed: %v", err)
	}
	if _, err := os.ReadFile(f.shown("Movies/Film (2001) [jmid-1]/movie.nfo")); err != nil {
		t.Fatalf("metadata stays readable: %v", err)
	}
	if f.reader.reads.Load() != 0 || f.mount.Stats().Denied == 0 {
		t.Fatal("a refused open must not reach the reader, and is counted")
	}
}

// C-FS-3: a film read that does not complete within the deadline fails with
// an I/O error, and is reported for healing.
func TestAStuckReadFailsWithinTheDeadline(t *testing.T) {
	f := start(t, nil)
	f.reader.block.Store(true)
	started := time.Now()
	_, err := os.ReadFile(f.shown(filmPath))
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("a stuck read should fail with EIO: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the read took %s", elapsed)
	}
	select {
	case got := <-f.reader.failed:
		if got != reference {
			t.Fatalf("reported %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the failure should be reported")
	}
	f.reader.block.Store(false)
	if _, err := os.ReadFile(f.shown(filmPath)); err != nil {
		t.Fatalf("the film reads again once the reader answers: %v", err)
	}
}

// C-FS-7: the watchdog finds a mount that stops answering, and Wait returns
// so that the process can exit and be restarted.
func TestTheWatchdogFindsAFrozenMount(t *testing.T) {
	f := start(t, nil)
	if content, err := os.ReadFile(f.shown(HealthName)); err != nil || string(content) != "ok" {
		t.Fatalf("health: %q, %v", content, err)
	}
	waited := make(chan error, 1)
	go func() { waited <- f.mount.Wait() }()
	select {
	case err := <-waited:
		t.Fatalf("a healthy mount ended: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	f.mount.Freeze()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("a frozen mount should be reported unhealthy")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watchdog did not notice a frozen mount")
	}
}

// C-FS-7: a mount left by a crashed predecessor is detached at start, and
// the new mount takes its place.
func TestStartReplacesADeadMount(t *testing.T) {
	f := start(t, nil)
	second, err := Start(Options{Backing: f.backing, Mountpoint: f.mountpoint, Reader: f.reader,
		AllowedUIDs: []uint32{uint32(os.Getuid())}, CacheTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("a second mount over the first: %v", err)
	}
	defer second.Unmount()
	if _, err := os.ReadFile(f.shown(filmPath)); err != nil {
		t.Fatalf("the new mount should serve: %v", err)
	}
}

// C-FS-7: the kernel aborts a mount whose process stops answering, so a
// caller's read fails instead of hanging. This is the patched go-fuse's
// request timeout (#65) reaching the kernel.
func TestTheKernelAbortsAFrozenMount(t *testing.T) {
	f := start(t, func(options *Options) {
		options.RequestTimeout = time.Second
		options.WatchEvery = time.Hour // keep the watchdog out of it
	})
	if _, err := os.Stat(f.shown(filmPath)); err != nil {
		t.Fatal(err)
	}
	f.mount.Freeze()
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := os.ReadFile(f.shown("Movies/Film (2001) [jmid-1]/movie.nfo"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a frozen mount answered")
		}
		t.Logf("the kernel aborted the frozen mount after %s: %v", time.Since(started).Round(time.Second), err)
	case <-time.After(60 * time.Second):
		f.mount.frozen.Store(false)
		t.Fatal("a read of a frozen mount was still blocked after a minute: no request timeout")
	}
}

// C-FS-9: on a kernel that does not offer request timeouts the mount refuses
// to run, and leaves nothing mounted.
func TestTheMountRefusesAKernelWithoutRequestTimeouts(t *testing.T) {
	requireFUSE(t)
	original := offersRequestTimeout
	offersRequestTimeout = func(*fuse.Server) bool { return false }
	defer func() { offersRequestTimeout = original }()
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "generated"), 0o755)
	mountpoint := filepath.Join(base, "presented")
	m, err := Start(Options{Backing: filepath.Join(base, "generated"), Mountpoint: mountpoint, Reader: &reader{},
		AllowedUIDs: []uint32{uint32(os.Getuid())}})
	defer detach(mountpoint) // whatever happened, leave nothing mounted
	if m != nil {
		m.Unmount()
	}
	if !errors.Is(err, ErrNoRequestTimeout) {
		t.Fatalf("an older kernel should be refused: %v", err)
	}
	mounts, _ := os.ReadFile("/proc/self/mountinfo")
	if strings.Contains(string(mounts), mountpoint) {
		t.Fatal("a refused mount was left mounted")
	}
}
