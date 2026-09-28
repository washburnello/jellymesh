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
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"

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
}

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
	flusher, _ := response.(http.Flusher)
	buffer := make([]byte, copyChunk)
	for {
		read, err := upstream.Body.Read(buffer)
		if read > 0 {
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
