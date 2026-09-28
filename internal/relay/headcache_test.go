package relay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jellymesh/internal/store"
)

// origin serves a file with ranges, as a source does, and records the ranges
// it was asked for.
type origin struct {
	content []byte
	mutex   sync.Mutex
	ranges  []string
	server  *httptest.Server
}

func newOrigin(t *testing.T, content []byte) *origin {
	t.Helper()
	o := &origin{content: content}
	o.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		o.mutex.Lock()
		o.ranges = append(o.ranges, request.Method+" "+request.Header.Get("Range"))
		o.mutex.Unlock()
		response.Header().Set("Content-Type", "video/x-matroska")
		http.ServeContent(response, request, "", time.Time{}, bytes.NewReader(o.content))
	}))
	t.Cleanup(o.server.Close)
	return o
}

func (o *origin) requests() []string {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	return append([]string(nil), o.ranges...)
}

// transfers returns the requests that could move media bytes: everything but
// HEAD, which the relay sends to confirm a cache hit with the source.
func (o *origin) transfers() []string {
	var moving []string
	for _, request := range o.requests() {
		if !strings.HasPrefix(request, http.MethodHead+" ") {
			moving = append(moving, request)
		}
	}
	return moving
}

func (o *origin) Media(ctx context.Context, _ string, method string, _ string, header http.Header) (*http.Response, error) {
	request, _ := http.NewRequestWithContext(ctx, method, o.server.URL, nil)
	request.Header = header.Clone()
	return http.DefaultClient.Do(request)
}

type cachedRelay struct {
	origin *origin
	cache  *HeadCache
	server *httptest.Server
	refs   references
}

func newCachedRelay(t *testing.T, content []byte, perItem int) *cachedRelay {
	t.Helper()
	o := newOrigin(t, content)
	cache, err := NewHeadCache(t.TempDir(), perItem, 64<<20)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	allowed, _ := ParseAllowed("127.0.0.0/8,::1")
	refs := references{reference: {SourceNodeID: "cedar", ItemID: "movie-1", LibraryID: "lib", Reference: reference, Revision: 1}}
	server := New(allowed, refs, &policy{}, o, nil)
	server.SetHeadCache(cache)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &cachedRelay{origin: o, cache: cache, server: httpServer, refs: refs}
}

// read requests rangeHeader and reads at most limit bytes, as a probe does
// before it disconnects.
func (r *cachedRelay) read(t *testing.T, method string, rangeHeader string, limit int) (*http.Response, []byte) {
	t.Helper()
	request, _ := http.NewRequest(method, r.server.URL+Path+reference, nil)
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, int64(limit)))
	return response, body
}

func content(size int) []byte {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index * 7)
	}
	return data
}

// waitFor polls until condition holds; the cache is written after the
// response completes, which a client that stopped reading does not wait for.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// C-PB-5: the first probe fills the head; the next probes move no media
// bytes from the source, only a HEAD each to confirm the item is still theirs.
func TestRepeatedProbesAreServedFromTheHead(t *testing.T) {
	file := content(3 << 20)
	r := newCachedRelay(t, file, 2<<20)
	response, body := r.read(t, http.MethodGet, "bytes=0-", 1<<20)
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, file[:1<<20]) {
		t.Fatalf("first probe: status %d, %d bytes", response.StatusCode, len(body))
	}
	waitFor(t, func() bool { _, ok := r.cache.Get(r.refs[reference]); return ok })
	before := len(r.origin.transfers())
	heads := len(r.origin.requests()) - before

	for probe := 0; probe < 3; probe++ {
		response, body := r.read(t, http.MethodGet, "bytes=0-", 512<<10)
		if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, file[:512<<10]) {
			t.Fatalf("probe %d: status %d, %d bytes", probe, response.StatusCode, len(body))
		}
		if response.Header.Get("Content-Range") != "bytes 0-3145727/3145728" {
			t.Fatalf("probe %d: Content-Range %q", probe, response.Header.Get("Content-Range"))
		}
	}
	if after := len(r.origin.transfers()); after != before {
		t.Fatalf("repeated probes moved bytes from the source: %v", r.origin.transfers()[before:])
	}
	if confirmations := len(r.origin.requests()) - len(r.origin.transfers()) - heads; confirmations != 3 {
		t.Fatalf("each cached probe should be confirmed with the source once, got %d", confirmations)
	}
}

