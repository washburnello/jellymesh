package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"jellymesh/internal/history"
)

func TestHistoryRepositorySaveGetRoundTrip(t *testing.T) {
	database := openTestDB(t)
	repo := NewHistoryRepository(database)
	ctx := context.Background()

	lastPlayed := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 9, 21, 11, 0, 0, 0, time.UTC)
	entry := history.Entry{
		UserID:              "user-1",
		WorkID:              "movie:tmdb:123",
		MediaType:           "movie",
		ProviderIdentity:    "tmdb:123",
		Played:              true,
		PlayCount:           4,
		ResumePositionTicks: 12345,
		LastPlayedAt:        lastPlayed,
		UpdatedAt:           updated,
	}
	if err := repo.Save(ctx, entry); err != nil {
		t.Fatalf("save: %v", err)
	}

	stored, ok, err := repo.Get(ctx, "user-1", "movie:tmdb:123")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("expected entry to be found")
	}
	if stored.MediaType != "movie" || stored.ProviderIdentity != "tmdb:123" || !stored.Played ||
		stored.PlayCount != 4 || stored.ResumePositionTicks != 12345 {
		t.Fatalf("unexpected round-tripped entry: %#v", stored)
	}
	if !stored.LastPlayedAt.Equal(lastPlayed) {
		t.Fatalf("last played at did not round trip: got %v want %v", stored.LastPlayedAt, lastPlayed)
	}
	if !stored.UpdatedAt.Equal(updated) {
		t.Fatalf("updated at did not round trip: got %v want %v", stored.UpdatedAt, updated)
	}
}

