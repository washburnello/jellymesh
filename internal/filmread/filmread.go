// Package filmread is the daemon's read service for the mount (design-spec
// section 11, A-17). The mount asks it for a range of a film by reference;
// it answers from 1 MiB chunks fetched from the source through the same
// media route the relay uses, so the source authorizes every fetch, the
// per-destination ceiling applies, and direct paths are used.
//
// Chunks are kept in a bounded cache, and read-ahead starts only once a film
// is being read sequentially, doubling up to 16 chunks. So a probe's
// scattered reads cost about what they read, and playback is not paced by
// one round trip per read.
//
// The service never decides more than the relay does. Every read resolves
// the reference and asks the destination's policy, so a revoked reference or
// a blocked source stops reads at once. A cached chunk is served only for
// ChunkLife after it was fetched, which bounds how long a refusal at the
// source can go unnoticed (C-FS-4).
package filmread

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jellymesh/internal/relay"
	"jellymesh/internal/store"
)

const (
	// ChunkSize is the unit fetched from a source.
	ChunkSize = 1 << 20
	// MaxAhead bounds read-ahead, in chunks.
	MaxAhead = 16
	// ChunkLife is how long a fetched chunk may be served.
	ChunkLife = time.Minute
	// FetchTimeout bounds one chunk fetch from a source.
	FetchTimeout = 30 * time.Second
	// HealInterval is how often films whose read failed are retried.
	HealInterval = 10 * time.Second

	// PaceMultiple, PaceBurst, and PaceFloor guard against a whole film
	// being pulled at once, as trickplay or chapter image extraction would
	// do (A-17, #69). Each film may be read at full speed for PaceBurst of
	// play at its average bitrate (at least MinBurstBytes); past that, reads
	// are paced to PaceMultiple times its bitrate, never slower than
	// PaceFloor, so one read never waits near the mount's deadline.
	// Playback and transcoding need no more than real time, so they are
	// never slowed; an extraction costs about what a few viewers would.
	PaceMultiple  = 4
	PaceBurst     = 10 * time.Minute
	MinBurstBytes = 64 << 20
	PaceFloor     = 1 << 20 // bytes a second
)

var referencePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Options configure a Server.
type Options struct {
	Resolver relay.Resolver
	Policy   relay.Policy
	Upstream relay.Upstream
	// CacheBytes bounds the chunk cache (default 256 MiB).
	CacheBytes int64
	// FailedPath, if set, keeps the films awaiting healing across restarts.
	FailedPath string
	// Healed is called when a film whose read failed is readable again, so
	// that its modification time can move on and Jellyfin probe it again.
	Healed func(ctx context.Context, reference string) error
	Logger *log.Logger
	// now is the clock, and sleep waits, which tests replace.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// Stats count what the service has done.
type Stats struct {
	Fetched, Served, FetchErrors int64
	Failed                       int
	// Paced is how long reads have waited for pacing in all.
	Paced time.Duration
}

// Server is the read service.
type Server struct {
	options Options
	cache   *cache

	streakMutex sync.Mutex
	streaks     map[string]*streak

	failedMutex sync.Mutex
	failed      map[string]time.Time
	saveMutex   sync.Mutex

	fetched, served, fetchErrors atomic.Int64

	reportMutex sync.Mutex
	report      json.RawMessage
	reported    time.Time
	mountStart  string

	readMutex sync.Mutex
	lastRead  map[string]time.Time

	paceMutex sync.Mutex
	paces     map[string]*bucket
	paced     atomic.Int64 // nanoseconds reads have waited
}

// New returns a read service.
func New(options Options) *Server {
	if options.CacheBytes <= 0 {
		options.CacheBytes = 256 << 20
	}
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.sleep == nil {
		options.sleep = sleep
	}
	server := &Server{options: options, streaks: map[string]*streak{}, failed: map[string]time.Time{}, paces: map[string]*bucket{}, lastRead: map[string]time.Time{}}
	server.cache = &cache{chunks: map[chunkKey]*chunk{}, order: list.New(), capacity: int(max(options.CacheBytes/ChunkSize, MaxAhead+2)), fetch: server.fetch, now: options.now}
	server.loadFailed()
	return server
}

// Handler serves the mount. It is served on a local socket only.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/films/{reference}", server.read)
	mux.HandleFunc("POST /v1/films/{reference}/failed", server.reportFailed)
	mux.HandleFunc("POST /v1/mount", server.mountReport)
	return mux
}

