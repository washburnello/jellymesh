package catalogsync

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"jellymesh/internal/sourcecatalog"
)

const (
	mediaPath     = "/jellymesh/v1/groups/{group}/media/{item}"
	subtitlesPath = "/jellymesh/v1/groups/{group}/subtitles/{item}/{index}"
	imagesPath    = "/jellymesh/v1/groups/{group}/images/{item}/primary"

	// copyChunk is how much of a stream is read before it is written on.
	// It bounds what the source holds per stream; nothing is buffered beyond.
	copyChunk = 32 << 10
)

// MediaSource opens a node's own media through Jellyfin; jellyfin.Client
// satisfies it.
type MediaSource interface {
	Stream(ctx context.Context, method string, itemID string, rangeHeader string) (*http.Response, error)
	Subtitle(ctx context.Context, itemID string, mediaSourceID string, index int) (*http.Response, error)
	PrimaryImage(ctx context.Context, itemID string) (*http.Response, error)
}

// SetMedia lets the server stream this node's media, with ceiling bytes per
// second shared by all of one destination's streams. A ceiling of zero means
// no limit.
func (server *Server) SetMedia(media MediaSource, ceiling int64) {
	server.media = media
	server.ceiling = ceiling
	server.pacers = map[string]*pacer{}
}

func (server *Server) registerMedia(members *http.ServeMux) {
	members.HandleFunc("GET "+mediaPath, server.stream)
	members.HandleFunc("HEAD "+mediaPath, server.stream)
	members.HandleFunc("GET "+subtitlesPath, server.subtitle)
	members.HandleFunc("GET "+imagesPath, server.image)
}

// streamHeaders are the response headers passed through from Jellyfin.
var streamHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"}

// stream relays an item's media to a member. Every request is authorized
// against live state, because Jellyfin's own stream route ignores library
// permissions (phase-0-results.md section 8).
func (server *Server) stream(response http.ResponseWriter, request *http.Request) {
	memberID, ok := server.authorizeMember(response, request)
	if !ok || server.media == nil {
		if ok {
			http.NotFound(response, request)
		}
		return
	}
	itemID := request.PathValue("item")
	if _, err := server.catalog.Authorize(request.Context(), itemID); err != nil {
		http.NotFound(response, request)
		return
	}
	upstream, err := server.media.Stream(request.Context(), request.Method, itemID, request.Header.Get("Range"))
	if err != nil {
		http.Error(response, "the source's Jellyfin could not be reached", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	server.pass(response, request, memberID, upstream, streamHeaders)
}

func (server *Server) subtitle(response http.ResponseWriter, request *http.Request) {
	memberID, ok := server.authorizeMember(response, request)
	if !ok || server.media == nil {
		if ok {
			http.NotFound(response, request)
		}
		return
	}
	itemID := request.PathValue("item")
	index, err := strconv.Atoi(request.PathValue("index"))
	if err != nil {
		http.NotFound(response, request)
		return
	}
	metadata, err := server.catalog.ItemMetadata(request.Context(), itemID)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	// Only a subtitle the catalog lists may be fetched; the index is never
	// passed to Jellyfin unchecked.
	var subtitle *sourcecatalog.Subtitle
	for position := range metadata.Subtitles {
		if metadata.Subtitles[position].Index == index {
			subtitle = &metadata.Subtitles[position]
		}
	}
	if subtitle == nil {
		http.NotFound(response, request)
		return
	}
	upstream, err := server.media.Subtitle(request.Context(), itemID, subtitle.MediaSourceID, index)
	if err != nil {
		http.Error(response, "the source's Jellyfin could not be reached", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	server.pass(response, request, memberID, upstream, []string{"Content-Type"})
}

func (server *Server) image(response http.ResponseWriter, request *http.Request) {
	memberID, ok := server.authorizeMember(response, request)
	if !ok || server.media == nil {
		if ok {
			http.NotFound(response, request)
		}
		return
	}
	itemID := request.PathValue("item")
	if _, err := server.catalog.Authorize(request.Context(), itemID); err != nil {
		http.NotFound(response, request)
		return
	}
	upstream, err := server.media.PrimaryImage(request.Context(), itemID)
	if err != nil {
		http.Error(response, "the source's Jellyfin could not be reached", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	server.pass(response, request, memberID, upstream, []string{"Content-Type", "Content-Length"})
}

// pass copies Jellyfin's response to the member, keeping only the named
// headers, chunk by chunk, paced by the member's ceiling. It stops as soon as
// either side goes away, so a destination that disconnects stops the read
// from Jellyfin too.
func (server *Server) pass(response http.ResponseWriter, request *http.Request, memberID string, upstream *http.Response, headers []string) {
	for _, name := range headers {
		if value := upstream.Header.Get(name); value != "" {
			response.Header().Set(name, value)
		}
	}
	switch upstream.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	case http.StatusNotFound:
		http.NotFound(response, request)
		return
	default:
		http.Error(response, "the source's Jellyfin refused the request", http.StatusBadGateway)
		return
	}
	response.WriteHeader(upstream.StatusCode)
	if request.Method == http.MethodHead {
		return
	}
	pacer := server.pacerFor(memberID)
	flusher, _ := response.(http.Flusher)
	buffer := make([]byte, copyChunk)
	for {
		read, err := upstream.Body.Read(buffer)
		if read > 0 {
			if pacer != nil && pacer.wait(request.Context(), read) != nil {
				return
			}
			if _, writeErr := response.Write(buffer[:read]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (server *Server) pacerFor(memberID string) *pacer {
	if server.ceiling <= 0 {
		return nil
	}
	server.pacerMutex.Lock()
	defer server.pacerMutex.Unlock()
	current, ok := server.pacers[memberID]
	if !ok {
		current = &pacer{rate: float64(server.ceiling)}
		server.pacers[memberID] = current
	}
	return current
}

// pacer spaces writes so that one destination's streams together never
// exceed its ceiling, however many run at once (A-1, C-PB-3).
type pacer struct {
	mutex sync.Mutex
	rate  float64
	next  time.Time
}

// maxBurst is how far ahead of the ceiling a destination may run, so short
// pauses in reading do not turn into bursts above it.
const maxBurst = 250 * time.Millisecond

func (pacer *pacer) wait(ctx context.Context, bytes int) error {
	pacer.mutex.Lock()
	now := time.Now()
	if pacer.next.Before(now.Add(-maxBurst)) {
		pacer.next = now.Add(-maxBurst)
	}
	start := pacer.next
	pacer.next = start.Add(time.Duration(float64(bytes) / pacer.rate * float64(time.Second)))
	pacer.mutex.Unlock()

	delay := time.Until(start)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
