package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// A-14: pins round-trip, a folder is pinned once, a touch within a day does
// not rewrite, and pruning forgets only pins unused since the cutoff.
func TestWorkPinsRoundTripTouchAndPrune(t *testing.T) {
	repo := NewWorkPinRepository(openTestDB(t))
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	old, err := repo.Add(ctx, WorkPin{Kind: "series", Folder: "TV Shows/A", Identifiers: map[string]string{"tmdb": "1"}, LastUsed: start})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := repo.Add(ctx, WorkPin{Kind: "movie", Folder: "Movies/B", Identifiers: map[string]string{"imdb": "tt2"}, LastUsed: start})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Add(ctx, WorkPin{Kind: "movie", Folder: "Movies/B", Identifiers: map[string]string{}}); err == nil {
		t.Fatal("a folder must be pinned only once")
	}
	if err := repo.Touch(ctx, []int64{fresh.ID}, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pins, err := repo.All(ctx)
	if err != nil || len(pins) != 2 || !pins[1].LastUsed.Equal(start) {
		t.Fatalf("a touch within a day should not rewrite: %+v, %v", pins, err)
	}
	if !reflect.DeepEqual(pins[0].Identifiers, map[string]string{"tmdb": "1"}) || pins[0].Kind != "series" || pins[0].ID != old.ID {
		t.Fatalf("pin did not round-trip: %+v", pins[0])
	}
	if err := repo.Touch(ctx, []int64{fresh.ID}, start.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if removed, err := repo.Prune(ctx, start.Add(24*time.Hour)); err != nil || removed != 1 {
		t.Fatalf("prune removed %d, %v", removed, err)
	}
	if pins, _ := repo.All(ctx); len(pins) != 1 || pins[0].ID != fresh.ID {
		t.Fatalf("the touched pin should remain: %+v", pins)
	}
}

// #61: a withdrawn path's reference is reused by what next appears there,
// once, and only within RetiredReferenceLife.
func TestRetiredReferencesAreReusedOnceAndExpire(t *testing.T) {
	database := openTestDB(t)
	repo := NewMaterializedRepository(database)
	ctx := context.Background()
	save := func(item string, path string, reference string) {
		if err := repo.Save(ctx, Materialized{SourceNodeID: "cedar", ItemID: item, LibraryID: "lib", Reference: reference, Path: path, Checksum: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	save("a", "Movies/A/A.strm", "ref-a")
	if err := repo.Remove(ctx, "cedar", "a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.ReferenceFor(ctx, "Movies/A/A.strm"); got != "ref-a" {
		t.Fatalf("the path's reference should be reused, got %q", got)
	}
	if got, _ := repo.ReferenceFor(ctx, "Movies/A/A.strm"); got == "ref-a" {
		t.Fatal("a retired reference is reused only once")
	}

	save("b", "Movies/B/B.strm", "ref-b")
	repo.Remove(ctx, "cedar", "b")
	real := nowUTC
	nowUTC = func() time.Time { return real().Add(RetiredReferenceLife + time.Hour) }
	defer func() { nowUTC = real }()
	if got, _ := repo.ReferenceFor(ctx, "Movies/B/B.strm"); got == "ref-b" {
		t.Fatal("a reference retired longer ago than RetiredReferenceLife must not be reused")
	}
}