// Stats returns the counters.
func (server *Server) Stats() Stats {
	server.failedMutex.Lock()
	failed := len(server.failed)
	server.failedMutex.Unlock()
	return Stats{Fetched: server.fetched.Load(), Served: server.served.Load(), FetchErrors: server.fetchErrors.Load(), Failed: failed,
		Paced: time.Duration(server.paced.Load())}
}

// mountReport keeps the mount's latest report, for status.
func (server *Server) mountReport(response http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 64<<10))
	if err != nil || !json.Valid(body) {
		http.Error(response, "a report is a JSON object", http.StatusBadRequest)
		return
	}
	var started struct {
		Started string `json:"started"`
	}
	json.Unmarshal(body, &started)
	server.reportMutex.Lock()
	restarted := server.mountStart != "" && started.Started != "" && started.Started != server.mountStart
	server.report, server.reported = body, server.options.now()
	if started.Started != "" {
		server.mountStart = started.Started
	}
	server.reportMutex.Unlock()
	if restarted {
		server.afterMountRestart()
	}
	response.WriteHeader(http.StatusNoContent)
}

// RestartWindow is how far before a mount restart a film's reads may have
// been cut off by it.
const RestartWindow = 2 * time.Minute

// afterMountRestart marks every film read shortly before the mount
// restarted as failed. A read the dying mount had in flight failed in the
// kernel, never reached this service, and could not be reported, so a probe
// cut off that way would otherwise never be retried (lab G2-C).
func (server *Server) afterMountRestart() {
	cutoff := server.options.now().Add(-RestartWindow - ReportGrace)
	server.readMutex.Lock()
	var recent []string
	for reference, at := range server.lastRead {
		if at.After(cutoff) {
			recent = append(recent, reference)
		}
	}
	server.readMutex.Unlock()
	for _, reference := range recent {
		server.markFailed(reference)
	}
	server.options.Logger.Printf("filmread: the mount restarted; %d recently read films will be checked", len(recent))
}

// ReportGrace allows for the interval between a restarted mount's start
// and its first report.
const ReportGrace = 30 * time.Second

// MountReport returns the mount's latest report and when it came, or a nil
// report if none has.
func (server *Server) MountReport() (json.RawMessage, time.Time) {
	server.reportMutex.Lock()
	defer server.reportMutex.Unlock()
	return server.report, server.reported
}

// Forget drops whatever is held for a reference, when its item is removed.
func (server *Server) Forget(reference string) {
	server.cache.forget(reference)
	server.streakMutex.Lock()
	delete(server.streaks, reference)
	server.streakMutex.Unlock()
	server.paceMutex.Lock()
	delete(server.paces, reference)
	server.paceMutex.Unlock()
	server.failedMutex.Lock()
	_, was := server.failed[reference]
	delete(server.failed, reference)
	server.failedMutex.Unlock()
	if was {
		server.saveFailed()
	}
}

// target is what a read is of: the item, and the size the mount shows.
type target struct {
	record store.Materialized
	size   int64
}

func (server *Server) authorize(ctx context.Context, reference string) (store.Materialized, int, error) {
	if !referencePattern.MatchString(reference) {
		return store.Materialized{}, http.StatusNotFound, errors.New("not a reference")
	}
	record, found, err := server.options.Resolver.ByReference(ctx, reference)
	if err != nil {
		return store.Materialized{}, http.StatusServiceUnavailable, err
	}
	if !found {
		// Never issued, or revoked when the item was withdrawn.
		return store.Materialized{}, http.StatusNotFound, errors.New("unknown reference")
	}
	if err := server.options.Policy.MayPlay(ctx, record.SourceNodeID, record.LibraryID); err != nil {
		return store.Materialized{}, http.StatusNotFound, err
	}
	return record, http.StatusOK, nil
}

