package filmread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellymesh/internal/relay"
	"jellymesh/internal/store"
)

const reference = "0123456789abcdef0123456789abcdef"

type resolver struct {
	mutex   sync.Mutex
	records map[string]store.Materialized
}

func (r *resolver) ByReference(_ context.Context, ref string) (store.Materialized, bool, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	record, ok := r.records[ref]
	return record, ok, nil
}

func (r *resolver) revoke(ref string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	delete(r.records, ref)
}

type policy struct{ refuse atomic.Bool }

func (p *policy) MayPlay(context.Context, string, string) error {
	if p.refuse.Load() {
		return relay.ErrRefused
	}
	return nil
}

// source serves a film by range, as a source's media route does.
type source struct {
	content []byte
	fetches atomic.Int64
	ranges  sync.Map // "start-end" -> count
	down    atomic.Bool
	refuse  atomic.Bool
	// hold, when set, delays every answer until it is closed.
	hold chan struct{}
}

func (s *source) Media(ctx context.Context, sourceID string, method string, itemID string, header http.Header) (*http.Response, error) {
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.down.Load() {
		return nil, errors.New("source unreachable")
	}
	if s.refuse.Load() {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	s.fetches.Add(1)
	var start, end int64
	if _, err := fmt.Sscanf(header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%d-%d", start, end)
	count, _ := s.ranges.LoadOrStore(key, new(atomic.Int64))
	count.(*atomic.Int64).Add(1)
	if end >= int64(len(s.content)) {
		end = int64(len(s.content)) - 1
	}
	response := &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(s.content[start : end+1]))}
	response.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.content)))
	return response, nil
}

func (s *source) fetchesOf(start, end int64) int64 {
	if count, ok := s.ranges.Load(fmt.Sprintf("%d-%d", start, end)); ok {
		return count.(*atomic.Int64).Load()
	}
	return 0
}

type fixture struct {
	server   *Server
	http     *httptest.Server
	source   *source
	resolver *resolver
	policy   *policy
	content  []byte
	now      atomic.Int64
	healed   chan string
}

func newFixture(t *testing.T, size int, options Options) *fixture {
	t.Helper()
	content := make([]byte, size)
	for index := range content {
		content[index] = byte(index * 7 % 251)
	}
	f := &fixture{source: &source{content: content}, content: content, policy: &policy{}, healed: make(chan string, 8),
		resolver: &resolver{records: map[string]store.Materialized{reference: {SourceNodeID: "cedar", ItemID: "film", LibraryID: "lib", Reference: reference, Path: "Movies/Film/Film - Cedar.mkv.jmfilm"}}}}
	f.now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	options.Resolver, options.Policy, options.Upstream = f.resolver, f.policy, f.source
	options.now = func() time.Time { return time.Unix(0, f.now.Load()) }
	options.Healed = func(_ context.Context, ref string) error { f.healed <- ref; return nil }
	f.server = New(options)
	f.http = httptest.NewServer(f.server.Handler())
	t.Cleanup(f.http.Close)
	return f
}

func (f *fixture) read(t *testing.T, ref string, offset int64, length int) (int, []byte) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/films/%s?offset=%d&length=%d&size=%d", f.http.URL, ref, offset, length, len(f.content))
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, body
}

// C-FS-3: reads return exactly the film's bytes, across chunk boundaries and
// at its end, fetched in whole chunks from the source.
func TestReadsReturnTheFilmsBytes(t *testing.T) {
	f := newFixture(t, 3*ChunkSize+12345, Options{})
	for _, span := range []struct {
		offset int64
		length int
	}{{0, 4096}, {ChunkSize - 100, 200}, {2*ChunkSize + 5, 131072}, {int64(len(f.content)) - 10, 4096}} {
		status, body := f.read(t, reference, span.offset, span.length)
		want := f.content[span.offset:min(span.offset+int64(span.length), int64(len(f.content)))]
		if status != http.StatusOK || !bytes.Equal(body, want) {
			t.Fatalf("read at %d: status %d, %d bytes, want %d", span.offset, status, len(body), len(want))
		}
	}
	f.source.ranges.Range(func(key, _ any) bool {
		var start, end int64
		fmt.Sscanf(key.(string), "%d-%d", &start, &end)
		if start%ChunkSize != 0 || (end-start+1 != ChunkSize && end != int64(len(f.content))-1) {
			t.Errorf("a fetch of %s is not a whole chunk", key)
		}
		return true
	})
	if status, _ := f.read(t, reference, int64(len(f.content)), 10); status != http.StatusBadRequest {
		t.Fatalf("a read past the end is refused: %d", status)
	}
}

