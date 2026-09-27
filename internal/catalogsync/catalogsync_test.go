package catalogsync

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/federation"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/jellyfin"
	"jellymesh/internal/jellyfin/jellyfintest"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/replication"
	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
	"jellymesh/internal/transport"
)

var now = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

type side struct {
	identity *node.Identity
	database *store.DB
	group    *membership.Group
}

func newIdentity(t *testing.T, name string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	identity, err := node.LoadOrCreate(filepath.Join(directory, "k"), filepath.Join(directory, "c"), name+".example.org")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return identity
}

func openSide(t *testing.T, name string) *side {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "state", "jellymesh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return &side{identity: newIdentity(t, name), database: database}
}

// world is cedar, a source with a fake Jellyfin, and walnut, a destination,
// in one group, with cedar serving its catalog over mutual TLS.
type world struct {
	fake        *jellyfintest.Server
	cedar       *side
	walnut      *side
	catalog     *sourcecatalog.Catalog
	destination *Destination
	source      replication.Peer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{cedar: openSide(t, "cedar"), walnut: openSide(t, "walnut")}

	group, err := membership.Found(ctx, store.NewGroupLogRepository(w.cedar.database), store.NewPeerRepository(w.cedar.database), w.cedar.identity, "group-1", "cedar", "Cedar", "", now)
	if err != nil {
		t.Fatalf("found: %v", err)
	}
	proposal, _ := grouplog.NewProposal(w.cedar.identity, "group-1", "cedar", grouplog.KindAdmission,
		grouplog.AdmissionBody{MemberID: "walnut", MemberKey: w.walnut.identity.PublicKey(), InvitationID: "i-1", InviterID: "cedar"}, now)
	if _, err := group.Sequence(ctx, w.cedar.identity, proposal, now); err != nil {
		t.Fatalf("admit walnut: %v", err)
	}
	w.cedar.group = group
	events := group.EventsAfter(0)
	if w.walnut.group, err = membership.Join(ctx, store.NewGroupLogRepository(w.walnut.database), store.NewPeerRepository(w.walnut.database), events, events[0].Hash()); err != nil {
		t.Fatalf("join: %v", err)
	}

	w.fake = jellyfintest.New()
	t.Cleanup(w.fake.Close)
	w.fake.AddLibrary("lib-movies", "Movies", "movies")
	w.fake.AddLibrary("lib-docs", "Documentaries", "movies")
	w.fake.AddUser("jellymesh", "service-pw", false, "lib-movies", "lib-docs")
	for index := 0; index < 5; index++ {
		w.fake.AddItem("lib-movies", jellyfin.Item{ID: fmt.Sprintf("movie-%d", index), Name: fmt.Sprintf("Movie %d", index), Type: "Movie",
			Path: fmt.Sprintf("/media/movies/%d.mkv", index), ProviderIDs: map[string]string{"Tmdb": fmt.Sprint(500 + index)}})
	}
	w.fake.AddItem("lib-docs", jellyfin.Item{ID: "doc-1", Name: "Oceans", Type: "Movie", Path: "/media/docs/oceans.mkv"})
	client := jellyfin.New(w.fake.URL, "jellymesh", "service-pw", "cedar")
	w.catalog = sourcecatalog.New(client, store.NewSourceCatalogRepository(w.cedar.database), nil, nil)
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish movies: %v", err)
	}
	if err := w.catalog.Publish(ctx, "lib-docs", []string{"/media/docs"}); err != nil {
		t.Fatalf("publish docs: %v", err)
	}

	replicationServer := replication.NewServer(w.cedar.identity)
	replicationServer.Add("group-1", group)
	catalogServer := NewServer(w.catalog, group, "group-1", store.NewPeerRepository(w.cedar.database))
	w.source = serve(t, w.cedar.identity, federation.Handler(replicationServer, replicationServer, catalogServer))

	w.destination = NewDestination(
		store.NewRemoteCatalogRepository(w.walnut.database, 7*24*time.Hour),
		store.NewSyncRepository(w.walnut.database),
		replication.NewClient(w.walnut.identity, w.walnut.group), "group-1", nil)
	w.destination.SetClock(func() time.Time { return now })
	return w
}