// C-PB-5: a closed range inside the head, and HEAD, are answered locally.
func TestRangesInsideTheHeadAndHeadRequestsStayLocal(t *testing.T) {
	file := content(1 << 20)
	r := newCachedRelay(t, file, 4<<20)
	r.read(t, http.MethodGet, "", len(file)) // the whole file fills the head
	waitFor(t, func() bool { head, ok := r.cache.Get(r.refs[reference]); return ok && len(head.Data) == len(file) })
	before := len(r.origin.transfers())

	response, body := r.read(t, http.MethodGet, "bytes=1000-1999", 1<<20)
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, file[1000:2000]) || response.Header.Get("Content-Length") != "1000" {
		t.Fatalf("closed range: status %d, %d bytes, length %q", response.StatusCode, len(body), response.Header.Get("Content-Length"))
	}
	head, headBody := r.read(t, http.MethodHead, "", 1)
	if head.StatusCode != http.StatusOK || len(headBody) != 0 || head.ContentLength != int64(len(file)) || head.Header.Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("head: status %d, length %d", head.StatusCode, head.ContentLength)
	}
	whole, wholeBody := r.read(t, http.MethodGet, "", len(file)+1)
	if whole.StatusCode != http.StatusOK || !bytes.Equal(wholeBody, file) {
		t.Fatal("a whole-file request should be answered from a head holding the whole file")
	}
	if after := len(r.origin.transfers()); after != before {
		t.Fatalf("local requests moved bytes from the source: %v", r.origin.transfers()[before:])
	}
}

// C-PB-5, C-PB-1: a range that runs past the head is stitched: the head, then
// exactly one source request starting where the head ends, and the bytes are
// exact across the join.
func TestARangePastTheHeadIsStitchedExactly(t *testing.T) {
	file := content(3 << 20)
	r := newCachedRelay(t, file, 1<<20)
	r.read(t, http.MethodGet, "bytes=0-", 1<<20)
	waitFor(t, func() bool { head, ok := r.cache.Get(r.refs[reference]); return ok && len(head.Data) == 1<<20 })
	before := len(r.origin.transfers())

	start, end := (1<<20)-1000, (1<<20)+5000
	response, body := r.read(t, http.MethodGet, "bytes="+itoa(start)+"-"+itoa(end), 1<<22)
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, file[start:end+1]) {
		t.Fatalf("stitched range: status %d, %d bytes, exact=%v", response.StatusCode, len(body), bytes.Equal(body, file[start:end+1]))
	}
	requests := r.origin.transfers()[before:]
	if len(requests) != 1 || requests[0] != "GET bytes="+itoa(1<<20)+"-"+itoa(end) {
		t.Fatalf("source requests = %v", requests)
	}
	whole, wholeBody := r.read(t, http.MethodGet, "", len(file)+1)
	if whole.StatusCode != http.StatusOK || !bytes.Equal(wholeBody, file) || whole.ContentLength != int64(len(file)) {
		t.Fatal("a whole file stitched from the head and the source must be exact")
	}
}

// A new revision is a different file as far as the cache is concerned, and a
// source whose size no longer matches the head drops it rather than splicing.
func TestTheHeadIsTiedToOneRevisionOfOneFile(t *testing.T) {
	file := content(2 << 20)
	r := newCachedRelay(t, file, 1<<20)
	r.read(t, http.MethodGet, "bytes=0-", 1<<20)
	waitFor(t, func() bool { _, ok := r.cache.Get(r.refs[reference]); return ok })

	next := r.refs[reference]
	next.Revision = 2
	if _, ok := r.cache.Get(next); ok {
		t.Fatal("a new revision must not find the old revision's head")
	}

	// The file changes at the source under the same revision. The HEAD that
	// confirms a cache hit reports the new size, so the old head is dropped
	// and the request is served from the source: the new bytes, exactly.
	changed := content(5 << 20)
	for index := range changed {
		changed[index] ^= 0xff
	}
	r.origin.content = changed
	response, body := r.read(t, http.MethodGet, "bytes=0-", 3<<20)
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, changed[:3<<20]) {
		t.Fatalf("after the file changed: status %d, exact=%v", response.StatusCode, bytes.Equal(body, changed[:len(body)]))
	}
	waitFor(t, func() bool {
		head, ok := r.cache.Get(r.refs[reference])
		return ok && head.Total == int64(len(changed))
	})
	if head, ok := r.cache.Get(r.refs[reference]); !ok || head.Total != int64(len(changed)) || !bytes.Equal(head.Data, changed[:len(head.Data)]) {
		t.Fatal("the head should be refilled from the changed file")
	}
}

