package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sync state values. "unknown" is the schema default for a source that has
// never reported in either direction; "available", "degraded", and
// "unavailable" mirror the health states the sync scheduler
// (internal/syncpolicy) reasons about.
const (
	SyncStateUnknown     = "unknown"
	SyncStateAvailable   = "available"
	SyncStateDegraded    = "degraded"
	SyncStateUnavailable = "unavailable"
)

// UnavailableAfterConsecutiveFailures is the heartbeat hysteresis the design
// calls for: a single failed request must never be enough to mark a source
// offline, since a lone dropped packet or a momentary restart is ordinary
// network weather, not an outage. Only the third consecutive failure moves a
// source from "degraded" to "unavailable"; every one of the first two only
// degrades it.
const UnavailableAfterConsecutiveFailures = 3

var ErrSourceNodeIDRequired = errors.New("a source node ID is required")

// SyncState is the durable record of one peer's sync cursor and health, kept
// so a restart does not lose either "where we left off" or "how well this
// source has been behaving."
type SyncState struct {
	SourceNodeID        string
	Cursor              string
	State               string
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
	FailureCategory     string
	ConsecutiveFailures int
}

// SyncRepository is the durable store of per-source sync cursors and health.
type SyncRepository struct {
	database *DB
}

// NewSyncRepository builds a SyncRepository over an already-open database.
func NewSyncRepository(database *DB) *SyncRepository {
	return &SyncRepository{database: database}
}

// Get looks up the sync state for one source node.
func (repo *SyncRepository) Get(ctx context.Context, sourceNodeID string) (SyncState, bool, error) {
	row := repo.database.SQL().QueryRowContext(ctx, syncSelectColumns+` FROM sync_cursors WHERE source_node_id = ?`, sourceNodeID)
	state, err := scanSyncState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncState{}, false, nil
	}
	if err != nil {
		return SyncState{}, false, err
	}
	return state, true, nil
}

// SaveCursor records a successful sync: the new cursor, state available,
// last_success_at, and a reset failure count. It does not touch
// last_failure_at or failure_category, which remain the historical record of
// the most recent failure even after recovery.
//
// It upserts, so the very first successful sync against a source that has no
// row yet still succeeds.
func (repo *SyncRepository) SaveCursor(ctx context.Context, sourceNodeID, cursor string, succeededAt time.Time) error {
	sourceNodeID = strings.TrimSpace(sourceNodeID)
	if sourceNodeID == "" {
		return ErrSourceNodeIDRequired
	}
	_, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO sync_cursors(source_node_id, cursor, state, last_success_at, last_failure_at, failure_category, consecutive_failures)
		VALUES (?, ?, ?, ?, '', '', 0)
		ON CONFLICT(source_node_id) DO UPDATE SET
			cursor               = excluded.cursor,
			state                = excluded.state,
			last_success_at      = excluded.last_success_at,
			consecutive_failures = 0
		`,
		sourceNodeID, cursor, SyncStateAvailable, FormatTime(succeededAt),
	)
	if err != nil {
		return fmt.Errorf("save sync cursor: %w", err)
	}
	return nil
}

// RecordFailure records a failed sync attempt: it increments
// consecutive_failures, sets last_failure_at and failure_category, and
// deliberately leaves the cursor untouched. A failed sync is never a reason
// to discard known-good state; the last successful cursor must survive so
// the next successful sync can resume from it rather than starting over.
//
// The resulting state is "degraded" for the first
// UnavailableAfterConsecutiveFailures-1 consecutive failures, and
// "unavailable" once the threshold is reached.
//
// It upserts, so the first failure ever recorded against a source still
// succeeds, starting its consecutive_failures at 1.
func (repo *SyncRepository) RecordFailure(ctx context.Context, sourceNodeID, category string, failedAt time.Time) error {
	sourceNodeID = strings.TrimSpace(sourceNodeID)
	if sourceNodeID == "" {
		return ErrSourceNodeIDRequired
	}
	_, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO sync_cursors(source_node_id, cursor, state, last_success_at, last_failure_at, failure_category, consecutive_failures)
		VALUES (?, '', ?, '', ?, ?, 1)
		ON CONFLICT(source_node_id) DO UPDATE SET
			last_failure_at      = excluded.last_failure_at,
			failure_category     = excluded.failure_category,
			consecutive_failures = sync_cursors.consecutive_failures + 1,
			state                = CASE
				WHEN sync_cursors.consecutive_failures + 1 >= ? THEN ?
				ELSE ?
			END
		`,
		sourceNodeID, degradedOrUnavailable(1), FormatTime(failedAt), category,
		UnavailableAfterConsecutiveFailures, SyncStateUnavailable, SyncStateDegraded,
	)
	if err != nil {
		return fmt.Errorf("record sync failure: %w", err)
	}
	return nil
}

// degradedOrUnavailable classifies a resulting consecutive-failure count into
// the state that belongs to it, used for the fresh-row INSERT branch where
// there is no prior row for the UPDATE's CASE expression to evaluate.
func degradedOrUnavailable(consecutiveFailures int) string {
	if consecutiveFailures >= UnavailableAfterConsecutiveFailures {
		return SyncStateUnavailable
	}
	return SyncStateDegraded
}

// List returns the sync state of every known source.
func (repo *SyncRepository) List(ctx context.Context) ([]SyncState, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, syncSelectColumns+` FROM sync_cursors ORDER BY source_node_id`)
	if err != nil {
		return nil, fmt.Errorf("list sync state: %w", err)
	}
	defer rows.Close()

	var states []SyncState
	for rows.Next() {
		state, err := scanSyncState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sync state: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sync state: %w", err)
	}
	return states, nil
}

const syncSelectColumns = `SELECT source_node_id, cursor, state, last_success_at, last_failure_at, failure_category, consecutive_failures`

func scanSyncState(row rowScanner) (SyncState, error) {
	var (
		state                          SyncState
		lastSuccessRaw, lastFailureRaw string
	)
	if err := row.Scan(&state.SourceNodeID, &state.Cursor, &state.State,
		&lastSuccessRaw, &lastFailureRaw, &state.FailureCategory, &state.ConsecutiveFailures); err != nil {
		return SyncState{}, err
	}

	lastSuccessAt, err := ParseTime(lastSuccessRaw)
	if err != nil {
		return SyncState{}, fmt.Errorf("parse last_success_at: %w", err)
	}
	state.LastSuccessAt = lastSuccessAt

	lastFailureAt, err := ParseTime(lastFailureRaw)
	if err != nil {
		return SyncState{}, fmt.Errorf("parse last_failure_at: %w", err)
	}
	state.LastFailureAt = lastFailureAt

	return state, nil
}
