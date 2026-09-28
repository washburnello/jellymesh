package sourcecatalog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"jellymesh/internal/jellyfin"
	"jellymesh/internal/jellyfin/jellyfintest"
	"jellymesh/internal/store"
)

type world struct {
	fake    *jellyfintest.Server
	client  *jellyfin.Client
	repo    *store.SourceCatalogRepository
	catalog *Catalog
}

func intPointer(value int) *int { return &value }

// newWorld is a Jellyfin with Movies and Shows visible to the service user,
// and Family Movies, which it can also see but which is protected.
func newWorld(t *testing.T) *world {
	t.Helper()
	fake := jellyfintest.New()
	t.Cleanup(fake.Close)
	fake.AddLibrary("lib-movies", "Movies", "movies")
	fake.AddLibrary("lib-shows", "Shows", "tvshows")
	fake.AddLibrary("lib-family", "Family Movies", "movies")
	fake.AddLibrary("lib-private", "Private", "movies")
	fake.AddUser("jellymesh", "pw-service-user", false, "lib-movies", "lib-shows", "lib-family")
	for index := 0; index < 5; index++ {
		fake.AddItem("lib-movies", jellyfin.Item{
			ID: fmt.Sprintf("movie-%d", index), Name: fmt.Sprintf("Movie %d", index), Type: "Movie",
			Path: fmt.Sprintf("/media/movies/Movie %d/movie.mkv", index), ProviderIDs: map[string]string{"Tmdb": fmt.Sprint(100 + index)},
		})
	}
	fake.AddItem("lib-shows", jellyfin.Item{ID: "ep-1", Name: "Pilot", Type: "Episode", SeriesID: "series-1", SeasonID: "season-1", IndexNumber: intPointer(1), ParentIndexNumber: intPointer(1), Path: "/media/tv/Show/Season 1/s01e01.mkv"})
	fake.AddItem("lib-shows", jellyfin.Item{ID: "season-1", Name: "Season 1", Type: "Season", SeriesID: "series-1", IndexNumber: intPointer(1)})
	fake.AddItem("lib-shows", jellyfin.Item{ID: "series-1", Name: "Show", Type: "Series", Path: "/media/tv/Show"})
	fake.AddItem("lib-family", jellyfin.Item{ID: "family-1", Name: "Birthday", Type: "Movie", Path: "/media/family/birthday.mkv"})
	fake.AddItem("lib-private", jellyfin.Item{ID: "private-1", Name: "Secret", Type: "Movie", Path: "/media/private/secret.mkv"})

	database, err := store.Open(filepath.Join(t.TempDir(), "state", "jellymesh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	client := jellyfin.New(fake.URL, "jellymesh", "pw-service-user", "node-1")
	repo := store.NewSourceCatalogRepository(database)
	return &world{fake: fake, client: client, repo: repo, catalog: New(client, repo, []string{"lib-family"}, nil)}
}

func (w *world) changes(t *testing.T, after uint64, exclude ...string) ChangePage {
	t.Helper()
	excluded := map[string]bool{}
	for _, library := range exclude {
		excluded[library] = true
	}
	page, err := w.catalog.Changes(context.Background(), after, 1000, excluded)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	return page
}

func ids(page ChangePage) map[string]Change {
	byID := map[string]Change{}
	for _, change := range page.Changes {
		byID[change.ItemID] = change
	}
	return byID
}

// C-PR-1: a protected library can never be published, even though the
// service user can see it, and nothing of it is ever served.
func TestAProtectedLibraryIsNeverExposed(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.catalog.Publish(ctx, "lib-family", []string{"/media/family"}); !errors.Is(err, ErrProtected) {
		t.Fatalf("publishing a protected library: error = %v, want ErrProtected", err)
	}
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish movies: %v", err)
	}
	for _, change := range w.changes(t, 0).Changes {
		if change.LibraryID == "lib-family" {
			t.Fatal("a protected library's item appeared in the change stream")
		}
	}
	if _, err := w.catalog.Authorize(ctx, "family-1"); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("authorizing a protected item by a guessed ID: error = %v, want ErrNotPublished", err)
	}
	libraries, _ := w.catalog.Libraries(ctx)
	for _, library := range libraries {
		if library.LibraryID == "lib-family" {
			t.Fatal("a protected library was listed")
		}
	}
}

