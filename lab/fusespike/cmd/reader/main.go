// Command reader stands in for the Jellymesh daemon's side of the FUSE spike
// (#60): it turns the mount's reads into range requests to a source, in
// fixed chunks, with a bounded cache and sequential read-ahead so that
// playback is not paced by one round trip per kernel read. It can be killed
// and restarted at will; the mount keeps its tree and fails reads fast
// meanwhile.
package main

import (
	"container/list"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const chunkSize = 1 << 20

// maxAhead bounds read-ahead, in chunks, once a stream is clearly sequential.
const maxAhead = 16

type chunkKey struct {
	ref   string
	index int64
}

type chunk struct {
	ready chan struct{}
	data  []byte
	err   error
	elem  *list.Element
}

type cache struct {
	mutex    sync.Mutex
	chunks   map[chunkKey]*chunk
	order    *list.List // front = most recent
	capacity int
}

// streaks track sequential reading per ref, so read-ahead starts only once a
// stream is being played and not for a probe's scattered reads.
type streak struct {
	last  int64
	count int
}

var (
	streaks   = map[string]*streak{}
	streakMu  sync.Mutex
	sources   map[string]source // ref -> source
	store     = &cache{chunks: map[chunkKey]*chunk{}, order: list.New()}
	fetchHTTP = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 32, ResponseHeaderTimeout: 20 * time.Second}}

	fetched, served, fetchErrors atomic.Int64
	perRef                       sync.Map // ref -> *atomic.Int64 fetched
)

type source struct {
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// get returns a chunk, fetching it once however many readers want it.
func (c *cache) get(key chunkKey) *chunk {
	c.mutex.Lock()
	if existing, ok := c.chunks[key]; ok {
		c.order.MoveToFront(existing.elem)
		c.mutex.Unlock()
		return existing
	}
	current := &chunk{ready: make(chan struct{})}
	current.elem = c.order.PushFront(key)
	c.chunks[key] = current
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.chunks, oldest.Value.(chunkKey))
	}
	c.mutex.Unlock()
	go func() {
		current.data, current.err = fetch(key)
		if current.err != nil {
			// A failed chunk is forgotten so a later read retries it.
			c.mutex.Lock()
			if c.chunks[key] == current {
				c.order.Remove(current.elem)
				delete(c.chunks, key)
			}
			c.mutex.Unlock()
		}
		close(current.ready)
	}()
	return current
}

func fetch(key chunkKey) ([]byte, error) {
	src, ok := sources[key.ref]
	if !ok {
		return nil, errors.New("unknown ref")
	}
	start := key.index * chunkSize
	end := start + chunkSize - 1
	if end >= src.Size {
		end = src.Size - 1
	}
	request, _ := http.NewRequest(http.MethodGet, src.URL, nil)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	response, err := fetchHTTP.Do(request)
	if err != nil {
		fetchErrors.Add(1)
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		fetchErrors.Add(1)
		return nil, fmt.Errorf("source answered %d", response.StatusCode)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || int64(len(data)) != end-start+1 {
		fetchErrors.Add(1)
		return nil, fmt.Errorf("short chunk: %d bytes, %v", len(data), err)
	}
	fetched.Add(int64(len(data)))
	value, _ := perRef.LoadOrStore(key.ref, new(atomic.Int64))
	value.(*atomic.Int64).Add(int64(len(data)))
	return data, nil
}

// readAhead reports how many chunks to fetch beyond index: none for an
// isolated read, then doubling with each further sequential chunk.
func readAhead(ref string, index int64) int64 {
	streakMu.Lock()
	defer streakMu.Unlock()
	current := streaks[ref]
	if current == nil {
		current = &streak{last: -2}
		streaks[ref] = current
	}
	switch {
	case index == current.last:
		// the same chunk again; keep the streak as it is
	case index == current.last+1:
		current.count++
	default:
		current.count = 0
	}
	current.last = index
	if current.count == 0 {
		return 0
	}
	ahead := int64(1) << min(current.count, 4)
	return min(ahead, maxAhead)
}

func handleRead(response http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	ref := query.Get("ref")
	off, err1 := strconv.ParseInt(query.Get("off"), 10, 64)
	length, err2 := strconv.ParseInt(query.Get("len"), 10, 64)
	src, ok := sources[ref]
	if err1 != nil || err2 != nil || !ok || off < 0 || length <= 0 || off >= src.Size {
		http.Error(response, "bad read", http.StatusBadRequest)
		return
	}
	if off+length > src.Size {
		length = src.Size - off
	}
	out := make([]byte, 0, length)
	for position := off; position < off+length; {
		index := position / chunkSize
		current := store.get(chunkKey{ref, index})
		for ahead := int64(1); ahead <= readAhead(ref, index); ahead++ {
			if (index+ahead)*chunkSize < src.Size {
				store.get(chunkKey{ref, index + ahead})
			}
		}
		select {
		case <-current.ready:
		case <-request.Context().Done():
			return // the mount gave up; its deadline has passed
		}
		if current.err != nil {
			http.Error(response, current.err.Error(), http.StatusBadGateway)
			return
		}
		within := position - index*chunkSize
		take := int64(len(current.data)) - within
		if remaining := off + length - position; take > remaining {
			take = remaining
		}
		out = append(out, current.data[within:within+take]...)
		position += take
	}
	served.Add(int64(len(out)))
	response.Header().Set("Content-Length", strconv.Itoa(len(out)))
	response.Write(out)
}

func main() {
	sourcesPath := flag.String("sources", "/state/sources.json", "ref to source URL and size")
	listen := flag.String("listen", ":8200", "listen address")
	cacheMB := flag.Int("cache-mb", 512, "chunk cache size")
	flag.Parse()
	raw, err := os.ReadFile(*sourcesPath)
	if err != nil {
		log.Fatalf("sources: %v", err)
	}
	if err := json.Unmarshal(raw, &sources); err != nil {
		log.Fatalf("sources: %v", err)
	}
	store.capacity = *cacheMB * (1 << 20) / chunkSize
	http.HandleFunc("/read", handleRead)
	http.HandleFunc("/stats", func(response http.ResponseWriter, request *http.Request) {
		refs := map[string]int64{}
		perRef.Range(func(key, value any) bool { refs[key.(string)] = value.(*atomic.Int64).Load(); return true })
		json.NewEncoder(response).Encode(map[string]any{"fetched": fetched.Load(), "served": served.Load(), "fetch_errors": fetchErrors.Load(), "refs": refs})
		if request.URL.Query().Get("reset") == "1" {
			perRef.Range(func(key, _ any) bool { perRef.Delete(key); return true })
			fetched.Store(0)
			served.Store(0)
			fetchErrors.Store(0)
		}
	})
	log.Printf("reader for %d refs on %s", len(sources), *listen)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