func TestHistoryRepositorySaveGetZeroTimeRoundTrip(t *testing.T) {
	database := openTestDB(t)
	repo := NewHistoryRepository(database)
	ctx := context.Background()

	// LastPlayedAt is intentionally left zero (never played); UpdatedAt is
	// left zero too so Save's default-to-now behavior is exercised.
	if err := repo.Save(ctx, history.Entry{UserID: "user-1", WorkID: "movie:tmdb:999"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	stored, ok, err := repo.Get(ctx, "user-1", "movie:tmdb:999")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("expected entry to be found")
	}
	if !stored.LastPlayedAt.IsZero() {
		t.Fatalf("expected zero last played at, got %v", stored.LastPlayedAt)
	}
	if stored.UpdatedAt.IsZero() {
		t.Fatal("expected Save to default a zero updated-at to now")
	}
}

func TestHistoryRepositoryGetMissingReturnsNotFound(t *testing.T) {
	database := openTestDB(t)
	repo := NewHistoryRepository(database)
	_, ok, err := repo.Get(context.Background(), "nobody", "nothing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatal("expected no entry to be found")
	}
}

func TestHistoryRepositorySaveUpserts(t *testing.T) {
	database := openTestDB(t)
	repo := NewHistoryRepository(database)
	ctx := context.Background()

	if err := repo.Save(ctx, history.Entry{UserID: "user-1", WorkID: "movie:tmdb:1", PlayCount: 1}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := repo.Save(ctx, history.Entry{UserID: "user-1", WorkID: "movie:tmdb:1", PlayCount: 2, Played: true}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	stored, ok, err := repo.Get(ctx, "user-1", "movie:tmdb:1")
	if err != nil || !ok {
		t.Fatalf("get after upsert: ok=%v err=%v", ok, err)
	}
	if stored.PlayCount != 2 || !stored.Played {
		t.Fatalf("upsert did not replace the row: %#v", stored)
	}

	var rowCount int
	if err := database.SQL().QueryRow(`SELECT count(*) FROM history`).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected a single row after upsert, got %d", rowCount)
	}
}

func TestHistoryRepositoryWorksForUserIsolatesUsers(t *testing.T) {
	database := openTestDB(t)
	repo := NewHistoryRepository(database)
	ctx := context.Background()

	if err := repo.Save(ctx, history.Entry{UserID: "user-1", WorkID: "movie:tmdb:1", Played: true}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.Save(ctx, history.Entry{UserID: "user-1", WorkID: "movie:tmdb:2", Played: false}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.Save(ctx, history.Entry{UserID: "user-2", WorkID: "movie:tmdb:1", Played: true}); err != nil {
		t.Fatalf("save: %v", err)
	}

	works, err := repo.WorksForUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("works for user: %v", err)
	}
	if len(works) != 2 {
		t.Fatalf("expected 2 works for user-1, got %d", len(works))
	}
	for _, work := range works {
		if work.UserID != "user-1" {
			t.Fatalf("leaked another user's entry: %#v", work)
		}
	}

	empty, err := repo.WorksForUser(ctx, "nobody")
	if err != nil {
		t.Fatalf("works for user: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no works for an unknown user, got %d", len(empty))
	}
}

func TestHistoryRepositoryValidationMatchesSentinels(t *testing.T) {
	database := openTestDB(t)
	repo := NewHistoryRepository(database)
	ctx := context.Background()

	if err := repo.Save(ctx, history.Entry{WorkID: "movie:tmdb:1"}); !errors.Is(err, history.ErrUserIDRequired) {
		t.Fatalf("expected ErrUserIDRequired, got %v", err)
	}
	if err := repo.Save(ctx, history.Entry{UserID: "user-1"}); !errors.Is(err, history.ErrWorkIDRequired) {
		t.Fatalf("expected ErrWorkIDRequired, got %v", err)
	}
	if err := repo.Save(ctx, history.Entry{UserID: "user-1", WorkID: "w", ResumePositionTicks: -1}); !errors.Is(err, history.ErrInvalidPosition) {
		t.Fatalf("expected ErrInvalidPosition, got %v", err)
	}
	if err := repo.Save(ctx, history.Entry{UserID: "user-1", WorkID: "w", PlayCount: -1}); !errors.Is(err, history.ErrInvalidPlayCount) {
		t.Fatalf("expected ErrInvalidPlayCount, got %v", err)
	}

	// The identity-mismatch case that the in-memory MergePreservingLocal
	// guards against cannot arise through Restore: it takes one entry and
	// reads its counterpart from the ledger by that entry's own key, so the
	// two sides always share an identity by construction.
}

// Restoration must not let a stale ledger row beat fresher local progress.
// This is the same invariant as TestMergeDoesNotResurrectDeliberateUnwatch in
// internal/history, asserted again here because a port to storage is exactly
// where the direction of the merge is easy to invert.
func TestRestoreDoesNotLetTheLedgerBeatNewerLocalProgress(t *testing.T) {
	ctx := context.Background()
	repo := NewHistoryRepository(openTestDB(t))

	// What was preserved before the item was purged.
	preserved := history.Entry{
		UserID: "user-1", WorkID: "movie:tmdb:123",
		Played: true, PlayCount: 3, ResumePositionTicks: 900,
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := repo.Save(ctx, preserved); err != nil {
		t.Fatalf("save preserved: %v", err)
	}

	// The user has since deliberately marked it unwatched in Jellyfin.
	local := history.Entry{
		UserID: "user-1", WorkID: "movie:tmdb:123",
		Played: false, PlayCount: 0, ResumePositionTicks: 0,
		UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	merged, found, err := repo.Restore(ctx, local)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !found {
		t.Fatal("a preserved row existed and should have been reported")
	}
	if merged.Played {
		t.Fatal("a stale ledger row overwrote a deliberate unwatch")
	}
	if merged.PlayCount != 0 || merged.ResumePositionTicks != 0 {
		t.Fatalf("stale progress was resurrected: %#v", merged)
	}
}

func TestRestoreFillsGapsWhenLocalHasNothing(t *testing.T) {
	ctx := context.Background()
	repo := NewHistoryRepository(openTestDB(t))

	preserved := history.Entry{
		UserID: "user-1", WorkID: "movie:tmdb:123",
		Played: true, PlayCount: 3, ResumePositionTicks: 900,
		UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := repo.Save(ctx, preserved); err != nil {
		t.Fatalf("save preserved: %v", err)
	}

	merged, found, err := repo.Restore(ctx, history.Entry{UserID: "user-1", WorkID: "movie:tmdb:123"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !found {
		t.Fatal("expected to find the preserved row")
	}
	if !merged.Played || merged.PlayCount != 3 || merged.ResumePositionTicks != 900 {
		t.Fatalf("restoration did not reapply preserved state: %#v", merged)
	}
}

func TestRestoreWithNoPreservedRowReturnsLocalUnchanged(t *testing.T) {
	ctx := context.Background()
	repo := NewHistoryRepository(openTestDB(t))

	local := history.Entry{
		UserID: "user-1", WorkID: "movie:tmdb:999",
		Played: true, PlayCount: 1, ResumePositionTicks: 42,
		UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	merged, found, err := repo.Restore(ctx, local)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if found {
		t.Fatal("no row was preserved, so found must be false")
	}
	if !merged.Played || merged.PlayCount != 1 || merged.ResumePositionTicks != 42 {
		t.Fatalf("local entry should pass through unchanged: %#v", merged)
	}
}

func TestRestoreValidatesIdentity(t *testing.T) {
	ctx := context.Background()
	repo := NewHistoryRepository(openTestDB(t))
	if _, _, err := repo.Restore(ctx, history.Entry{WorkID: "w"}); !errors.Is(err, history.ErrUserIDRequired) {
		t.Fatalf("expected ErrUserIDRequired, got %v", err)
	}
	if _, _, err := repo.Restore(ctx, history.Entry{UserID: "u"}); !errors.Is(err, history.ErrWorkIDRequired) {
		t.Fatalf("expected ErrWorkIDRequired, got %v", err)
	}
}