func serve(t *testing.T, identity *node.Identity, handler http.Handler) replication.Peer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(tls.NewListener(listener, federation.TLSConfig(identity)))
	t.Cleanup(func() { server.Close() })
	return replication.Peer{Address: listener.Addr().String(), Fingerprint: identity.Fingerprint()}
}

func (w *world) sync(t *testing.T, optedOut ...string) Result {
	t.Helper()
	excluded := map[string]bool{}
	for _, library := range optedOut {
		excluded[library] = true
	}
	result, err := w.destination.Sync(context.Background(), w.source, "cedar", excluded)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return result
}

func (w *world) held(t *testing.T, library string) map[string]store.RemoteItem {
	t.Helper()
	items, err := store.NewRemoteCatalogRepository(w.walnut.database, 0).Items(context.Background(), "cedar", library)
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	byID := map[string]store.RemoteItem{}
	for _, item := range items {
		byID[item.ItemID] = item
	}
	return byID
}

// C-CA-1: a destination syncs incrementally and idempotently over the wire,
// in pages, and applies a newer revision.
func TestADestinationSyncsASourceIncrementally(t *testing.T) {
	w := newWorld(t)
	w.destination.SetPageSize(2)
	first := w.sync(t)
	if first.Applied != 6 || len(first.Libraries) != 2 {
		t.Fatalf("first sync: %+v", first)
	}
	if again := w.sync(t); again.Applied != 0 {
		t.Fatalf("an unchanged source should apply nothing: %+v", again)
	}

	w.fake.Update("movie-1", "Movie 1 (Remastered)")
	if err := w.catalog.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	third := w.sync(t)
	held := w.held(t, "")
	if third.Applied != 1 || held["movie-1"].Revision != 2 {
		t.Fatalf("after an edit: %+v, revision %d", third, held["movie-1"].Revision)
	}
	if len(held) != 6 {
		t.Fatalf("the destination holds %d items, want 6", len(held))
	}
}