// C-PR-1: a library that becomes protected after publication is withdrawn on
// the next refresh.
func TestALibraryProtectedAfterPublicationIsWithdrawn(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	before := w.changes(t, 0)
	stricter := New(w.client, w.repo, []string{"lib-family", "lib-movies"}, nil)
	// Protection applies at once, before the refresh that withdraws the items.
	if early, _ := stricter.Changes(ctx, 0, 1000, nil); len(early.Changes) != 0 {
		t.Fatal("a protected library's live items must never be sent, even before the next refresh")
	}
	if _, err := stricter.Authorize(ctx, "movie-1"); !errors.Is(err, ErrNotPublished) {
		t.Fatal("a protected library's items must be refused at once")
	}
	if libraries, _ := stricter.Libraries(ctx); len(libraries) != 0 {
		t.Fatal("a protected library must not be listed, even before the next refresh")
	}
	if err := stricter.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// Destinations that received these items while they were published must
	// learn they are withdrawn: the change stream carries only tombstones.
	page, err := stricter.Changes(ctx, before.Next, 1000, nil)
	if err != nil || len(page.Changes) != 5 {
		t.Fatalf("withdrawal: %+v, %v", page, err)
	}
	for _, change := range page.Changes {
		if !change.Tombstone || change.Metadata != nil {
			t.Fatalf("a now-protected library may only produce bare tombstones: %+v", change)
		}
	}
	if live, _ := stricter.Changes(ctx, 0, 1000, nil); len(live.Changes) != 5 {
		t.Fatalf("from the start, only the 5 tombstones remain visible, got %d", len(live.Changes))
	}
	if _, err := stricter.Authorize(ctx, "movie-1"); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("authorize after protection: %v", err)
	}
	publications, _ := w.repo.Publications(ctx)
	if len(publications) != 1 || !publications[0].Paused {
		t.Fatalf("publication should be paused: %+v", publications)
	}
}

// A-8: a library the service user cannot see cannot be published.
func TestALibraryTheServiceUserCannotSeeCannotBePublished(t *testing.T) {
	w := newWorld(t)
	if err := w.catalog.Publish(context.Background(), "lib-private", []string{"/media/private"}); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("error = %v, want ErrNotVisible", err)
	}
}

// C-CA-3: publication requires every item under the declared roots, and a
// root is absolute and narrower than the filesystem root.
func TestPublicationRequiresEveryItemUnderTheRoots(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, roots := range [][]string{nil, {"media/movies"}, {"/"}} {
		if err := w.catalog.Publish(ctx, "lib-movies", roots); !errors.Is(err, ErrNoRoots) {
			t.Errorf("roots %v: error = %v, want ErrNoRoots", roots, err)
		}
	}
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/mov"}); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("a root that is only a string prefix: error = %v, want ErrOutsideRoots", err)
	}
	if err := w.catalog.Publish(ctx, "lib-shows", []string{"/media/tv"}); err != nil {
		t.Fatalf("a season without a folder must not block publication: %v", err)
	}
}

// C-CA-3: an item that appears outside the roots pauses and withdraws the
// whole publication until the operator confirms new roots.
func TestAnItemOutsideTheRootsPausesThePublication(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	before := w.changes(t, 0)

	// Someone adds a path to the library in Jellyfin.
	w.fake.AddItem("lib-movies", jellyfin.Item{ID: "movie-new", Name: "Surprise", Type: "Movie", Path: "/media/home-videos/surprise.mkv"})
	w.catalog.Refresh(ctx)

	after := w.changes(t, before.Next)
	if len(after.Changes) != 5 {
		t.Fatalf("pausing should tombstone all 5 items, got %d changes", len(after.Changes))
	}
	for _, change := range after.Changes {
		if !change.Tombstone || change.ItemID == "movie-new" {
			t.Fatalf("unexpected change while paused: %+v", change)
		}
	}
	if libraries, _ := w.catalog.Libraries(ctx); len(libraries) != 0 {
		t.Fatal("a paused library must not be listed")
	}
	if _, err := w.catalog.Authorize(ctx, "movie-1"); !errors.Is(err, ErrNotPublished) {
		t.Fatal("a paused library's items must not be authorized")
	}

	// The operator confirms the new root.
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies", "/media/home-videos"}); err != nil {
		t.Fatalf("republish: %v", err)
	}
	restored := ids(w.changes(t, after.Next))
	if len(restored) != 6 || restored["movie-1"].Tombstone || restored["movie-1"].Revision <= 2 {
		t.Fatalf("republishing should restore every item with a newer revision: %+v", restored["movie-1"])
	}
}