func (server *Server) read(response http.ResponseWriter, request *http.Request) {
	reference := request.PathValue("reference")
	query := request.URL.Query()
	offset, err1 := strconv.ParseInt(query.Get("offset"), 10, 64)
	length, err2 := strconv.ParseInt(query.Get("length"), 10, 64)
	size, err3 := strconv.ParseInt(query.Get("size"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || offset < 0 || length <= 0 || length > 4*ChunkSize || size <= 0 || offset >= size {
		http.Error(response, "a read needs an offset inside the film, a length, and the film's size", http.StatusBadRequest)
		return
	}
	record, status, err := server.authorize(request.Context(), reference)
	if err != nil {
		http.Error(response, err.Error(), status)
		return
	}
	if offset+length > size {
		length = size - offset
	}
	if bitrate, err := strconv.ParseInt(query.Get("bitrate"), 10, 64); err == nil && bitrate > 0 {
		if wait := server.pace(reference, bitrate, length); wait > 0 {
			server.paced.Add(int64(wait))
			if server.options.sleep(request.Context(), wait) != nil {
				return // the mount gave up
			}
		}
	}
	server.readMutex.Lock()
	server.lastRead[reference] = server.options.now()
	if len(server.lastRead) > 4096 {
		for held, at := range server.lastRead {
			if server.options.now().Sub(at) > RestartWindow+ReportGrace {
				delete(server.lastRead, held)
			}
		}
	}
	server.readMutex.Unlock()
	item := target{record: record, size: size}
	out := make([]byte, 0, length)
	for position := offset; position < offset+length; {
		index := position / ChunkSize
		current := server.cache.get(item, index)
		for ahead := int64(1); ahead <= server.readAhead(reference, index); ahead++ {
			if (index+ahead)*ChunkSize < size {
				server.cache.get(item, index+ahead)
			}
		}
		select {
		case <-current.ready:
		case <-request.Context().Done():
			return // the mount gave up; its deadline has passed
		}
		if current.err != nil {
			server.markFailed(reference)
			http.Error(response, "the source of this film did not answer: "+current.err.Error(), http.StatusBadGateway)
			return
		}
		within := position - index*ChunkSize
		take := int64(len(current.data)) - within
		if remaining := offset + length - position; take > remaining {
			take = remaining
		}
		if take <= 0 {
			http.Error(response, "the source's file is shorter than the catalog says", http.StatusBadGateway)
			return
		}
		out = append(out, current.data[within:within+take]...)
		position += take
	}
	server.served.Add(int64(len(out)))
	response.Header().Set("Content-Length", strconv.Itoa(len(out)))
	response.Write(out)
}

// fetch reads one chunk from the source. A short or misplaced answer is an
// error: the file at the source is not the one the catalog describes, and
// the catalog's next update brings the new size.
func (server *Server) fetch(item target, index int64) ([]byte, error) {
	start := index * ChunkSize
	end := min(start+ChunkSize, item.size) - 1
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()
	header := http.Header{}
	header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	response, err := server.options.Upstream.Media(ctx, item.record.SourceNodeID, http.MethodGet, item.record.ItemID, header)
	if err != nil {
		server.fetchErrors.Add(1)
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		server.fetchErrors.Add(1)
		return nil, fmt.Errorf("the source answered %d", response.StatusCode)
	}
	if want := fmt.Sprintf("bytes %d-%d/%d", start, end, item.size); response.Header.Get("Content-Range") != want {
		server.fetchErrors.Add(1)
		return nil, fmt.Errorf("the source sent %q, not %q", response.Header.Get("Content-Range"), want)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, ChunkSize+1))
	if err != nil || int64(len(data)) != end-start+1 {
		server.fetchErrors.Add(1)
		return nil, fmt.Errorf("a short chunk: %d bytes, %v", len(data), err)
	}
	server.fetched.Add(int64(len(data)))
	return data, nil
}

// bucket paces one film: tokens are bytes that may be read now, refilled at
// rate up to burst, and may go negative, which is the wait.
type bucket struct {
	tokens, rate, burst float64
	last                time.Time
}

// pace takes length bytes from a film's bucket and returns how long the
// read must wait.
func (server *Server) pace(reference string, bitrate int64, length int64) time.Duration {
	now := server.options.now()
	rate := max(float64(bitrate)/8*PaceMultiple, PaceFloor)
	burst := max(float64(bitrate)/8*PaceBurst.Seconds(), MinBurstBytes)
	server.paceMutex.Lock()
	defer server.paceMutex.Unlock()
	current := server.paces[reference]
	if current == nil {
		if len(server.paces) >= 4096 {
			server.prunePaces(now)
		}
		current = &bucket{tokens: burst, last: now}
		server.paces[reference] = current
	}
	current.rate, current.burst = rate, burst
	current.tokens = min(current.burst, current.tokens+now.Sub(current.last).Seconds()*current.rate)
	current.last = now
	current.tokens -= float64(length)
	if current.tokens >= 0 {
		return 0
	}
	return time.Duration(-current.tokens / current.rate * float64(time.Second))
}

// prunePaces forgets films whose buckets have refilled, which are the same
// as new ones.
func (server *Server) prunePaces(now time.Time) {
	for reference, held := range server.paces {
		if held.tokens+now.Sub(held.last).Seconds()*held.rate >= held.burst {
			delete(server.paces, reference)
		}
	}
}

func sleep(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// streak tracks sequential reading of one film, so that read-ahead starts
// only once it is being played, not for a probe's scattered reads.
type streak struct {
	last  int64
	count int
}

// readAhead reports how many chunks to fetch beyond index: none for an
// isolated read, then doubling with each further sequential chunk.
func (server *Server) readAhead(reference string, index int64) int64 {
	server.streakMutex.Lock()
	defer server.streakMutex.Unlock()
	current := server.streaks[reference]
	if current == nil {
		current = &streak{last: -2}
		server.streaks[reference] = current
	}
	switch {
	case index == current.last:
	case index == current.last+1:
		current.count++
	default:
		current.count = 0
	}
	current.last = index
	if current.count == 0 {
		return 0
	}
	return min(int64(1)<<min(current.count, 4), MaxAhead)
}

// chunkKey names a chunk by everything it depends on. A reference can be
// reused for another item at the same path (C-MA-8), so the item is part of
// the key, and a file of another size is another file.
type chunkKey struct {
	reference, source, item string
	size, index             int64
}

type chunk struct {
	ready   chan struct{}
	data    []byte
	err     error
	fetched time.Time
	element *list.Element
}

// cache holds chunks, least recently used first out. One fetch serves every
// reader that wants a chunk while it is in flight.
type cache struct {
	mutex    sync.Mutex
	chunks   map[chunkKey]*chunk
	order    *list.List // front is the most recent
	capacity int
	fetch    func(target, int64) ([]byte, error)
	now      func() time.Time
}

func (c *cache) get(item target, index int64) *chunk {
	key := chunkKey{item.record.Reference, item.record.SourceNodeID, item.record.ItemID, item.size, index}
	c.mutex.Lock()
	if existing, ok := c.chunks[key]; ok {
		stale := false
		select {
		case <-existing.ready:
			stale = c.now().Sub(existing.fetched) > ChunkLife
		default:
		}
		if !stale {
			c.order.MoveToFront(existing.element)
			c.mutex.Unlock()
			return existing
		}
		c.order.Remove(existing.element)
		delete(c.chunks, key)
	}
	current := &chunk{ready: make(chan struct{})}
	current.element = c.order.PushFront(key)
	c.chunks[key] = current
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.chunks, oldest.Value.(chunkKey))
	}
	c.mutex.Unlock()
	go func() {
		data, err := c.fetch(item, index)
		c.mutex.Lock()
		current.data, current.err, current.fetched = data, err, c.now()
		if err != nil && c.chunks[key] == current {
			// A failed chunk is forgotten, so a later read retries it.
			c.order.Remove(current.element)
			delete(c.chunks, key)
		}
		c.mutex.Unlock()
		close(current.ready)
	}()
	return current
}

