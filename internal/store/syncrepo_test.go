package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSyncGetNotFoundForUnseenSource(t *testing.T) {
	database := openTestDB(t)
	repo := NewSyncRepository(database)
	_, found, err := repo.Get(context.Background(), "cedar")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if found {
		t.Fatal("expected no sync state for an unseen source")
	}
}

func TestSaveCursorRequiresSourceNodeID(t *testing.T) {
	database := openTestDB(t)
	repo := NewSyncRepository(database)
	if err := repo.SaveCursor(context.Background(), "", "cursor", time.Now()); !errors.Is(err, ErrSourceNodeIDRequired) {
		t.Fatalf("error = %v, want ErrSourceNodeIDRequired", err)
	}
	if err := repo.RecordFailure(context.Background(), "", "network", time.Now()); !errors.Is(err, ErrSourceNodeIDRequired) {
		t.Fatalf("error = %v, want ErrSourceNodeIDRequired", err)
	}
}

func TestSaveCursorUpsertsForAnUnseenSource(t *testing.T) {
	database := openTestDB(t)
	repo := NewSyncRepository(database)
	ctx := context.Background()
	succeededAt := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	if err := repo.SaveCursor(ctx, "cedar", "cursor-1", succeededAt); err != nil {
		t.Fatalf("save cursor on unseen source: %v", err)
	}

	state, found, err := repo.Get(ctx, "cedar")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if state.Cursor != "cursor-1" {
		t.Fatalf("cursor = %q, want cursor-1", state.Cursor)
	}
	if state.State != SyncStateAvailable {
		t.Fatalf("state = %q, want %q", state.State, SyncStateAvailable)
	}
	if !state.LastSuccessAt.Equal(succeededAt) {
		t.Fatalf("last success at = %v, want %v", state.LastSuccessAt, succeededAt)
	}
	if state.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want 0", state.ConsecutiveFailures)
	}
}

func TestSaveCursorResetsFailureCountAndMarksAvailable(t *testing.T) {
	database := openTestDB(t)
	repo := NewSyncRepository(database)
	ctx := context.Background()

	failedAt := time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	if err := repo.RecordFailure(ctx, "cedar", "timeout", failedAt); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if err := repo.RecordFailure(ctx, "cedar", "timeout", failedAt); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	before, _, err := repo.Get(ctx, "cedar")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if before.ConsecutiveFailures != 2 {
		t.Fatalf("consecutive failures = %d, want 2", before.ConsecutiveFailures)
	}

	succeededAt := failedAt.Add(time.Minute)
	if err := repo.SaveCursor(ctx, "cedar", "cursor-2", succeededAt); err != nil {
		t.Fatalf("save cursor: %v", err)
	}

	after, found, err := repo.Get(ctx, "cedar")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if after.State != SyncStateAvailable {
		t.Fatalf("state = %q, want %q", after.State, SyncStateAvailable)
	}
	if after.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want reset to 0", after.ConsecutiveFailures)
	}
	if after.Cursor != "cursor-2" {
		t.Fatalf("cursor = %q, want cursor-2", after.Cursor)
	}
}

func TestRecordFailurePreservesTheExistingCursor(t *testing.T) {
	database := openTestDB(t)
	repo := NewSyncRepository(database)
	ctx := context.Background()

	succeededAt := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	if err := repo.SaveCursor(ctx, "cedar", "known-good-cursor", succeededAt); err != nil {
		t.Fatalf("save cursor: %v", err)
	}

	failedAt := succeededAt.Add(time.Hour)
	if err := repo.RecordFailure(ctx, "cedar", "network", failedAt); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	state, found, err := repo.Get(ctx, "cedar")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if state.Cursor != "known-good-cursor" {
		t.Fatalf("a failed sync must never discard the known-good cursor, got %q", state.Cursor)
	}
	if state.FailureCategory != "network" {
		t.Fatalf("failure category = %q, want network", state.FailureCategory)
	}
	if !state.LastFailureAt.Equal(failedAt) {
		t.Fatalf("last failure at = %v, want %v", state.LastFailureAt, failedAt)
	}
	if state.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", state.ConsecutiveFailures)
	}
}

func TestSyncStateTransitionsFromUnknownThroughDegradedToUnavailable(t *testing.T) {
	database := openTestDB(t)
	repo := NewSyncRepository(database)
	ctx := context.Background()

	// A row created with no explicit state (as the schema default provides)
	// represents a source that has never reported success or failure.
	mustExec(t, database, `INSERT INTO sync_cursors(source_node_id) VALUES('cedar')`)
	initial, found, err := repo.Get(ctx, "cedar")
	if err != nil || !found {
		t.Fatalf("get initial: found=%v err=%v", found, err)
	}
	if initial.State != SyncStateUnknown {
		t.Fatalf("initial state = %q, want %q", initial.State, SyncStateUnknown)
	}

	failedAt := time.Now().UTC()

	if err := repo.RecordFailure(ctx, "cedar", "timeout", failedAt); err != nil {
		t.Fatalf("record failure 1: %v", err)
	}
	afterFirst, _, _ := repo.Get(ctx, "cedar")
	if afterFirst.State != SyncStateDegraded {
		t.Fatalf("state after 1 failure = %q, want %q", afterFirst.State, SyncStateDegraded)
	}
	if afterFirst.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", afterFirst.ConsecutiveFailures)
	}

	if err := repo.RecordFailure(ctx, "cedar", "timeout", failedAt); err != nil {
		t.Fatalf("record failure 2: %v", err)
	}
	afterSecond, _, _ := repo.Get(ctx, "cedar")
	if afterSecond.State != SyncStateDegraded {
		t.Fatalf("state after 2 failures = %q, want still %q below the threshold", afterSecond.State, SyncStateDegraded)
	}
	if afterSecond.ConsecutiveFailures != 2 {
		t.Fatalf("consecutive failures = %d, want 2", afterSecond.ConsecutiveFailures)
	}

	if UnavailableAfterConsecutiveFailures != 3 {
		t.Fatalf("test assumes the documented threshold of 3, constant is %d", UnavailableAfterConsecutiveFailures)
	}
	if err := repo.RecordFailure(ctx, "cedar", "timeout", failedAt); err != nil {
		t.Fatalf("record failure 3: %v", err)
	}
	afterThird, _, _ := repo.Get(ctx, "cedar")
	if afterThird.State != SyncStateUnavailable {
		t.Fatalf("state after reaching the threshold = %q, want %q", afterThird.State, SyncStateUnavailable)
	}
	if afterThird.ConsecutiveFailures != 3 {
		t.Fatalf("consecutive failures = %d, want 3", afterThird.ConsecutiveFailures)
	}
}

func TestSyncListReturnsAllSources(t *testing.T) {
	database := openTestDB(t)
	repo := NewSyncRepository(database)
	ctx := context.Background()

	if err := repo.SaveCursor(ctx, "cedar", "c1", time.Now()); err != nil {
		t.Fatalf("save cursor cedar: %v", err)
	}
	if err := repo.RecordFailure(ctx, "walnut", "network", time.Now()); err != nil {
		t.Fatalf("record failure walnut: %v", err)
	}

	states, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(states))
	}
	if states[0].SourceNodeID != "cedar" || states[1].SourceNodeID != "walnut" {
		t.Fatalf("unexpected sources: %+v", states)
	}
}