// C-CA-1: changes are incremental, carry revisions, and an unchanged refresh
// produces nothing new. Nothing of the source's filesystem is sent.
func TestChangesAreIncrementalAndRevisioned(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	first := w.changes(t, 0)
	if len(first.Changes) != 5 {
		t.Fatalf("first page: %d changes, want 5", len(first.Changes))
	}
	for _, change := range first.Changes {
		if change.Revision != 1 || change.Metadata == nil || change.Metadata.ProviderIDs["Tmdb"] == "" {
			t.Fatalf("change %+v", change)
		}
	}

	w.catalog.Refresh(ctx)
	if page := w.changes(t, first.Next); len(page.Changes) != 0 {
		t.Fatalf("an unchanged refresh produced %d changes", len(page.Changes))
	}

	w.fake.Update("movie-2", "Movie 2: Director's Cut")
	w.catalog.Refresh(ctx)
	page := w.changes(t, first.Next)
	if len(page.Changes) != 1 || page.Changes[0].ItemID != "movie-2" || page.Changes[0].Revision != 2 || page.Changes[0].Metadata.Name != "Movie 2: Director's Cut" {
		t.Fatalf("after one edit: %+v", page.Changes)
	}

	paged, err := w.catalog.Changes(ctx, 0, 2, nil)
	if err != nil || len(paged.Changes) != 2 || !paged.More {
		t.Fatalf("a limited page: %+v, %v", paged, err)
	}
}