func (c *cache) forget(reference string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for key, held := range c.chunks {
		if key.reference == reference {
			c.order.Remove(held.element)
			delete(c.chunks, key)
		}
	}
}

func (server *Server) reportFailed(response http.ResponseWriter, request *http.Request) {
	reference := request.PathValue("reference")
	if !referencePattern.MatchString(reference) {
		http.NotFound(response, request)
		return
	}
	server.markFailed(reference)
	response.WriteHeader(http.StatusNoContent)
}

// markFailed records a film whose read failed. Jellyfin probes a file only
// when it changes, so a film whose probe failed would otherwise stay without
// media information.
func (server *Server) markFailed(reference string) {
	server.failedMutex.Lock()
	_, known := server.failed[reference]
	if !known {
		server.failed[reference] = server.options.now()
	}
	server.failedMutex.Unlock()
	if !known {
		server.saveFailed()
	}
}

// Heal retries each film whose read failed, every HealInterval until ctx
// ends. A film readable again is reported to Healed and forgotten; a film
// whose reference is gone is forgotten.
func (server *Server) Heal(ctx context.Context) {
	ticker := time.NewTicker(HealInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		server.HealOnce(ctx)
	}
}

// HealOnce makes one pass over the failed films.
func (server *Server) HealOnce(ctx context.Context) {
	server.failedMutex.Lock()
	pending := make([]string, 0, len(server.failed))
	for reference := range server.failed {
		pending = append(pending, reference)
	}
	server.failedMutex.Unlock()
	for _, reference := range pending {
		record, status, _ := server.authorize(ctx, reference)
		switch {
		case status == http.StatusNotFound:
			server.Forget(reference) // withdrawn or refused: nothing to heal
			continue
		case status != http.StatusOK:
			continue
		}
		if !server.readable(ctx, record) {
			continue
		}
		if server.options.Healed != nil {
			if err := server.options.Healed(ctx, reference); err != nil {
				server.options.Logger.Printf("filmread: marking %s changed: %v", record.Path, err)
				continue
			}
		}
		server.options.Logger.Printf("filmread: readable again, marked changed: %s", record.Path)
		server.failedMutex.Lock()
		delete(server.failed, reference)
		server.failedMutex.Unlock()
		server.saveFailed()
	}
}

