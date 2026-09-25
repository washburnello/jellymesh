package catalog

import (
	"testing"
	"time"
)

func TestRetentionStoreUsesSevenDayDefault(t *testing.T) {
	store, err := NewRetentionStore(RetentionPolicy{GracePeriod: DefaultDeletionGracePeriod})
	if err != nil {
		t.Fatalf("new retention store: %v", err)
	}
	deletedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	record, err := store.RecordDeletion("cedar", "movies", "movie-1", "movie:tmdb:123", deletedAt)
	if err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	if record.ExpiresAt != deletedAt.Add(7*24*time.Hour) {
		t.Fatalf("unexpected expiration: %s", record.ExpiresAt)
	}
	if store.Expire(deletedAt.Add(7*24*time.Hour - time.Second)) != 0 {
		t.Fatal("record expired before grace period")
	}
	if store.Expire(record.ExpiresAt) != 1 {
		t.Fatal("record did not expire at grace period")
	}
	if _, ok := store.Get("cedar", "movies", "movie-1"); ok {
		t.Fatal("expired record was not removed")
	}
}

func TestReturningItemDiscardsRetention(t *testing.T) {
	store, err := NewRetentionStore(RetentionPolicy{GracePeriod: DefaultDeletionGracePeriod})
	if err != nil {
		t.Fatalf("new retention store: %v", err)
	}
	if _, err := store.RecordDeletion("cedar", "movies", "movie-1", "movie:tmdb:123", time.Now()); err != nil {
		t.Fatalf("record deletion: %v", err)
	}
	store.Discard("cedar", "movies", "movie-1")
	if _, ok := store.Get("cedar", "movies", "movie-1"); ok {
		t.Fatal("discarded retention record still exists")
	}
}