func TestTheCacheIsBoundedAndRemovable(t *testing.T) {
	cache, err := NewHeadCache(t.TempDir(), 1<<20, 3<<20)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	for index := 0; index < 6; index++ {
		record := store.Materialized{SourceNodeID: "cedar", ItemID: itoa(index), Revision: 1}
		if err := cache.Put(record, Head{Data: content(1 << 20), Total: 5 << 20, ContentType: "video/x-matroska"}); err != nil {
			t.Fatalf("put: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if size := cache.Size(); size > 3<<20+1024 {
		t.Fatalf("the cache holds %d bytes, over its capacity", size)
	}
	newest := store.Materialized{SourceNodeID: "cedar", ItemID: "5", Revision: 1}
	if _, ok := cache.Get(newest); !ok {
		t.Fatal("the most recent head should survive eviction")
	}
	if _, ok := cache.Get(store.Materialized{SourceNodeID: "cedar", ItemID: "0", Revision: 1}); ok {
		t.Fatal("the oldest head should be evicted")
	}
	cache.Remove(newest)
	if _, ok := cache.Get(newest); ok {
		t.Fatal("a removed item's head must be gone")
	}
}

func TestParseRange(t *testing.T) {
	cases := map[string]byteRange{"": {0, -1, false}, "bytes=0-": {0, -1, true}, "bytes=10-20": {10, 20, true}}
	for header, want := range cases {
		if got, ok := parseRange(header); !ok || got != want {
			t.Errorf("parseRange(%q) = %+v, %v", header, got, ok)
		}
	}
	for _, header := range []string{"bytes=-500", "bytes=0-10,20-30", "items=0-1", "bytes=20-10", "bytes=x-"} {
		if _, ok := parseRange(header); ok {
			t.Errorf("parseRange(%q) should not be cacheable", header)
		}
	}
}

func itoa(value int) string { return strconv.Itoa(value) }

// refusing wraps an origin and refuses everything once told to, as a source
// that has blocked this node or withdrawn the item does.
type refusing struct {
	*origin
	refuse bool
}

func (r *refusing) Media(ctx context.Context, source string, method string, item string, header http.Header) (*http.Response, error) {
	if r.refuse {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return r.origin.Media(ctx, source, method, item, header)
}

// The cache never decides access: once the source refuses the item, even
// bytes held in the head are no longer served.
func TestACachedHeadIsNotServedOnceTheSourceRefuses(t *testing.T) {
	file := content(2 << 20)
	o := newOrigin(t, file)
	upstream := &refusing{origin: o}
	cache, _ := NewHeadCache(t.TempDir(), 1<<20, 64<<20)
	allowed, _ := ParseAllowed("127.0.0.0/8")
	refs := references{reference: {SourceNodeID: "cedar", ItemID: "movie-1", LibraryID: "lib", Reference: reference, Revision: 1}}
	server := New(allowed, refs, &policy{}, upstream, nil)
	server.SetHeadCache(cache)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	r := &cachedRelay{origin: o, cache: cache, server: httpServer, refs: refs}

	r.read(t, http.MethodGet, "bytes=0-", 1<<20)
	waitFor(t, func() bool { _, ok := cache.Get(refs[reference]); return ok })
	if response, _ := r.read(t, http.MethodGet, "bytes=0-99", 100); response.StatusCode != http.StatusPartialContent {
		t.Fatalf("a cached range while allowed: status %d", response.StatusCode)
	}
	upstream.refuse = true
	if response, body := r.read(t, http.MethodGet, "bytes=0-99", 100); response.StatusCode != http.StatusNotFound || len(body) > 20 {
		t.Fatalf("a cached range after the source refuses: status %d, %d bytes", response.StatusCode, len(body))
	}
	if response, _ := r.read(t, http.MethodHead, "", 1); response.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD after the source refuses: status %d", response.StatusCode)
	}
}