func (server *Server) readable(ctx context.Context, record store.Materialized) bool {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	header := http.Header{}
	header.Set("Range", "bytes=0-0")
	response, err := server.options.Upstream.Media(ctx, record.SourceNodeID, http.MethodGet, record.ItemID, header)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusPartialContent
}

func (server *Server) loadFailed() {
	if server.options.FailedPath == "" {
		return
	}
	content, err := os.ReadFile(server.options.FailedPath)
	if err != nil {
		return
	}
	var references []string
	if json.Unmarshal(content, &references) != nil {
		return
	}
	for _, reference := range references {
		if referencePattern.MatchString(reference) {
			server.failed[reference] = server.options.now()
		}
	}
}

func (server *Server) saveFailed() {
	if server.options.FailedPath == "" {
		return
	}
	server.saveMutex.Lock()
	defer server.saveMutex.Unlock()
	server.failedMutex.Lock()
	references := make([]string, 0, len(server.failed))
	for reference := range server.failed {
		references = append(references, reference)
	}
	server.failedMutex.Unlock()
	encoded, _ := json.Marshal(references)
	temporary := server.options.FailedPath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(server.options.FailedPath), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return
	}
	if err := os.Rename(temporary, server.options.FailedPath); err != nil {
		server.options.Logger.Printf("filmread: keeping failed films: %v", strings.TrimSpace(err.Error()))
	}
}
