// Package relay is the destination's local media relay (design-spec section
// 9, "The relay"). Jellyfin opens a generated .strm, which names this relay
// and a random reference; the relay forwards the request to the source over
// mutual TLS and streams the answer back.
//
// Jellyfin sends no credential when it opens a .strm, and a local user can
// read the .strm's URL through Jellyfin (phase-0-results.md section 8). So
// the URL is not a credential. A request is served only if it comes from an
// allow-listed client address, names a reference that has been issued and not
// revoked, and passes the destination's policy for that item right now
// (conformance.md assumption A-10).
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jellymesh/internal/store"
)

// Path is where the relay serves references.
const Path = "/r/"

var referencePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ErrRefused is returned by a Policy that will not let an item be played.
var ErrRefused = errors.New("the destination's policy does not allow this item")

// Resolver finds what a reference names; store.MaterializedRepository
// satisfies it.
type Resolver interface {
	ByReference(ctx context.Context, reference string) (store.Materialized, bool, error)
}

// Policy decides whether an item may be played now: its source is a member
// this node has not blocked, and its library is published and not opted out.
type Policy interface {
	MayPlay(ctx context.Context, sourceNodeID string, libraryID string) error
}

// Upstream opens an item's media at its source.
type Upstream interface {
	Media(ctx context.Context, sourceNodeID string, method string, itemID string, header http.Header) (*http.Response, error)
}

// Server is the relay.
type Server struct {
	allowed  []*net.IPNet
	resolver Resolver
	policy   Policy
	upstream Upstream
	logger   *log.Logger
	heads    *HeadCache
}

// SetHeadCache has the relay keep and serve the first bytes of each item.
func (server *Server) SetHeadCache(cache *HeadCache) { server.heads = cache }

// ParseAllowed parses a comma-separated list of client addresses and CIDR
// ranges.
func ParseAllowed(list string) ([]*net.IPNet, error) {
	var networks []*net.IPNet
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			if ip := net.ParseIP(entry); ip != nil && ip.To4() != nil {
				entry += "/32"
			} else {
				entry += "/128"
			}
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("relay client %q: %w", entry, err)
		}
		networks = append(networks, network)
	}
	if len(networks) == 0 {
		return nil, errors.New("the relay needs at least one allowed client address")
	}
	return networks, nil
}

// New returns a relay serving only clients in allowed.
func New(allowed []*net.IPNet, resolver Resolver, policy Policy, upstream Upstream, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{allowed: allowed, resolver: resolver, policy: policy, upstream: upstream, logger: logger}
}

// Handler returns the relay's routes. Anything but exactly Path followed by
// a reference is a plain 404: the relay neither cleans nor redirects paths.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path+"{reference}", server.relay)
	mux.HandleFunc("HEAD "+Path+"{reference}", server.relay)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rest, ok := strings.CutPrefix(request.URL.Path, Path)
		if !ok || !referencePattern.MatchString(rest) {
			http.NotFound(response, request)
			return
		}
		mux.ServeHTTP(response, request)
	})
}

