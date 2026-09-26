// Package replication moves the group log between members over mutual TLS
// (design-spec section 8, "Replication").
//
// It is pull-based. A member asks a peer for its log head and, if the peer is
// ahead, for the events it is missing. Events verify themselves against the
// log the member already holds, so the peer serving them needs no special
// trust: it can withhold events but cannot forge or reorder them, and asking
// a different member defeats withholding.
package replication

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

const (
	// MaxPageSize bounds one events response, so a single request cannot
	// make a node serialize an unbounded log.
	MaxPageSize = 256

	headPath      = "/jellymesh/v1/groups/{group}/log/head"
	eventsPath    = "/jellymesh/v1/groups/{group}/log/events"
	proposalsPath = "/jellymesh/v1/groups/{group}/proposals"

	// maxProposalBytes bounds a submitted proposal; the largest, an
	// admission, is well under a kilobyte.
	maxProposalBytes = 64 << 10
)

// Server serves this node's group logs to their members and, when this node
// is a group's owner, sequences the proposals members submit.
type Server struct {
	mutex    sync.RWMutex
	groups   map[string]*membership.Group
	identity *node.Identity
	now      func() time.Time
}

// NewServer returns a server that signs as identity when it sequences a
// submitted proposal. identity may be nil for a node that only serves logs.
func NewServer(identity *node.Identity) *Server {
	return &Server{groups: make(map[string]*membership.Group), identity: identity, now: time.Now}
}

// Add serves group under groupID.
func (server *Server) Add(groupID string, group *membership.Group) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.groups[groupID] = group
}

func (server *Server) group(groupID string) (*membership.Group, bool) {
	server.mutex.RLock()
	defer server.mutex.RUnlock()
	group, ok := server.groups[groupID]
	return group, ok
}

// IsTrusted satisfies transport.TrustStore for the federation listener: a key
// may connect if it belongs to a member of any group this node serves. Each
// request is then checked against the one group it asks about, so a member
// of one group cannot read another's log.
func (server *Server) IsTrusted(fingerprint transport.Fingerprint) bool {
	server.mutex.RLock()
	defer server.mutex.RUnlock()
	for _, group := range server.groups {
		if group.IsTrusted(fingerprint) {
			return true
		}
	}
	return false
}

// Handler returns the replication routes.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+headPath, server.head)
	mux.HandleFunc("GET "+eventsPath, server.events)
	mux.HandleFunc("POST "+proposalsPath, server.propose)
	return mux
}

// authorize returns the requested group if the connection's authenticated
// key belongs to one of its members. Anything else is a 404, so a non-member
// cannot even learn which groups this node serves.
func (server *Server) authorize(response http.ResponseWriter, request *http.Request) (*membership.Group, bool) {
	if request.TLS == nil {
		http.NotFound(response, request)
		return nil, false
	}
	fingerprint, err := transport.PeerFingerprint(*request.TLS)
	if err != nil {
		http.NotFound(response, request)
		return nil, false
	}
	group, ok := server.group(request.PathValue("group"))
	if !ok || !group.IsTrusted(fingerprint) {
		http.NotFound(response, request)
		return nil, false
	}
	return group, true
}

func (server *Server) head(response http.ResponseWriter, request *http.Request) {
	group, ok := server.authorize(response, request)
	if !ok {
		return
	}
	writeJSON(response, group.Head())
}

func (server *Server) events(response http.ResponseWriter, request *http.Request) {
	group, ok := server.authorize(response, request)
	if !ok {
		return
	}
	after, err := strconv.ParseUint(request.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(response, "after must be a sequence number", http.StatusBadRequest)
		return
	}
	limit := MaxPageSize
	if raw := request.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			http.Error(response, "limit must be a positive number", http.StatusBadRequest)
			return
		}
		limit = min(parsed, MaxPageSize)
	}
	events := group.EventsAfter(after)
	if len(events) > limit {
		events = events[:limit]
	}
	if events == nil {
		events = []grouplog.Event{}
	}
	writeJSON(response, events)
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}

// propose sequences a proposal submitted by a member. Only the owner's node
// can do this; any other node answers 409 so the submitter can find the
// owner. The proposal's own signature, not the submitting connection, is what
// authorizes it, so a member may relay another member's proposal, and the
// log applies the same role rules it applies to every event.
func (server *Server) propose(response http.ResponseWriter, request *http.Request) {
	group, ok := server.authorize(response, request)
	if !ok {
		return
	}
	var proposal grouplog.Proposal
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxProposalBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		http.Error(response, "malformed proposal", http.StatusBadRequest)
		return
	}
	if server.identity == nil {
		http.Error(response, "this node does not sequence", http.StatusConflict)
		return
	}
	event, err := group.Sequence(request.Context(), server.identity, proposal, server.now())
	switch {
	case errors.Is(err, grouplog.ErrWrongSigner):
		http.Error(response, "this node is not the group owner", http.StatusConflict)
	case err != nil:
		// The rules refused it. The reason is safe to return: the submitter
		// is a member, and every member can compute it from the log.
		http.Error(response, err.Error(), http.StatusUnprocessableEntity)
	default:
		writeJSON(response, event)
	}
}