// C-FS-3: one isolated read, a probe's, fetches only its chunk; sequential
// reading starts read-ahead, which doubles up to MaxAhead chunks.
func TestReadAheadStartsOnlyWhenReadingIsSequential(t *testing.T) {
	f := newFixture(t, 40*ChunkSize, Options{})
	f.read(t, reference, 20*ChunkSize, 65536)
	time.Sleep(50 * time.Millisecond)
	if fetches := f.source.fetches.Load(); fetches != 1 {
		t.Fatalf("an isolated read fetched %d chunks", fetches)
	}
	for index := int64(0); index < 6; index++ {
		f.read(t, reference, index*ChunkSize, 65536)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.source.fetchesOf(21*ChunkSize, 22*ChunkSize-1) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	// After six sequential chunks, read-ahead reaches 16 beyond chunk 5.
	if f.source.fetchesOf(21*ChunkSize, 22*ChunkSize-1) != 1 {
		t.Fatal("sequential reading should read ahead up to sixteen chunks")
	}
	if f.source.fetchesOf(22*ChunkSize, 23*ChunkSize-1) != 0 {
		t.Fatal("read-ahead must stop at sixteen chunks")
	}
	if f.source.fetchesOf(20*ChunkSize, 21*ChunkSize-1) != 1 {
		t.Fatal("a chunk already held must not be fetched again")
	}
}

// C-FS-4: every read asks the destination's policy and resolves the
// reference, so a revoked reference or a refused source stops reads at
// once, even of chunks already held.
func TestReadsStopAtOnceWhenRefusedOrRevoked(t *testing.T) {
	f := newFixture(t, 2*ChunkSize, Options{})
	if status, _ := f.read(t, reference, 0, 1000); status != http.StatusOK {
		t.Fatalf("first read: %d", status)
	}
	f.policy.refuse.Store(true)
	if status, _ := f.read(t, reference, 0, 1000); status != http.StatusNotFound {
		t.Fatalf("a refused source's held chunk was served: %d", status)
	}
	f.policy.refuse.Store(false)
	f.resolver.revoke(reference)
	if status, _ := f.read(t, reference, 0, 1000); status != http.StatusNotFound {
		t.Fatalf("a revoked reference was served: %d", status)
	}
	if status, _ := f.read(t, "not-a-reference", 0, 1000); status != http.StatusNotFound {
		t.Fatalf("a malformed reference: %d", status)
	}
}

// C-FS-4: a held chunk is served only for ChunkLife, so a refusal at the
// source takes effect within it.
func TestAHeldChunkIsServedOnlyForItsLife(t *testing.T) {
	f := newFixture(t, ChunkSize, Options{})
	f.read(t, reference, 0, 1000)
	f.read(t, reference, 0, 1000)
	if fetches := f.source.fetches.Load(); fetches != 1 {
		t.Fatalf("a fresh chunk should be served from the cache: %d fetches", fetches)
	}
	f.now.Add(int64(ChunkLife + time.Second))
	f.source.refuse.Store(true)
	if status, _ := f.read(t, reference, 0, 1000); status != http.StatusBadGateway {
		t.Fatalf("an expired chunk must be fetched again, and the source's refusal seen: %d", status)
	}
}

// C-FS-3: a source whose file is not the size the catalog says is an error,
// never short or misplaced bytes.
func TestAFileOfAnotherSizeIsAnError(t *testing.T) {
	f := newFixture(t, ChunkSize+500, Options{})
	url := fmt.Sprintf("%s/v1/films/%s?offset=0&length=1000&size=%d", f.http.URL, reference, len(f.content)+1)
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("a size mismatch should fail: %d", response.StatusCode)
	}
}

