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