// Parents take sequences before their children.
func TestParentsComeBeforeChildren(t *testing.T) {
	w := newWorld(t)
	if err := w.catalog.Publish(context.Background(), "lib-shows", []string{"/media/tv"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	order := map[string]int{}
	for index, change := range w.changes(t, 0).Changes {
		order[change.ItemID] = index
	}
	if !(order["series-1"] < order["season-1"] && order["season-1"] < order["ep-1"]) {
		t.Fatalf("order = %v, want series, season, episode", order)
	}
	if ids(w.changes(t, 0))["ep-1"].ParentID != "season-1" {
		t.Fatal("an episode's parent is its season")
	}
}

// omitting drops one item from enumeration while Jellyfin still has it, as a
// page boundary moving under concurrent edits might.
type omitting struct {
	*jellyfin.Client
	skip string
}

func (adapter omitting) Items(ctx context.Context, library string, start int, limit int) (jellyfin.Page, error) {
	page, err := adapter.Client.Items(ctx, library, start, limit)
	var kept []jellyfin.Item
	for _, item := range page.Items {
		if item.ID != adapter.skip {
			kept = append(kept, item)
		}
	}
	page.Items = kept
	return page, err
}

// C-CA-2: an item missing from an enumeration is tombstoned only once Jellyfin
// confirms it has gone, so a paging glitch never deletes anything downstream.
func TestTombstonesRequireConfirmation(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	first := w.changes(t, 0)

	glitchy := New(omitting{w.client, "movie-3"}, w.repo, []string{"lib-family"}, nil)
	glitchy.Refresh(ctx)
	if page := w.changes(t, first.Next); len(page.Changes) != 0 {
		t.Fatalf("an item still in Jellyfin was tombstoned: %+v", page.Changes)
	}

	w.fake.Delete("movie-3")
	w.catalog.Refresh(ctx)
	page := w.changes(t, first.Next)
	if len(page.Changes) != 1 || !page.Changes[0].Tombstone || page.Changes[0].ItemID != "movie-3" || page.Changes[0].Metadata != nil {
		t.Fatalf("a deleted item should become a bare tombstone: %+v", page.Changes)
	}
}

type failing struct{ *jellyfin.Client }

func (failing) Items(context.Context, string, int, int) (jellyfin.Page, error) {
	return jellyfin.Page{}, errors.New("jellyfin is restarting")
}

// C-ST-5 at the source: a refresh that cannot read a library changes nothing.
func TestATransientFailureWithdrawsNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	first := w.changes(t, 0)
	if err := New(failing{w.client}, w.repo, nil, nil).Refresh(ctx); err == nil {
		t.Fatal("the failure should be reported")
	}
	if page := w.changes(t, first.Next); len(page.Changes) != 0 {
		t.Fatal("a failed refresh must not withdraw anything")
	}
}

// C-CA-5, C-PR-2: an opted-out library is never sent to that destination,
// not even as a tombstone, and the cursor still advances past it.
func TestAnOptedOutLibraryIsNeverSent(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"})
	w.catalog.Publish(ctx, "lib-shows", []string{"/media/tv"})
	page := w.changes(t, 0, "lib-movies")
	for _, change := range page.Changes {
		if change.LibraryID == "lib-movies" {
			t.Fatal("an opted-out library's item was sent")
		}
	}
	if len(page.Changes) != 3 || page.Next != w.changes(t, 0).Next {
		t.Fatalf("the destination should get shows only, with its cursor past everything: %+v", page)
	}
	w.catalog.Unpublish(ctx, "lib-movies")
	if later := w.changes(t, page.Next, "lib-movies"); len(later.Changes) != 0 {
		t.Fatal("tombstones of an opted-out library reveal its item IDs and must not be sent")
	}
	if later := w.changes(t, page.Next); len(later.Changes) != 5 {
		t.Fatal("a destination that had not opted out learns of the withdrawal")
	}
}

// C-CA-4: a per-item request is authorized against live state, so an item
// moved out of a published library is refused before any refresh.
func TestAuthorizationUsesLiveLibraryMembership(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := w.catalog.Authorize(ctx, "movie-1"); err != nil {
		t.Fatalf("a published item: %v", err)
	}
	w.fake.Move("movie-1", "lib-family")
	if _, err := w.catalog.Authorize(ctx, "movie-1"); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("an item moved to another library: error = %v, want ErrNotPublished", err)
	}
	w.fake.Delete("movie-2")
	if _, err := w.catalog.Authorize(ctx, "movie-2"); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("a deleted item: error = %v, want ErrNotPublished", err)
	}
	if _, err := w.catalog.Authorize(ctx, "no-such-item"); !errors.Is(err, ErrNotPublished) {
		t.Fatal("an unknown item must be refused")
	}
	// The moved item is withdrawn on the next refresh.
	before := w.changes(t, 0).Next
	w.catalog.Refresh(ctx)
	withdrawn := ids(w.changes(t, before))
	if !withdrawn["movie-1"].Tombstone || !withdrawn["movie-2"].Tombstone {
		t.Fatalf("moved and deleted items should be withdrawn: %+v", withdrawn)
	}
}

// C-MA-7: a change carries the source's studios, first tagline, and ratings.
func TestChangesCarrySourceMetadata(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.fake.AddItem("lib-movies", jellyfin.Item{
		ID: "movie-rich", Name: "Rich", Type: "Movie", Path: "/media/movies/Rich/rich.mkv",
		Studios: []jellyfin.NamedItem{{Name: "Probe Pictures"}, {Name: " "}}, Taglines: []string{"Free your mind", "Second"},
		CommunityRating: 8.7, CriticRating: 83,
	})
	if err := w.catalog.Publish(ctx, "lib-movies", []string{"/media/movies"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	change, ok := ids(w.changes(t, 0))["movie-rich"]
	if !ok || change.Metadata == nil {
		t.Fatal("the item was not published")
	}
	got := change.Metadata
	if len(got.Studios) != 1 || got.Studios[0] != "Probe Pictures" || got.Tagline != "Free your mind" || got.CommunityRating != 8.7 || got.CriticRating != 83 {
		t.Fatalf("source metadata not carried: %+v", got)
	}
}