// C-FS-5: a film whose read failed is retried, and once readable is marked
// changed; the list of such films survives a restart.
func TestAFailedFilmIsHealedOnceReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.json")
	f := newFixture(t, ChunkSize, Options{FailedPath: path})
	f.source.down.Store(true)
	if status, _ := f.read(t, reference, 0, 1000); status != http.StatusBadGateway {
		t.Fatalf("an unreachable source: %d", status)
	}
	if f.server.Stats().Failed != 1 {
		t.Fatal("the failed film should be recorded")
	}
	f.server.HealOnce(context.Background())
	select {
	case <-f.healed:
		t.Fatal("a film still unreadable must not be marked changed")
	default:
	}

	// A restarted service still knows it.
	restarted := New(Options{Resolver: f.resolver, Policy: f.policy, Upstream: f.source, FailedPath: path,
		Healed: func(_ context.Context, ref string) error { f.healed <- ref; return nil }})
	if restarted.Stats().Failed != 1 {
		t.Fatal("the failed films should survive a restart")
	}
	f.source.down.Store(false)
	restarted.HealOnce(context.Background())
	select {
	case ref := <-f.healed:
		if ref != reference {
			t.Fatalf("healed %s", ref)
		}
	default:
		t.Fatal("a readable film should be marked changed")
	}
	if restarted.Stats().Failed != 0 {
		t.Fatal("a healed film is forgotten")
	}

	// A film the mount reports, whose reference is then revoked, is
	// forgotten rather than retried forever.
	response, _ := http.Post(f.http.URL+"/v1/films/"+reference+"/failed", "", nil)
	response.Body.Close()
	f.resolver.revoke(reference)
	f.server.HealOnce(context.Background())
	if f.server.Stats().Failed != 0 {
		t.Fatal("a revoked film should be forgotten")
	}
}

// C-FS-3: a read the mount abandons, at its deadline, does not hold the
// service: the request ends, and the chunk is still fetched once for the
// next reader.
func TestAnAbandonedReadEndsWithItsRequest(t *testing.T) {
	f := newFixture(t, ChunkSize, Options{})
	f.source.hold = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		f.http.URL+"/v1/films/"+reference+"?offset=0&length=10&size="+strconv.Itoa(len(f.content)), nil)
	started := time.Now()
	if _, err := http.DefaultClient.Do(request); err == nil {
		t.Fatal("the read should have been abandoned")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("abandoning took %s", elapsed)
	}
	close(f.source.hold)
	if status, body := f.read(t, reference, 0, 10); status != http.StatusOK || !bytes.Equal(body, f.content[:10]) {
		t.Fatalf("a later read: %d", status)
	}
}

// pacedFixture records pacing waits on a fake clock instead of sleeping.
func pacedFixture(t *testing.T, size int) (*fixture, *time.Duration) {
	t.Helper()
	waited := new(time.Duration)
	f := newFixture(t, size, Options{})
	f.server.options.sleep = func(_ context.Context, wait time.Duration) error {
		*waited += wait
		f.now.Add(int64(wait))
		return nil
	}
	return f, waited
}

func (f *fixture) readPaced(t *testing.T, offset int64, length int, bitrate int64) int {
	t.Helper()
	url := fmt.Sprintf("%s/v1/films/%s?offset=%d&length=%d&size=%d&bitrate=%d", f.http.URL, reference, offset, length, len(f.content), bitrate)
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}

