// Package catalogsync carries the source catalog to destinations
// (design-spec section 9, "The catalog API" and "The destination catalog").
//
// The source serves its catalog on two member-only routes. The destination
// pulls it page by page, applies each page and its cursor atomically, and
// enforces its own decisions again on what it receives: an opted-out library,
// or one the source does not list as published, is dropped even if a
// misbehaving source sends it.
package catalogsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"jellymesh/internal/audit"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/replication"
	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
	"jellymesh/internal/transport"
)

const (
	librariesPath = "/jellymesh/v1/groups/{group}/catalog/libraries"
	changesPath   = "/jellymesh/v1/groups/{group}/catalog/changes"

	// DefaultPageSize is how many changes a destination asks for at once.
	DefaultPageSize = 500
)

// Server serves this node's source catalog to the members of its group.
type Server struct {
	catalog *sourcecatalog.Catalog
	group   *membership.Group
	groupID string
	blocks  membership.BlockList
}

func NewServer(catalog *sourcecatalog.Catalog, group *membership.Group, groupID string, blocks membership.BlockList) *Server {
	return &Server{catalog: catalog, group: group, groupID: groupID, blocks: blocks}
}

// Register adds the catalog routes. Both are for members only.
func (server *Server) Register(_ *http.ServeMux, members *http.ServeMux) {
	members.HandleFunc("GET "+librariesPath, server.libraries)
	members.HandleFunc("GET "+changesPath, server.changes)
}

// authorize requires the caller to be a member of this group whom this node
// has not blocked. A block is a pairwise media cut, so a blocked member gets
// nothing, as does anyone else: every refusal is a 404.
func (server *Server) authorize(response http.ResponseWriter, request *http.Request) bool {
	if request.PathValue("group") != server.groupID || request.TLS == nil || server.catalog == nil {
		http.NotFound(response, request)
		return false
	}
	fingerprint, err := peerFingerprint(request)
	if err != nil {
		http.NotFound(response, request)
		return false
	}
	var memberID string
	_ = server.group.View(func(state *grouplog.State) {
		if member, ok := state.MemberByFingerprint(fingerprint); ok {
			memberID = member.NodeID
		}
	})
	if memberID == "" {
		http.NotFound(response, request)
		return false
	}
	if server.blocks != nil {
		blocked, err := server.blocks.IsBlocked(request.Context(), memberID)
		if err != nil || blocked {
			http.NotFound(response, request)
			return false
		}
	}
	return true
}

