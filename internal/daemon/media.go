package daemon

import (
	"context"
	"errors"
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

// Media satisfies relay.Upstream. It takes a direct path to the source when
// one is open (A-16), as the same request to the same handler over HTTP/3,
// and TCP otherwise. If the direct path fails before answering, the request
// goes over TCP; if it fails part-way through a body, the rest is fetched
// over TCP from where it stopped, so the viewer sees no error (C-NT-6).
func (access sourceAccess) Media(ctx context.Context, sourceID string, method string, itemID string, header http.Header) (*http.Response, error) {
	peer, err := access.peer(sourceID)
	if err != nil {
		return nil, err
	}
	path, err := access.path("media", itemID)
	if err != nil {
		return nil, err
	}
	tcp := func(header http.Header) (*http.Response, error) {
		return access.node.client.Stream(ctx, peer, method, path, header)
	}
	if client, ok := access.node.direct.client(sourceID); ok {
		request, err := http.NewRequestWithContext(ctx, method, "https://direct"+path, nil)
		if err == nil {
			for key, values := range header {
				request.Header[key] = append([]string(nil), values...)
			}
			response, err := client.Do(request)
			if err == nil {
				if method == http.MethodGet {
					response.Body = resumeOverTCP(ctx, response, func(from int64, end int64) (io.ReadCloser, error) {
						return reopenFrom(tcp, header, from, end)
					}, func(err error) { access.node.direct.failed(sourceID, err) })
				}
				return response, nil
			}
			access.node.direct.failed(sourceID, err)
		}
	}
	return tcp(header)
}

// reopenFrom asks for the rest of a body, from offset from to end (or to
// the end of the file when end is negative), and accepts only an answer
// that starts exactly there.
func reopenFrom(open func(http.Header) (*http.Response, error), header http.Header, from int64, end int64) (io.ReadCloser, error) {
	resumed := header.Clone()
	if resumed == nil {
		resumed = http.Header{}
	}
	if end >= 0 {
		resumed.Set("Range", fmt.Sprintf("bytes=%d-%d", from, end))
	} else {
		resumed.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	response, err := open(resumed)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusPartialContent || !strings.HasPrefix(response.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", from)) {
		response.Body.Close()
		return nil, fmt.Errorf("the source did not resume at %d: %s %q", from, response.Status, response.Header.Get("Content-Range"))
	}
	return response.Body, nil
}

// resumeOverTCP wraps a direct response's body so that a failure part-way
// through is continued over TCP, once.
func resumeOverTCP(ctx context.Context, response *http.Response, reopen func(from int64, end int64) (io.ReadCloser, error), failed func(error)) io.ReadCloser {
	body := &resumingBody{ctx: ctx, body: response.Body, reopen: reopen, failed: failed, end: -1}
	switch response.StatusCode {
	case http.StatusPartialContent:
		var total int64
		if _, err := fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/%d", &body.next, &body.end, &total); err != nil {
			fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/*", &body.next, &body.end)
		}
	case http.StatusOK:
		if response.ContentLength >= 0 {
			body.end = response.ContentLength - 1
		}
	default:
		body.resumed = true // an error answer is passed through as it is
	}
	return body
}

type resumingBody struct {
	ctx     context.Context
	body    io.ReadCloser
	reopen  func(from int64, end int64) (io.ReadCloser, error)
	failed  func(error)
	next    int64 // offset of the next byte in the file
	end     int64 // last offset expected, or -1 if unknown
	resumed bool
}

func (body *resumingBody) Read(p []byte) (int, error) {
	n, err := body.body.Read(p)
	body.next += int64(n)
	if err == nil || errors.Is(err, io.EOF) || body.resumed || body.ctx.Err() != nil {
		return n, err
	}
	body.resumed = true
	body.failed(err)
	body.body.Close()
	rest, reopenErr := body.reopen(body.next, body.end)
	if reopenErr != nil {
		return n, err
	}
	body.body = rest
	if n > 0 {
		return n, nil
	}
	return body.Read(p)
}

func (body *resumingBody) Close() error { return body.body.Close() }

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
// Size finds a film's size with a bodiless request, for a source whose
// catalog does not carry it.
func (access sourceAccess) Size(ctx context.Context, sourceID string, itemID string) (int64, error) {
	response, err := access.Media(ctx, sourceID, http.MethodHead, itemID, http.Header{})
	if err != nil {
		return 0, err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength <= 0 {
		return 0, fmt.Errorf("the source answered %d with no length", response.StatusCode)
	}
	return response.ContentLength, nil
}

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

// ReadHandler serves the mount film bytes over the local read socket, or is
// nil unless films are presented through the mount (A-17).
func (n *Node) ReadHandler() http.Handler {
	if n.reads == nil {
		return nil
	}
	return n.reads.Handler()
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