// C-FS-10: a film read straight through, as extraction does, runs at full
// speed for the burst and then at PaceMultiple times its bitrate.
func TestAWholeFilmReadIsPacedPastTheBurst(t *testing.T) {
	const bitrate = 8 << 20 // 8 Mbit/s: 1 MiB/s, so 4 MiB/s paced and a 600 MiB burst
	size := 640 << 20
	f, waited := pacedFixture(t, size)
	for offset := int64(0); offset < int64(size); offset += 4 * ChunkSize {
		if status := f.readPaced(t, offset, 4*ChunkSize, bitrate); status != http.StatusOK {
			t.Fatalf("read at %d: %d", offset, status)
		}
		if offset < 590<<20 && *waited > 0 {
			t.Fatalf("a read inside the burst waited (at %d MiB)", offset>>20)
		}
	}
	// 40 MiB past the burst at 4 MiB/s, less what refilled meanwhile.
	if *waited < 9*time.Second || *waited > 10*time.Second {
		t.Fatalf("40 MiB past the burst waited %s, want about 10 s", *waited)
	}
	if f.server.Stats().Paced != *waited {
		t.Fatalf("paced time is counted: %s", f.server.Stats().Paced)
	}
}

// C-FS-10: playback, reading at the film's own pace, and a film without a
// known bitrate are never slowed; a low bitrate is paced no slower than
// PaceFloor, so one read never waits near the mount's deadline.
func TestPlaybackIsNeverPaced(t *testing.T) {
	const bitrate = 2_000_000
	size := 400 << 20
	f, waited := pacedFixture(t, size)
	perSecond := int64(bitrate / 8)
	// Two hours' worth at real time, with a seek back and a seek ahead.
	offset := int64(0)
	for second := 0; second < 1600; second++ {
		f.readPaced(t, offset%int64(size-int(perSecond)), int(perSecond), bitrate)
		offset += perSecond
		if second == 600 {
			offset -= 200 << 20 / 4
		}
		f.now.Add(int64(time.Second))
	}
	if *waited != 0 {
		t.Fatalf("playback waited %s", *waited)
	}

	unknown, unknownWaited := pacedFixture(t, 300<<20)
	for offset := int64(0); offset < 300<<20; offset += 4 * ChunkSize {
		unknown.readPaced(t, offset, 4*ChunkSize, 0)
	}
	if *unknownWaited != 0 {
		t.Fatalf("a film without a bitrate was paced: %s", *unknownWaited)
	}

	slow, slowWaited := pacedFixture(t, 80<<20)
	worst := time.Duration(0)
	for offset := int64(0); offset < 80<<20; offset += ChunkSize {
		before := *slowWaited
		slow.readPaced(t, offset, ChunkSize, 64_000) // 64 kbit/s
		worst = max(worst, *slowWaited-before)
	}
	if *slowWaited == 0 || worst > time.Second+time.Millisecond {
		t.Fatalf("a low bitrate past its burst: waited %s in all, %s at worst", *slowWaited, worst)
	}
}

// C-FS-5: when the mount restarts, films read shortly before are retried and
// marked changed, since a probe the dying mount cut off was never reported;
// films read long before are not.
func TestFilmsReadBeforeAMountRestartAreRetried(t *testing.T) {
	f := newFixture(t, ChunkSize, Options{})
	other := "fedcba9876543210fedcba9876543210"
	f.resolver.records[other] = store.Materialized{SourceNodeID: "cedar", ItemID: "film", LibraryID: "lib", Reference: other}
	report := func(started string) {
		response, err := http.Post(f.http.URL+"/v1/mount", "application/json", strings.NewReader(`{"started":"`+started+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	report("first")
	f.read(t, other, 0, 100)
	f.now.Add(int64(10 * time.Minute))
	f.read(t, reference, 0, 100)
	report("first") // the same mount: nothing happens
	if f.server.Stats().Failed != 0 {
		t.Fatal("a report from the same mount marked films")
	}
	f.now.Add(int64(20 * time.Second))
	report("second")
	if f.server.Stats().Failed != 1 {
		t.Fatalf("only the recently read film should be retried: %d", f.server.Stats().Failed)
	}
	f.server.HealOnce(context.Background())
	select {
	case got := <-f.healed:
		if got != reference {
			t.Fatalf("healed %s", got)
		}
	default:
		t.Fatal("the recently read film should be marked changed")
	}
}