func (server *Server) libraries(response http.ResponseWriter, request *http.Request) {
	if !server.authorize(response, request) {
		return
	}
	libraries, err := server.catalog.Libraries(request.Context())
	if err != nil {
		http.Error(response, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(response, libraries)
}

func (server *Server) changes(response http.ResponseWriter, request *http.Request) {
	if !server.authorize(response, request) {
		return
	}
	query := request.URL.Query()
	after, err := strconv.ParseUint(query.Get("after"), 10, 64)
	if err != nil {
		http.Error(response, "after must be a sequence number", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(query.Get("limit"))
	exclude := map[string]bool{}
	for _, library := range strings.Split(query.Get("exclude"), ",") {
		if library = strings.TrimSpace(library); library != "" {
			exclude[library] = true
		}
	}
	page, err := server.catalog.Changes(request.Context(), after, limit, exclude)
	if err != nil {
		http.Error(response, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(response, page)
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}

// Destination applies sources' catalogs to this node's remote catalog.
type Destination struct {
	items    *store.RemoteCatalogRepository
	cursors  *store.SyncRepository
	client   *replication.Client
	groupID  string
	audit    *audit.Log
	pageSize int
	now      func() time.Time
}

func NewDestination(items *store.RemoteCatalogRepository, cursors *store.SyncRepository, client *replication.Client, groupID string, log *audit.Log) *Destination {
	return &Destination{items: items, cursors: cursors, client: client, groupID: groupID, audit: log, pageSize: DefaultPageSize, now: time.Now}
}

// SetPageSize changes how many changes are asked for at once.
func (destination *Destination) SetPageSize(size int) {
	if size > 0 {
		destination.pageSize = size
	}
}

// SetClock replaces the destination's clock, for tests of retention.
func (destination *Destination) SetClock(now func() time.Time) { destination.now = now }

// Result reports one sync.
type Result struct {
	Libraries []sourcecatalog.PublishedLibrary
	Applied   int
	Removed   int
	Stale     int
	Dropped   int
}

// Sync brings this node's view of one source up to date. optedOut names the
// source's libraries this node has opted out of. A failure keeps the cursor
// and every record, and is recorded against the source's sync health.
func (destination *Destination) Sync(ctx context.Context, source replication.Peer, sourceNodeID string, optedOut map[string]bool) (Result, error) {
	result, err := destination.sync(ctx, source, sourceNodeID, optedOut)
	if err != nil {
		if recordErr := destination.cursors.RecordFailure(ctx, sourceNodeID, category(err), destination.now()); recordErr != nil {
			err = errors.Join(err, recordErr)
		}
	}
	return result, err
}

func category(err error) string {
	switch {
	case errors.Is(err, replication.ErrPeerUnreachable):
		return "unreachable"
	case errors.Is(err, replication.ErrRefusedByPeer), errors.Is(err, replication.ErrNotServed):
		return "refused"
	default:
		return "failed"
	}
}

func (destination *Destination) sync(ctx context.Context, source replication.Peer, sourceNodeID string, optedOut map[string]bool) (Result, error) {
	var result Result
	base := "/jellymesh/v1/groups/" + url.PathEscape(destination.groupID) + "/catalog"
	if err := destination.client.GetJSON(ctx, source, base+"/libraries", &result.Libraries); err != nil {
		return result, fmt.Errorf("fetch libraries: %w", err)
	}
	published := map[string]bool{}
	for _, library := range result.Libraries {
		published[library.LibraryID] = true
	}

	cursor := uint64(0)
	if state, found, err := destination.cursors.Get(ctx, sourceNodeID); err != nil {
		return result, err
	} else if found && state.Cursor != "" {
		if cursor, err = strconv.ParseUint(state.Cursor, 10, 64); err != nil {
			return result, fmt.Errorf("stored cursor %q: %w", state.Cursor, err)
		}
	}

	var excluded []string
	for library, out := range optedOut {
		if out {
			excluded = append(excluded, library)
		}
	}
	sort.Strings(excluded)

	for {
		query := url.Values{
			"after": {strconv.FormatUint(cursor, 10)}, "limit": {strconv.Itoa(destination.pageSize)},
			"exclude": {strings.Join(excluded, ",")},
		}
		var page sourcecatalog.ChangePage
		if err := destination.client.GetJSON(ctx, source, base+"/changes?"+query.Encode(), &page); err != nil {
			return result, fmt.Errorf("fetch changes after %d: %w", cursor, err)
		}
		if page.Next < cursor {
			return result, fmt.Errorf("the source moved the cursor backwards, from %d to %d", cursor, page.Next)
		}
		applied, err := destination.apply(ctx, sourceNodeID, page, published, optedOut)
		result.Applied += applied.Applied
		result.Removed += applied.Removed
		result.Stale += applied.Stale
		result.Dropped += applied.Dropped
		if err != nil {
			return result, err
		}
		cursor = page.Next
		if !page.More {
			return result, nil
		}
	}
}

// apply turns a page into operations, enforcing this node's rules on it, and
// writes them with the new cursor in one transaction.
func (destination *Destination) apply(ctx context.Context, sourceNodeID string, page sourcecatalog.ChangePage, published map[string]bool, optedOut map[string]bool) (Result, error) {
	var result Result
	ids := make([]string, 0, len(page.Changes))
	for _, change := range page.Changes {
		ids = append(ids, change.ItemID)
	}
	existing, err := destination.items.Revisions(ctx, sourceNodeID, ids)
	if err != nil {
		return result, err
	}

	var operations []store.RemoteOperation
	for _, change := range page.Changes {
		current, held := existing[change.ItemID]
		if held && change.Revision <= current.Revision {
			result.Stale++
			continue
		}
		if change.Tombstone {
			if held {
				operations = append(operations, store.RemoteOperation{Item: current, Remove: true, LogicalWorkID: LogicalWorkID(current.ItemType, current.Metadata)})
				result.Removed++
			}
			continue
		}
		// This node's own decisions, applied again to what arrives.
		if optedOut[change.LibraryID] || !published[change.LibraryID] || change.Metadata == nil {
			result.Dropped++
			continue
		}
		metadata, err := json.Marshal(change.Metadata)
		if err != nil {
			return result, err
		}
		item := store.RemoteItem{
			SourceNodeID: sourceNodeID, ItemID: change.ItemID, LibraryID: change.LibraryID, ParentID: change.ParentID,
			ItemType: change.ItemType, Revision: change.Revision, Metadata: string(metadata),
		}
		operations = append(operations, store.RemoteOperation{Item: item})
		existing[change.ItemID] = item
		result.Applied++
	}
	if err := destination.items.Apply(ctx, sourceNodeID, operations, page.Next, destination.now()); err != nil {
		return Result{}, err
	}
	if result.Dropped > 0 {
		_ = destination.audit.Record(ctx, sourceNodeID, "catalog.items_dropped", sourceNodeID, map[string]string{
			"node_id": sourceNodeID, "count": strconv.Itoa(result.Dropped),
		})
	}
	return result, nil
}

// OptOut removes every record of a source library this node no longer wants.
func (destination *Destination) OptOut(ctx context.Context, sourceNodeID string, libraryID string) (int, error) {
	return destination.items.RemoveLibrary(ctx, sourceNodeID, libraryID)
}

// OptIn makes the next sync of the source start from the beginning, so the
// library it had been excluding arrives in full.
func (destination *Destination) OptIn(ctx context.Context, sourceNodeID string) error {
	return destination.items.ResetCursor(ctx, sourceNodeID)
}

// canonicalProviders names the strong identities used for a logical work ID,
// in order of preference, under one spelling each (plan-review D6).
var canonicalProviders = []struct{ canonical, jellyfin string }{
	{"tmdb", "Tmdb"}, {"tvdb", "Tvdb"}, {"imdb", "Imdb"},
}

// LogicalWorkID derives the identity a withdrawn item is retained under, from
// its strongest provider identifier. It is empty when there is none, because
// title and year alone never identify a work.
func LogicalWorkID(itemType string, metadata string) string {
	var decoded sourcecatalog.Metadata
	if json.Unmarshal([]byte(metadata), &decoded) != nil {
		return ""
	}
	for _, provider := range canonicalProviders {
		for key, value := range decoded.ProviderIDs {
			if strings.EqualFold(key, provider.jellyfin) && strings.TrimSpace(value) != "" {
				return strings.ToLower(itemType) + ":" + provider.canonical + ":" + strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func peerFingerprint(request *http.Request) (transport.Fingerprint, error) {
	return transport.PeerFingerprint(*request.TLS)
}