// C-CA-2: a tombstone removes the item at once and retains its metadata for
// the grace period; an item that returns is restored and no longer retained.
func TestATombstoneRemovesAndRetains(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.sync(t)

	w.fake.Delete("movie-2")
	w.catalog.Refresh(ctx)
	if result := w.sync(t); result.Removed != 1 {
		t.Fatalf("sync after deletion: %+v", result)
	}
	if _, present := w.held(t, "")["movie-2"]; present {
		t.Fatal("a tombstoned item must be removed at once")
	}
	var metadata, logical, expires string
	err := w.walnut.database.SQL().QueryRowContext(ctx,
		`SELECT metadata, logical_work_id, expires_at FROM retention WHERE source_node_id = 'cedar' AND source_item_id = 'movie-2'`,
	).Scan(&metadata, &logical, &expires)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if logical != "movie:tmdb:502" || metadata == "" || expires != store.FormatTime(now.Add(7*24*time.Hour)) {
		t.Fatalf("retention = %q, %q, %q", logical, metadata, expires)
	}

	w.fake.AddItem("lib-movies", jellyfin.Item{ID: "movie-2", Name: "Movie 2", Type: "Movie", Path: "/media/movies/2.mkv", ProviderIDs: map[string]string{"Tmdb": "502"}})
	w.catalog.Refresh(ctx)
	w.sync(t)
	if _, present := w.held(t, "")["movie-2"]; !present {
		t.Fatal("a returning item should be live again")
	}
	var count int
	w.walnut.database.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM retention WHERE source_item_id = 'movie-2'`).Scan(&count)
	if count != 0 {
		t.Fatal("a returning item is no longer retained")
	}
}

// Unpublishing at the source removes the library at the destination.
func TestUnpublishingRemovesTheLibraryDownstream(t *testing.T) {
	w := newWorld(t)
	w.sync(t)
	if err := w.catalog.Unpublish(context.Background(), "lib-docs"); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	result := w.sync(t)
	if result.Removed != 1 || len(w.held(t, "lib-docs")) != 0 || len(result.Libraries) != 1 {
		t.Fatalf("after unpublishing: %+v", result)
	}
}

// C-PR-2, C-CA-5: an opted-out library is not sent, is removed locally on
// opting out, and arrives in full on opting back in.
func TestOptingOutAndBackIn(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.sync(t)
	if removed, err := w.destination.OptOut(ctx, "cedar", "lib-movies"); err != nil || removed != 5 {
		t.Fatalf("opt out removed %d, %v", removed, err)
	}
	w.fake.Update("movie-1", "Changed while opted out")
	w.catalog.Refresh(ctx)
	if result := w.sync(t, "lib-movies"); result.Applied != 0 || result.Dropped != 0 || len(w.held(t, "lib-movies")) != 0 {
		t.Fatalf("an opted-out library must not even be sent: %+v", result)
	}

	if err := w.destination.OptIn(ctx, "cedar"); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	result := w.sync(t)
	if len(w.held(t, "lib-movies")) != 5 || result.Applied != 5 {
		t.Fatalf("opting back in should restore the library: %+v", result)
	}
}

// C-CA-5: a destination drops what a misbehaving source sends from an
// opted-out library, from a library it does not list as published, and at a
// stale revision.
func TestTheDestinationEnforcesItsOwnRules(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.sync(t)
	name := "Injected"
	page := sourcecatalog.ChangePage{Next: 9000, Changes: []sourcecatalog.Change{
		{Sequence: 8998, LibraryID: "lib-docs", ItemID: "doc-2", ItemType: "Movie", Revision: 1, Metadata: &sourcecatalog.Metadata{Name: name}},
		{Sequence: 8999, LibraryID: "lib-family", ItemID: "family-1", ItemType: "Movie", Revision: 1, Metadata: &sourcecatalog.Metadata{Name: name}},
		{Sequence: 9000, LibraryID: "lib-movies", ItemID: "movie-1", ItemType: "Movie", Revision: 1, Metadata: &sourcecatalog.Metadata{Name: name}},
	}}
	result, err := w.destination.apply(ctx, "cedar", page, map[string]bool{"lib-movies": true, "lib-docs": true}, map[string]bool{"lib-docs": true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Dropped != 2 || result.Stale != 1 || result.Applied != 0 {
		t.Fatalf("result = %+v, want two dropped and one stale", result)
	}
	held := w.held(t, "")
	if _, present := held["doc-2"]; present {
		t.Fatal("an opted-out library's item must be dropped")
	}
	if _, present := held["family-1"]; present {
		t.Fatal("an item from a library the source does not publish must be dropped")
	}
	if held["movie-1"].Revision != 1 || held["movie-1"].Metadata == "" {
		t.Fatal("a stale change must not overwrite the stored item")
	}
}

// C-CA-5: a blocked destination and a non-member get nothing.
func TestABlockedDestinationOrANonMemberGetsNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := store.NewPeerRepository(w.cedar.database).SetBlocked(ctx, "walnut", true); err != nil {
		t.Fatalf("block: %v", err)
	}
	if _, err := w.destination.Sync(ctx, w.source, "cedar", nil); err == nil {
		t.Fatal("a blocked destination must get nothing")
	}
	if len(w.held(t, "")) != 0 {
		t.Fatal("nothing should have been stored")
	}
	state, _, _ := store.NewSyncRepository(w.walnut.database).Get(ctx, "cedar")
	if state.ConsecutiveFailures != 1 {
		t.Fatalf("the failure should be recorded against the source: %+v", state)
	}

	outsider := newIdentity(t, "outsider")
	client := replication.NewClient(outsider, transport.NewMemoryTrustStore(w.cedar.identity.Fingerprint()))
	var libraries []sourcecatalog.PublishedLibrary
	if err := client.GetJSON(ctx, w.source, "/jellymesh/v1/groups/group-1/catalog/libraries", &libraries); !errors.Is(err, replication.ErrNotServed) {
		t.Fatalf("a non-member: error = %v, want ErrNotServed", err)
	}
}

// C-ST-5: a sync that fails part way keeps the cursor and the records it had.
func TestAFailedSyncKeepsKnownGoodState(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.sync(t)
	before := w.held(t, "")
	state, _, _ := store.NewSyncRepository(w.walnut.database).Get(ctx, "cedar")

	unreachable := replication.Peer{Address: "127.0.0.1:1", Fingerprint: w.cedar.identity.Fingerprint()}
	if _, err := w.destination.Sync(ctx, unreachable, "cedar", nil); err == nil {
		t.Fatal("expected a failure")
	}
	after, _, _ := store.NewSyncRepository(w.walnut.database).Get(ctx, "cedar")
	if after.Cursor != state.Cursor || len(w.held(t, "")) != len(before) {
		t.Fatalf("a failed sync changed known-good state: cursor %q -> %q", state.Cursor, after.Cursor)
	}
}

func TestLogicalWorkIDUsesTheStrongestProviderUnderOneSpelling(t *testing.T) {
	cases := map[string]string{
		`{"provider_ids":{"Imdb":"tt1","Tmdb":"7"}}`: "movie:tmdb:7",
		`{"provider_ids":{"imdb":"tt1"}}`:            "movie:imdb:tt1",
		`{"name":"Untitled"}`:                        "",
	}
	for metadata, want := range cases {
		if got := LogicalWorkID("Movie", metadata); got != want {
			t.Errorf("LogicalWorkID(%s) = %q, want %q", metadata, got, want)
		}
	}
}

// C-CA-5: the catalog routes authorize each request themselves, so they stay
// closed to a non-member or a blocked member even if mounted behind a broader
// gate than the group's own trust.
func TestTheCatalogRoutesAuthorizeThemselves(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	server := NewServer(w.catalog, w.cedar.group, "group-1", store.NewPeerRepository(w.cedar.database))
	mux := http.NewServeMux()
	server.Register(nil, mux)
	call := func(identity *node.Identity) int {
		request := httptest.NewRequest(http.MethodGet, "/jellymesh/v1/groups/group-1/catalog/libraries", nil)
		request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{identity.Certificate()}}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := call(w.walnut.identity); code != http.StatusOK {
		t.Fatalf("a member: status %d", code)
	}
	if code := call(newIdentity(t, "outsider")); code != http.StatusNotFound {
		t.Fatalf("a non-member: status %d, want 404", code)
	}
	store.NewPeerRepository(w.cedar.database).SetBlocked(ctx, "walnut", true)
	if code := call(w.walnut.identity); code != http.StatusNotFound {
		t.Fatalf("a blocked member: status %d, want 404", code)
	}
}

// A source that moves the cursor backwards is refused rather than followed.
func TestASourceCannotMoveTheCursorBackwards(t *testing.T) {
	w := newWorld(t)
	w.sync(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jellymesh/v1/groups/group-1/catalog/libraries", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, []sourcecatalog.PublishedLibrary{{LibraryID: "lib-movies"}})
	})
	mux.HandleFunc("GET /jellymesh/v1/groups/group-1/catalog/changes", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, sourcecatalog.ChangePage{Next: 1})
	})
	hostile := serve(t, w.cedar.identity, mux)
	if _, err := w.destination.Sync(context.Background(), hostile, "cedar", nil); err == nil {
		t.Fatal("a cursor moving backwards must be refused")
	}
}
