package history

import (
	"errors"
	"testing"
	"time"
)

func TestLedgerKeepsUserHistoryByLogicalWork(t *testing.T) {
	ledger := NewLedger()
	entry := Entry{
		UserID:              "user-1",
		WorkID:              "movie:tmdb:123",
		MediaType:           "movie",
		ProviderIdentity:    "tmdb:123",
		Played:              true,
		PlayCount:           2,
		ResumePositionTicks: 4200,
		LastPlayedAt:        time.Now().UTC(),
	}
	if err := ledger.Save(entry); err != nil {
		t.Fatalf("save history: %v", err)
	}
	stored, ok := ledger.Get("user-1", "movie:tmdb:123")
	if !ok {
		t.Fatal("history entry was not found")
	}
	if !stored.Played || stored.PlayCount != 2 || stored.ResumePositionTicks != 4200 {
		t.Fatalf("unexpected history entry: %#v", stored)
	}
}

func TestLedgerIsolatesUsers(t *testing.T) {
	ledger := NewLedger()
	if err := ledger.Save(Entry{UserID: "user-1", WorkID: "movie:tmdb:123", Played: true}); err != nil {
		t.Fatalf("save first user: %v", err)
	}
	if err := ledger.Save(Entry{UserID: "user-2", WorkID: "movie:tmdb:123"}); err != nil {
		t.Fatalf("save second user: %v", err)
	}
	first, ok := ledger.Get("user-1", "movie:tmdb:123")
	if !ok || !first.Played {
		t.Fatalf("unexpected first-user history: %#v", first)
	}
	second, ok := ledger.Get("user-2", "movie:tmdb:123")
	if !ok || second.Played {
		t.Fatalf("unexpected second-user history: %#v", second)
	}
}

func TestMergePreservesLocalProgress(t *testing.T) {
	local := Entry{
		UserID:              "user-1",
		WorkID:              "movie:tmdb:123",
		MediaType:           "movie",
		Played:              true,
		PlayCount:           3,
		ResumePositionTicks: 900,
		LastPlayedAt:        time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
		UpdatedAt:           time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
	}
	preserved := Entry{
		UserID:              "user-1",
		WorkID:              "movie:tmdb:123",
		MediaType:           "movie",
		ProviderIdentity:    "tmdb:123",
		Played:              true,
		PlayCount:           2,
		ResumePositionTicks: 100,
		LastPlayedAt:        time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC),
		UpdatedAt:           time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC),
	}
	merged, err := MergePreservingLocal(local, preserved)
	if err != nil {
		t.Fatalf("merge history: %v", err)
	}
	if merged.PlayCount != 3 || merged.ResumePositionTicks != 900 || !merged.Played {
		t.Fatalf("local progress was not preserved: %#v", merged)
	}
	if merged.ProviderIdentity != "tmdb:123" {
		t.Fatalf("missing provider identity was not filled: %#v", merged)
	}
}

func TestMergeFillsMissingLocalHistory(t *testing.T) {
	preserved := Entry{
		UserID:              "user-1",
		WorkID:              "movie:tmdb:123",
		MediaType:           "movie",
		ProviderIdentity:    "tmdb:123",
		Played:              true,
		PlayCount:           2,
		ResumePositionTicks: 4200,
		LastPlayedAt:        time.Now().UTC(),
		UpdatedAt:           time.Now().UTC(),
	}
	merged, err := MergePreservingLocal(Entry{UserID: "user-1", WorkID: "movie:tmdb:123"}, preserved)
	if err != nil {
		t.Fatalf("merge history: %v", err)
	}
	if !merged.Played || merged.PlayCount != 2 || merged.ResumePositionTicks != 4200 {
		t.Fatalf("missing history was not restored: %#v", merged)
	}
}

func TestMergeRejectsDifferentLogicalWorks(t *testing.T) {
	_, err := MergePreservingLocal(
		Entry{UserID: "user-1", WorkID: "movie:tmdb:123"},
		Entry{UserID: "user-1", WorkID: "movie:tmdb:456"},
	)
	if !errors.Is(err, ErrHistoryIdentityMismatch) {
		t.Fatalf("unexpected identity error: %v", err)
	}
}

func TestLedgerRejectsInvalidEntries(t *testing.T) {
	ledger := NewLedger()
	if err := ledger.Save(Entry{WorkID: "movie:tmdb:123"}); !errors.Is(err, ErrUserIDRequired) {
		t.Fatalf("unexpected missing-user error: %v", err)
	}
	if err := ledger.Save(Entry{UserID: "user-1", WorkID: "movie:tmdb:123", ResumePositionTicks: -1}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("unexpected position error: %v", err)
	}
	if err := ledger.Save(Entry{UserID: "user-1", WorkID: "movie:tmdb:123", PlayCount: -1}); !errors.Is(err, ErrInvalidPlayCount) {
		t.Fatalf("unexpected play-count error: %v", err)
	}
}

func TestMergeDoesNotResurrectDeliberateUnwatch(t *testing.T) {
	preserved := Entry{
		UserID: "user-1", WorkID: "movie:tmdb:123",
		Played: true, PlayCount: 3, ResumePositionTicks: 900,
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	// The user later marked the work unwatched; that record is newer.
	local := Entry{
		UserID: "user-1", WorkID: "movie:tmdb:123",
		Played: false, PlayCount: 0, ResumePositionTicks: 0,
		UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	merged, err := MergePreservingLocal(local, preserved)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.Played {
		t.Fatal("a deliberate unwatch was undone by the preserved ledger entry")
	}
	if merged.PlayCount != 0 {
		t.Fatalf("play count was resurrected: %d", merged.PlayCount)
	}
	if merged.ResumePositionTicks != 0 {
		t.Fatalf("resume position was resurrected: %d", merged.ResumePositionTicks)
	}
}

func TestMergeStillRestoresWhenLocalHasNoRecord(t *testing.T) {
	preserved := Entry{
		UserID: "user-1", WorkID: "movie:tmdb:123",
		Played: true, PlayCount: 3, ResumePositionTicks: 900,
		UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	local := Entry{UserID: "user-1", WorkID: "movie:tmdb:123"}
	merged, err := MergePreservingLocal(local, preserved)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !merged.Played || merged.PlayCount != 3 || merged.ResumePositionTicks != 900 {
		t.Fatalf("restoration path regressed: %#v", merged)
	}
}
