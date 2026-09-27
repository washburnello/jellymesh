package daemon

import (
	"context"
	"errors"
	"fmt"

	"jellymesh/internal/catalogsync"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/jellyfin"
	"jellymesh/internal/policy"
	"jellymesh/internal/replication"
	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
)

// ErrNoServiceUser means the node has no Jellyfin service user configured, so
// it can consume other members' catalogs but cannot publish its own.
var ErrNoServiceUser = errors.New("no Jellyfin service user is configured; set JELLYMESH_JELLYFIN_USER and JELLYMESH_JELLYFIN_PASSWORD")

// LibraryView is a library the service user can see, as the operator sees it.
type LibraryView struct {
	jellyfin.Library
	Protected bool                     `json:"protected"`
	Published *store.SourcePublication `json:"publication,omitempty"`
}

// registerJellyfinSecrets adds the service user's current token to the audit
// redactor. The token is issued on first use and may be reissued, so this
// runs after every operation that talks to Jellyfin.
func (n *Node) registerJellyfinSecrets() {
	if n.jellyfin != nil {
		n.redactor.Register(n.jellyfin.Credentials()...)
	}
}

// Libraries lists what the service user can see, marking protected and
// published libraries.
func (n *Node) Libraries(ctx context.Context) ([]LibraryView, error) {
	if n.source == nil {
		return nil, ErrNoServiceUser
	}
	defer n.registerJellyfinSecrets()
	libraries, err := n.jellyfin.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	publications, err := n.source.Publications(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.SourcePublication{}
	for _, publication := range publications {
		byID[publication.LibraryID] = publication
	}
	protected := map[string]bool{}
	for _, id := range n.cfg.ProtectedLibraries {
		protected[id] = true
	}
	views := make([]LibraryView, 0, len(libraries))
	for _, library := range libraries {
		view := LibraryView{Library: library, Protected: protected[library.ID]}
		if publication, ok := byID[library.ID]; ok {
			view.Published = &publication
		}
		views = append(views, view)
	}
	return views, nil
}

// Publish offers one of this node's libraries with its declared roots.
func (n *Node) Publish(ctx context.Context, libraryID string, roots []string) error {
	if n.source == nil {
		return ErrNoServiceUser
	}
	defer n.registerJellyfinSecrets()
	return n.source.Publish(ctx, libraryID, roots)
}

// Unpublish withdraws one of this node's libraries.
func (n *Node) Unpublish(ctx context.Context, libraryID string) error {
	if n.source == nil {
		return ErrNoServiceUser
	}
	return n.source.Unpublish(ctx, libraryID)
}

// publishedLibraries returns this node's live publications as the admission
// rule counts them.
func (n *Node) publishedLibraries(ctx context.Context) ([]policy.Library, error) {
	if n.source == nil {
		return nil, nil
	}
	published, err := n.source.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	libraries := make([]policy.Library, 0, len(published))
	for _, library := range published {
		libraries = append(libraries, policy.Library{ID: library.LibraryID, Name: library.Name, CollectionType: library.CollectionType})
	}
	return libraries, nil
}

type memberPeer struct {
	nodeID string
	peer   replication.Peer
}

func (n *Node) otherMemberRecords(runtime *groupRuntime) []memberPeer {
	var members []memberPeer
	_ = runtime.group.View(func(state *grouplog.State) {
		for _, member := range state.Members() {
			if member.NodeID != n.nodeID && member.PublicHostname != "" {
				members = append(members, memberPeer{nodeID: member.NodeID,
					peer: replication.Peer{Address: member.PublicHostname, Fingerprint: member.Fingerprint}})
			}
		}
	})
	return members
}

// CatalogResult reports one catalog pass.
type CatalogResult struct {
	Sources map[string]catalogsync.Result `json:"sources"`
	Failed  map[string]string             `json:"failed,omitempty"`
}

// SyncCatalogOnce refreshes this node's own catalog from Jellyfin and pulls
// every other member's catalog. A member this node has blocked is skipped:
// a block cuts media in both directions. Records of a node that is no longer
// a member are removed.
func (n *Node) SyncCatalogOnce(ctx context.Context) (CatalogResult, error) {
	result := CatalogResult{Sources: map[string]catalogsync.Result{}, Failed: map[string]string{}}
	if n.source != nil {
		if err := n.source.Refresh(ctx); err != nil {
			n.logger.Printf("refresh source catalog: %v", err)
		}
		n.registerJellyfinSecrets()
	}
	runtime, err := n.current()
	if err != nil {
		return result, nil
	}

	for _, member := range n.otherMemberRecords(runtime) {
		if blocked, err := n.peers.IsBlocked(ctx, member.nodeID); err != nil || blocked {
			continue
		}
		optedOut := map[string]bool{}
		_ = runtime.inviter.WithPolicy(ctx, func(state *policy.State, _ policy.Roster) error {
			for library := range state.OptOuts[member.nodeID] {
				optedOut[library] = true
			}
			return nil
		})
		synced, err := runtime.destination.Sync(ctx, member.peer, member.nodeID, optedOut)
		if err != nil {
			result.Failed[member.nodeID] = err.Error()
			n.logger.Printf("catalog sync from %s: %v", member.nodeID, err)
			continue
		}
		result.Sources[member.nodeID] = synced
		if err := n.recordPublications(ctx, runtime, member.nodeID, synced.Libraries); err != nil {
			n.logger.Printf("record publications of %s: %v", member.nodeID, err)
		}
	}

	sources, err := n.remote.Sources(ctx)
	if err != nil {
		return result, err
	}
	for _, source := range sources {
		var member bool
		_ = runtime.group.View(func(state *grouplog.State) { member = state.IsMember(source) })
		if !member {
			if err := n.remote.RemoveSource(ctx, source); err != nil {
				return result, err
			}
			_ = n.audit.Record(ctx, "local", "catalog.source_removed", source, map[string]string{"node_id": source})
		}
	}
	err = runtime.inviter.WithPolicy(ctx, func(state *policy.State, roster policy.Roster) error {
		state.Reconcile(roster)
		return nil
	})
	return result, err
}

// recordPublications replaces what this node knows a member publishes with
// what that member just reported, for the opt-out interface and CanConsume.
func (n *Node) recordPublications(ctx context.Context, runtime *groupRuntime, sourceID string, libraries []sourcecatalog.PublishedLibrary) error {
	return runtime.inviter.WithPolicy(ctx, func(state *policy.State, roster policy.Roster) error {
		listed := map[string]bool{}
		for _, library := range libraries {
			listed[library.LibraryID] = true
			if err := state.Publish(roster, policy.Publication{
				GroupID: runtime.id, SourceNodeID: sourceID,
				Library: policy.Library{ID: library.LibraryID, Name: library.Name, CollectionType: library.CollectionType},
			}); err != nil {
				return err
			}
		}
		for _, publication := range state.Published {
			if publication.SourceNodeID == sourceID && !listed[publication.Library.ID] {
				_ = state.Unpublish(sourceID, publication.Library.ID)
			}
		}
		return nil
	})
}

// SetOptOut opts this node out of, or back in to, one library of a member.
func (n *Node) SetOptOut(ctx context.Context, sourceID string, libraryID string, optedOut bool) error {
	runtime, err := n.current()
	if err != nil {
		return err
	}
	err = runtime.inviter.WithPolicy(ctx, func(state *policy.State, _ policy.Roster) error {
		return state.SetOptOut(sourceID, libraryID, optedOut)
	})
	if err != nil {
		return err
	}
	action := "catalog.opted_in"
	if optedOut {
		action = "catalog.opted_out"
		if _, err := runtime.destination.OptOut(ctx, sourceID, libraryID); err != nil {
			return err
		}
	} else if err := runtime.destination.OptIn(ctx, sourceID); err != nil {
		return err
	}
	_ = n.audit.Record(ctx, "local", action, sourceID, map[string]string{"node_id": sourceID, "library_id": libraryID})
	return nil
}

// RemoteLibrary is one library another member publishes, as this node holds
// it.
type RemoteLibrary struct {
	SourceID       string `json:"source_id"`
	LibraryID      string `json:"library_id"`
	Name           string `json:"name"`
	CollectionType string `json:"collection_type"`
	OptedOut       bool   `json:"opted_out"`
	ItemsHeld      int    `json:"items_held"`
}

// Remote lists what other members publish and how much of it this node holds.
func (n *Node) Remote(ctx context.Context) ([]RemoteLibrary, error) {
	runtime, err := n.current()
	if err != nil {
		return nil, err
	}
	var libraries []RemoteLibrary
	err = runtime.inviter.WithPolicy(ctx, func(state *policy.State, _ policy.Roster) error {
		for _, publication := range state.Published {
			if publication.SourceNodeID == n.nodeID {
				continue
			}
			libraries = append(libraries, RemoteLibrary{
				SourceID: publication.SourceNodeID, LibraryID: publication.Library.ID, Name: publication.Library.Name,
				CollectionType: publication.Library.CollectionType,
				OptedOut:       state.IsOptedOut(publication.SourceNodeID, publication.Library.ID),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for index := range libraries {
		items, err := n.remote.Items(ctx, libraries[index].SourceID, libraries[index].LibraryID)
		if err != nil {
			return nil, err
		}
		libraries[index].ItemsHeld = len(items)
	}
	return libraries, nil
}

var errLibrariesRequired = fmt.Errorf("%w: publish at least one library before joining, or name one", policy.ErrInsufficientPublications)
