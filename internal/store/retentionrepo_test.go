package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"jellymesh/internal/catalog"
)

func newTestRetentionRepository(t *testing.T) (*RetentionRepository, *DB) {
	t.Helper()
	database := openTestDB(t)
	repo, err := NewRetentionRepository(database, catalog.RetentionPolicy{GracePeriod: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("new retention repository: %v", err)
	}
	return repo, database
}

func TestRetentionRepositoryRecordDeletionRoundTrip(t *testing.T) {
	repo, _ := newTestRetentionRepository(t)
	ctx := context.Background()

	deletedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	record, err := repo.RecordDeletion(ctx, "cedar", "movies", "movie-1", "movie:tmdb:123", deletedAt)
	if err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	wantExpiry := deletedAt.Add(7 * 24 * time.Hour)
	if !record.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("unexpected expiry: got %v want %v", record.ExpiresAt, wantExpiry)
	}

	stored, ok, err := repo.Get(ctx, "cedar", "movies", "movie-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("expected record to be found")
	}
	if stored.LogicalWorkID != "movie:tmdb:123" {
		t.Fatalf("logical work id did not round trip: %#v", stored)
	}
	if !stored.DeletedAt.Equal(deletedAt) {
		t.Fatalf("deleted at did not round trip: got %v want %v", stored.DeletedAt, deletedAt)
	}
	if !stored.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expires at did not round trip: got %v want %v", stored.ExpiresAt, wantExpiry)
	}
}

func TestRetentionRepositoryRecordDeletionDefaultsZeroDeletedAt(t *testing.T) {
	repo, _ := newTestRetentionRepository(t)
	ctx := context.Background()

	before := time.Now().UTC()
	record, err := repo.RecordDeletion(ctx, "cedar", "movies", "movie-2", "", time.Time{})
	if err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	after := time.Now().UTC()
	if record.DeletedAt.Before(before) || record.DeletedAt.After(after) {
		t.Fatalf("expected deleted-at to default to now, got %v (window %v - %v)", record.DeletedAt, before, after)
	}
}

func TestRetentionRepositoryGetMissingReturnsNotFound(t *testing.T) {
	repo, _ := newTestRetentionRepository(t)
	_, ok, err := repo.Get(context.Background(), "cedar", "movies", "missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatal("expected no record to be found")
	}
}

func TestRetentionRepositoryDiscard(t *testing.T) {
	repo, _ := newTestRetentionRepository(t)
	ctx := context.Background()

	if _, err := repo.RecordDeletion(ctx, "cedar", "movies", "movie-1", "movie:tmdb:123", time.Now()); err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	if err := repo.Discard(ctx, "cedar", "movies", "movie-1"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	_, ok, err := repo.Get(ctx, "cedar", "movies", "movie-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatal("discarded record still exists")
	}

	// Discarding something that was never there is not an error.
	if err := repo.Discard(ctx, "cedar", "movies", "never-existed"); err != nil {
		t.Fatalf("discard of missing record: %v", err)
	}
}

func TestRetentionRepositoryCountForSource(t *testing.T) {
	repo, _ := newTestRetentionRepository(t)
	ctx := context.Background()

	if _, err := repo.RecordDeletion(ctx, "cedar", "movies", "movie-1", "", time.Now()); err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	if _, err := repo.RecordDeletion(ctx, "cedar", "tv", "show-1", "", time.Now()); err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	if _, err := repo.RecordDeletion(ctx, "walnut", "movies", "movie-9", "", time.Now()); err != nil {
		t.Fatalf("record deletion: %v", err)
	}

	count, err := repo.CountForSource(ctx, "cedar")
	if err != nil {
		t.Fatalf("count for source: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 records for cedar, got %d", count)
	}

	count, err = repo.CountForSource(ctx, "nobody")
	if err != nil {
		t.Fatalf("count for source: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 records for an unknown source, got %d", count)
	}
}

func TestRetentionRepositoryExpireRemovesOnlyDueRecords(t *testing.T) {
	repo, _ := newTestRetentionRepository(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// Expires exactly at base + grace period.
	dueAtBoundary, err := repo.RecordDeletion(ctx, "cedar", "movies", "due-at-boundary", "", base)
	if err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	// Expired well before the sweep time.
	if _, err := repo.RecordDeletion(ctx, "cedar", "movies", "long-expired", "", base.Add(-24*time.Hour)); err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	// Not due yet.
	notDue, err := repo.RecordDeletion(ctx, "cedar", "movies", "not-due", "", base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("record deletion: %v", err)
	}

	sweepAt := dueAtBoundary.ExpiresAt
	count, err := repo.Expire(ctx, sweepAt)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 expired records, got %d", count)
	}

	if _, ok, _ := repo.Get(ctx, "cedar", "movies", "due-at-boundary"); ok {
		t.Fatal("record at the expiry boundary should have been removed")
	}
	if _, ok, _ := repo.Get(ctx, "cedar", "movies", "long-expired"); ok {
		t.Fatal("long-expired record should have been removed")
	}
	if _, ok, err := repo.Get(ctx, "cedar", "movies", "not-due"); err != nil || !ok {
		t.Fatalf("record not yet due should still exist: ok=%v err=%v", ok, err)
	}
	_ = notDue
}

func TestRetentionRepositoryExpireUsesTheIndex(t *testing.T) {
	_, database := newTestRetentionRepository(t)
	rows, err := database.SQL().Query(
		`EXPLAIN QUERY PLAN DELETE FROM retention WHERE expires_at <= ?`,
		FormatTime(time.Now()),
	)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan += detail + " "
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !contains(plan, "idx_retention_expires") {
		t.Fatalf("expire's delete is not using the retention expiry index, plan was: %s", plan)
	}
}

func TestNewRetentionRepositoryRejectsInvalidPolicy(t *testing.T) {
	database := openTestDB(t)
	_, err := NewRetentionRepository(database, catalog.RetentionPolicy{GracePeriod: 0})
	if !errors.Is(err, catalog.ErrInvalidRetentionPolicy) {
		t.Fatalf("expected ErrInvalidRetentionPolicy, got %v", err)
	}
}

func TestRetentionRepositoryValidationMatchesSentinels(t *testing.T) {
	repo, _ := newTestRetentionRepository(t)
	ctx := context.Background()

	if _, err := repo.RecordDeletion(ctx, "", "movies", "item", "", time.Now()); !errors.Is(err, catalog.ErrSourceNodeIDRequired) {
		t.Fatalf("expected ErrSourceNodeIDRequired, got %v", err)
	}
	if _, err := repo.RecordDeletion(ctx, "cedar", "", "item", "", time.Now()); !errors.Is(err, catalog.ErrSourceLibraryIDRequired) {
		t.Fatalf("expected ErrSourceLibraryIDRequired, got %v", err)
	}
	if _, err := repo.RecordDeletion(ctx, "cedar", "movies", "", "", time.Now()); !errors.Is(err, catalog.ErrSourceItemIDRequired) {
		t.Fatalf("expected ErrSourceItemIDRequired, got %v", err)
	}
}