func (server *Server) clientAllowed(request *http.Request) bool {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range server.allowed {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// relayHeaders are what the relay passes through from the source.
var relayHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"}

const copyChunk = 32 << 10

// continuationDelay is how long the relay waits, after serving a cached head,
// before asking the source for what follows.
const continuationDelay = 200 * time.Millisecond

func (server *Server) relay(response http.ResponseWriter, request *http.Request) {
	if !server.clientAllowed(request) {
		http.Error(response, "this client may not use the relay", http.StatusForbidden)
		return
	}
	reference := request.PathValue("reference")
	if !referencePattern.MatchString(reference) {
		http.NotFound(response, request)
		return
	}
	target, found, err := server.resolver.ByReference(request.Context(), reference)
	if err != nil {
		http.Error(response, "the relay could not read its references", http.StatusServiceUnavailable)
		return
	}
	if !found {
		// Never issued, or revoked when the item was withdrawn.
		http.NotFound(response, request)
		return
	}
	if err := server.policy.MayPlay(request.Context(), target.SourceNodeID, target.LibraryID); err != nil {
		http.NotFound(response, request)
		return
	}

	span, cacheable := parseRange(request.Header.Get("Range"))
	if server.heads != nil && cacheable {
		if head, hit := server.heads.Get(target); hit && span.start < int64(len(head.Data)) {
			// The cache saves bytes, never a decision: the source authorizes
			// every request, so a hit is confirmed with a bodiless HEAD, which
			// also shows the file is still the size the head was cut from.
			switch server.confirm(request.Context(), target, head) {
			case confirmed:
				server.fromHead(response, request, target, head, span)
				return
			case refused:
				http.NotFound(response, request)
				return
			case unreachable:
				http.Error(response, "the source of this item is not reachable right now", http.StatusBadGateway)
				return
			case stale:
				server.heads.Remove(target) // and serve from the source below
			}
		}
	}

	header := http.Header{}
	if value := request.Header.Get("Range"); value != "" {
		header.Set("Range", value)
	}
	upstream, err := server.upstream.Media(request.Context(), target.SourceNodeID, request.Method, target.ItemID, header)
	if err != nil {
		// A clear failure, and nothing else changes: an unreachable source is
		// never a reason to withdraw what this node holds (C-PB-2).
		server.logger.Printf("relay: source %s unavailable: %v", target.SourceNodeID, err)
		http.Error(response, "the source of this item is not reachable right now", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()

	switch upstream.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	case http.StatusNotFound:
		http.NotFound(response, request)
		return
	default:
		http.Error(response, "the source of this item refused the request", http.StatusBadGateway)
		return
	}
	for _, name := range relayHeaders {
		if value := upstream.Header.Get(name); value != "" {
			response.Header().Set(name, value)
		}
	}
	response.WriteHeader(upstream.StatusCode)
	if request.Method == http.MethodHead {
		return
	}

	// A response that starts at the beginning of the file fills the cache
	// with as much as the client actually reads, up to the head size.
	var fill *filling
	if server.heads != nil && cacheable && span.start == 0 && request.Method == http.MethodGet {
		if total := totalSize(upstream); total > 0 {
			fill = &filling{total: total, contentType: upstream.Header.Get("Content-Type"), limit: server.heads.perItem}
		}
	}
	server.copy(response, upstream.Body, fill)
	if fill != nil {
		_ = server.heads.Put(target, Head{Data: fill.data, Total: fill.total, ContentType: fill.contentType})
	}
}

type confirmation int

const (
	confirmed confirmation = iota
	refused
	unreachable
	stale
)

// confirm asks the source whether this node may still have the item, and
// whether it is still the file the head was cut from.
func (server *Server) confirm(ctx context.Context, target store.Materialized, head Head) confirmation {
	upstream, err := server.upstream.Media(ctx, target.SourceNodeID, http.MethodHead, target.ItemID, http.Header{})
	if err != nil {
		return unreachable
	}
	upstream.Body.Close()
	switch {
	case upstream.StatusCode == http.StatusNotFound:
		return refused
	case upstream.StatusCode != http.StatusOK:
		return unreachable
	case upstream.ContentLength != head.Total:
		return stale
	}
	return confirmed
}

// filling collects the start of a stream for the head cache.
type filling struct {
	data        []byte
	total       int64
	contentType string
	limit       int
}

func (fill *filling) add(chunk []byte) {
	if fill == nil || len(fill.data) >= fill.limit {
		return
	}
	room := fill.limit - len(fill.data)
	if len(chunk) > room {
		chunk = chunk[:room]
	}
	fill.data = append(fill.data, chunk...)
}

// copy streams body to the client a chunk at a time, flushing each one, and
// stops when either side goes away.
func (server *Server) copy(response http.ResponseWriter, body io.Reader, fill *filling) bool {
	flusher, _ := response.(http.Flusher)
	buffer := make([]byte, copyChunk)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			fill.add(buffer[:read])
			if _, writeErr := response.Write(buffer[:read]); writeErr != nil {
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return errors.Is(err, io.EOF)
		}
	}
}

// fromHead answers a request that starts within a cached head. The head
// supplies what it can; the rest, if the client wants it, comes from the
// source as a range starting where the head ends, and extends the head.
func (server *Server) fromHead(response http.ResponseWriter, request *http.Request, target store.Materialized, head Head, span byteRange) {
	end := span.end
	if end < 0 || end >= head.Total {
		end = head.Total - 1
	}
	response.Header().Set("Accept-Ranges", "bytes")
	if head.ContentType != "" {
		response.Header().Set("Content-Type", head.ContentType)
	}
	response.Header().Set("Content-Length", strconv.FormatInt(end-span.start+1, 10))
	status := http.StatusOK
	if span.requested {
		status = http.StatusPartialContent
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", span.start, end, head.Total))
	}
	response.WriteHeader(status)
	if request.Method == http.MethodHead {
		return
	}

	cached := int64(len(head.Data))
	stop := end + 1
	if stop > cached {
		stop = cached
	}
	if _, err := response.Write(head.Data[span.start:stop]); err != nil {
		return
	}
	if flusher, ok := response.(http.Flusher); ok {
		flusher.Flush()
	}
	if end < cached {
		return
	}
	// The head may sit entirely in socket buffers before the client has read
	// any of it, so a successful write does not mean the client wants more. A
	// probe reads about a megabyte and disconnects, and the server notices at
	// once; a player reading on is still here after a moment. Waiting that
	// moment keeps probes off the source's uplink, and costs a player nothing
	// it would notice, with a whole head already in its buffer.
	select {
	case <-request.Context().Done():
		return
	case <-time.After(continuationDelay):
	}

	// The client wants more than the head holds: continue from the source.
	header := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", cached, end)}}
	upstream, err := server.upstream.Media(request.Context(), target.SourceNodeID, http.MethodGet, target.ItemID, header)
	if err != nil {
		server.logger.Printf("relay: source %s unavailable mid-stream: %v", target.SourceNodeID, err)
		return
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusPartialContent || totalSize(upstream) != head.Total {
		// The file changed under this revision, or the source misbehaved:
		// never splice mismatched bytes, and stop trusting the head.
		server.heads.Remove(target)
		return
	}
	var fill *filling
	if int(cached) < server.heads.perItem {
		fill = &filling{data: append([]byte(nil), head.Data...), total: head.Total, contentType: head.ContentType, limit: server.heads.perItem}
	}
	server.copy(response, upstream.Body, fill)
	if fill != nil && len(fill.data) > len(head.Data) {
		_ = server.heads.Put(target, Head{Data: fill.data, Total: fill.total, ContentType: fill.contentType})
	}
}

// byteRange is one requested range. end is -1 when open-ended.
type byteRange struct {
	start, end int64
	requested  bool
}

// parseRange reads a single "bytes=a-b" or "bytes=a-" range. A suffix range,
// several ranges, or anything malformed is not served from the cache, and is
// passed to the source as it came.
func parseRange(header string) (byteRange, bool) {
	if header == "" {
		return byteRange{start: 0, end: -1}, true
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return byteRange{}, false
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok || first == "" {
		return byteRange{}, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || start < 0 {
		return byteRange{}, false
	}
	end := int64(-1)
	if last = strings.TrimSpace(last); last != "" {
		if end, err = strconv.ParseInt(last, 10, 64); err != nil || end < start {
			return byteRange{}, false
		}
	}
	return byteRange{start: start, end: end, requested: true}, true
}

// totalSize reads a response's full length: from Content-Range for a partial
// response, from Content-Length for a whole one.
func totalSize(response *http.Response) int64 {
	if response.StatusCode == http.StatusPartialContent {
		contentRange := response.Header.Get("Content-Range")
		if index := strings.LastIndex(contentRange, "/"); index >= 0 {
			if total, err := strconv.ParseInt(contentRange[index+1:], 10, 64); err == nil {
				return total
			}
		}
		return 0
	}
	if response.StatusCode == http.StatusOK && response.ContentLength > 0 {
		return response.ContentLength
	}
	return 0
}
