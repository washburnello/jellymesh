package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"jellymesh/internal/catalogsync"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/materialize"
	"jellymesh/internal/membership"
	"jellymesh/internal/policy"
	"jellymesh/internal/relay"
	"jellymesh/internal/replication"
	"jellymesh/internal/store"
)

// maxSidecar bounds a subtitle or image fetched from a source.
const maxSidecar = 16 << 20

// mayConsume reports whether this node may play a source library now: the
// source is a member this node has not blocked, and the library is published
// and not opted out. The relay asks it on every request.
func (n *Node) mayConsume(ctx context.Context, sourceID string, libraryID string) bool {
	runtime, err := n.current()
	if err != nil {
		return false
	}
	if blocked, err := n.peers.IsBlocked(ctx, sourceID); err != nil || blocked {
		return false
	}
	allowed := false
	_ = runtime.inviter.WithPolicy(ctx, func(state *policy.State, roster policy.Roster) error {
		allowed = state.CanConsume(roster, sourceID, libraryID)
		return nil
	})
	return allowed
}

// relayPolicy adapts mayConsume for the relay.
type relayPolicy struct{ node *Node }

func (p relayPolicy) MayPlay(ctx context.Context, sourceID string, libraryID string) error {
	if !p.node.mayConsume(ctx, sourceID, libraryID) {
		return relay.ErrRefused
	}
	return nil
}

// sourceAccess opens media and sidecars at a source over mutual TLS.
type sourceAccess struct{ node *Node }

func (access sourceAccess) path(kind string, parts ...string) (string, error) {
	runtime, err := access.node.current()
	if err != nil {
		return "", err
	}
	escaped := make([]string, len(parts))
	for index, part := range parts {
		escaped[index] = url.PathEscape(part)
	}
	return "/jellymesh/v1/groups/" + url.PathEscape(runtime.id) + "/" + kind + "/" + strings.Join(escaped, "/"), nil
}

func (access sourceAccess) peer(sourceID string) (replication.Peer, error) {
	runtime, err := access.node.current()
	if err != nil {
		return replication.Peer{}, err
	}
	return access.node.peer(runtime, sourceID)
}

// Media satisfies relay.Upstream.
func (access sourceAccess) Media(ctx context.Context, sourceID string, method string, itemID string, header http.Header) (*http.Response, error) {
	peer, err := access.peer(sourceID)
	if err != nil {
		return nil, err
	}
	path, err := access.path("media", itemID)
	if err != nil {
		return nil, err
	}
	return access.node.client.Stream(ctx, peer, method, path, header)
}

func (access sourceAccess) fetch(ctx context.Context, sourceID string, path string) ([]byte, error) {
	peer, err := access.peer(sourceID)
	if err != nil {
		return nil, err
	}
	response, err := access.node.client.Stream(ctx, peer, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the source answered %s", response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, maxSidecar))
}

// Subtitle satisfies materialize.Fetcher.
func (access sourceAccess) Subtitle(ctx context.Context, sourceID string, itemID string, index int) ([]byte, error) {
	path, err := access.path("subtitles", itemID, strconv.Itoa(index))
	if err != nil {
		return nil, err
	}
	return access.fetch(ctx, sourceID, path)
}

// Image satisfies materialize.Fetcher.
func (access sourceAccess) Image(ctx context.Context, sourceID string, itemID string) ([]byte, error) {
	path, err := access.path("images", itemID, "primary")
	if err != nil {
		return nil, err
	}
	return access.fetch(ctx, sourceID, path)
}

// RelayHandler serves the local relay, which must be listened on at
// cfg.RelayListenAddress.
func (n *Node) RelayHandler() (http.Handler, error) {
	allowed, err := relay.ParseAllowed(n.cfg.RelayAllowedClients)
	if err != nil {
		return nil, err
	}
	server := relay.New(allowed, n.materialized, relayPolicy{n}, sourceAccess{n}, n.logger)
	if n.heads != nil {
		server.SetHeadCache(n.heads)
	}
	return server.Handler(), nil
}

// Materialize brings the generated root into line with what this node may
// consume, then tells Jellyfin which collection folders changed. The notice
// is best effort: Jellyfin's scheduled scan is what reliably finds the
// change (assumption A-12).
func (n *Node) Materialize(ctx context.Context) (materialize.Result, error) {
	runtime, err := n.current()
	if err != nil {
		return materialize.Result{}, err
	}
	items, err := n.remote.All(ctx)
	if err != nil {
		return materialize.Result{}, err
	}
	consumable := map[string]bool{}
	var names map[string]string
	_ = runtime.group.View(func(state *grouplog.State) {
		names = map[string]string{}
		for _, member := range state.Members() {
			names[member.NodeID] = member.FriendlyName
		}
	})
	for _, item := range items {
		key := item.SourceNodeID + "/" + item.LibraryID
		if _, seen := consumable[key]; !seen {
			consumable[key] = n.mayConsume(ctx, item.SourceNodeID, item.LibraryID)
		}
	}
	result, err := n.materializer.Reconcile(ctx, materialize.Input{
		Items:       items,
		Consumable:  func(source string, library string) bool { return consumable[source+"/"+library] },
		SourceNames: names,
	})
	if err != nil {
		return result, err
	}
	for _, failure := range result.Failures {
		n.logger.Printf("materialize: %s", failure)
	}
	if len(result.Changed) > 0 && n.jellyfin != nil {
		if err := n.jellyfin.NotifyUpdated(ctx, n.jellyfinPaths(result.Changed), "Modified"); err != nil {
			n.logger.Printf("notify jellyfin: %v", err)
		}
		n.registerJellyfinSecrets()
	}
	if result.Written > 0 || result.Removed > 0 {
		_ = n.audit.Record(ctx, "local", "catalog.materialized", runtime.id, map[string]string{
			"group_id": runtime.id, "count": strconv.Itoa(result.Written), "outcome": fmt.Sprintf("%d removed", result.Removed),
		})
	}
	return result, nil
}

// jellyfinPaths maps generated paths to the paths Jellyfin sees them at.
func (n *Node) jellyfinPaths(paths []string) []string {
	root, err := filepath.Abs(n.cfg.GeneratedRootPath)
	if err != nil {
		return paths
	}
	mapped := make([]string, 0, len(paths))
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		mapped = append(mapped, filepath.ToSlash(filepath.Join(n.cfg.JellyfinGeneratedRoot, relative)))
	}
	return mapped
}

// GeneratedRoot describes one collection folder the operator adds to a
// Jellyfin library.
type GeneratedRoot struct {
	CollectionType string `json:"collection_type"`
	Path           string `json:"path"`
	JellyfinPath   string `json:"jellyfin_path"`
}

// GeneratedRoots lists the collection folders, with where Jellyfin sees them.
func (n *Node) GeneratedRoots() []GeneratedRoot {
	var roots []GeneratedRoot
	for collectionType, path := range n.materializer.Roots() {
		mapped := n.jellyfinPaths([]string{path})
		roots = append(roots, GeneratedRoot{CollectionType: collectionType, Path: path, JellyfinPath: mapped[0]})
	}
	return roots
}

var _ relay.Resolver = (*store.MaterializedRepository)(nil)

// newCatalogServer serves this node's catalog and, when it has a service
// user, its media, under the configured per-destination ceiling.
func (n *Node) newCatalogServer(group *membership.Group, groupID string) *catalogsync.Server {
	server := catalogsync.NewServer(n.source, group, groupID, n.peers)
	if n.jellyfin != nil {
		server.SetMedia(n.jellyfin, n.cfg.UploadCeiling)
	}
	return server
}
