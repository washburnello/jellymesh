package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"jellymesh/internal/history"
)

// HistoryRepository is the durable equivalent of history.Ledger. It applies
// the identical validation and merge semantics, but backs them with the
// history table so a user's watched state survives a node restart instead of
// living only in a process's memory.
type HistoryRepository struct {
	database *DB
}

// NewHistoryRepository wraps database with the history-ledger access pattern.
func NewHistoryRepository(database *DB) *HistoryRepository {
	return &HistoryRepository{database: database}
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so the same decode
// logic serves single-row lookups and multi-row scans.
// validateHistoryEntry applies the same checks history.Ledger.Save applies,
// trimming identity fields in place and returning history's own sentinel
// errors so callers can keep using errors.Is unchanged regardless of which
// backing store they are talking to.
func validateHistoryEntry(entry *history.Entry) error {
	entry.UserID = strings.TrimSpace(entry.UserID)
	entry.WorkID = strings.TrimSpace(entry.WorkID)
	entry.MediaType = strings.TrimSpace(entry.MediaType)
	entry.ProviderIdentity = strings.TrimSpace(entry.ProviderIdentity)
	if entry.UserID == "" {
		return history.ErrUserIDRequired
	}
	if entry.WorkID == "" {
		return history.ErrWorkIDRequired
	}
	if entry.ResumePositionTicks < 0 {
		return history.ErrInvalidPosition
	}
	if entry.PlayCount < 0 {
		return history.ErrInvalidPlayCount
	}
	return nil
}

// Save upserts entry, keyed by (UserID, WorkID) exactly as the in-memory
// ledger keys its outer and inner maps.
func (repo *HistoryRepository) Save(ctx context.Context, entry history.Entry) error {
	if err := validateHistoryEntry(&entry); err != nil {
		return err
	}
	// Matches history.Ledger.Save: an unset timestamp means "as of now".
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = nowUTC()
	}
	_, err := repo.database.SQL().ExecContext(ctx, upsertHistorySQL,
		entry.UserID, entry.WorkID, entry.MediaType, entry.ProviderIdentity,
		boolToInt(entry.Played), entry.PlayCount, entry.ResumePositionTicks,
		FormatTime(entry.LastPlayedAt), FormatTime(entry.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("save history entry: %w", err)
	}
	return nil
}

// Get returns the stored entry for a user's logical work, mirroring
// Ledger.Get including its found-or-not-found boolean.
func (repo *HistoryRepository) Get(ctx context.Context, userID string, workID string) (history.Entry, bool, error) {
	userID = strings.TrimSpace(userID)
	workID = strings.TrimSpace(workID)
	row := repo.database.SQL().QueryRowContext(ctx, `
		SELECT local_user_id, work_id, media_type, provider_identity, played,
		       play_count, resume_position_ticks, last_played_at, updated_at
		FROM history WHERE local_user_id = ? AND work_id = ?`, userID, workID)
	entry, err := scanHistoryEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return history.Entry{}, false, nil
	}
	if err != nil {
		return history.Entry{}, false, fmt.Errorf("get history entry: %w", err)
	}
	return entry, true, nil
}

// WorksForUser returns every logical work the given user has history for,
// mirroring Ledger.WorksForUser.
func (repo *HistoryRepository) WorksForUser(ctx context.Context, userID string) ([]history.Entry, error) {
	userID = strings.TrimSpace(userID)
	rows, err := repo.database.SQL().QueryContext(ctx, `
		SELECT local_user_id, work_id, media_type, provider_identity, played,
		       play_count, resume_position_ticks, last_played_at, updated_at
		FROM history WHERE local_user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("query works for user: %w", err)
	}
	defer rows.Close()

	entries := make([]history.Entry, 0)
	for rows.Next() {
		entry, err := scanHistoryEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan history entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// Restore reapplies preserved history to a work that has reappeared.
//
// The direction matters and is easy to invert. The history table IS the
// ledger, so the stored row is the *preserved* side. The authoritative side is
// the caller's local entry, which reflects what the local Jellyfin currently
// holds for this user and work. The specification is explicit that local
// Jellyfin user data remains the source of truth and that the ledger only
// fills gaps, so a stale ledger row must never win over fresher local
// progress.
//
// The returned entry is what the caller should write back to Jellyfin. The
// ledger is updated to the merged result in the same transaction so it stays
// current for the next restoration. The boolean reports whether a preserved
// row existed at all; when it did not, the local entry is returned unchanged.
func (repo *HistoryRepository) Restore(ctx context.Context, local history.Entry) (history.Entry, bool, error) {
	// Deliberately no timestamp defaulting here, unlike Save. A zero UpdatedAt
	// on the local entry means "the local server holds no recorded state for
	// this work", which is precisely the case restoration exists to serve.
	// Stamping it with the current time would make every freshly-read local
	// entry appear newer than any preserved row, and the guard that protects
	// newer local progress would then suppress every restoration. That failure
	// is silent: the ledger would look healthy and simply never reapply
	// anything.
	if err := validateHistoryEntry(&local); err != nil {
		return history.Entry{}, false, err
	}

	var merged history.Entry
	var found bool
	err := repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT local_user_id, work_id, media_type, provider_identity, played,
			       play_count, resume_position_ticks, last_played_at, updated_at
			FROM history WHERE local_user_id = ? AND work_id = ?`, local.UserID, local.WorkID)

		preserved, err := scanHistoryEntry(row)
		switch {
		case err == nil:
			found = true
		case errors.Is(err, sql.ErrNoRows):
			// Nothing was preserved for this work, so there is nothing to
			// reapply. The local entry stands as-is.
			merged = local
			return nil
		default:
			return fmt.Errorf("read preserved history entry: %w", err)
		}

		result, err := history.MergePreservingLocal(local, preserved)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, upsertHistorySQL,
			result.UserID, result.WorkID, result.MediaType, result.ProviderIdentity,
			boolToInt(result.Played), result.PlayCount, result.ResumePositionTicks,
			FormatTime(result.LastPlayedAt), FormatTime(result.UpdatedAt),
		); err != nil {
			return fmt.Errorf("save merged history entry: %w", err)
		}
		merged = result
		return nil
	})
	if err != nil {
		return history.Entry{}, false, err
	}
	return merged, found, nil
}

const upsertHistorySQL = `
	INSERT INTO history (
		local_user_id, work_id, media_type, provider_identity, played,
		play_count, resume_position_ticks, last_played_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (local_user_id, work_id) DO UPDATE SET
		media_type            = excluded.media_type,
		provider_identity     = excluded.provider_identity,
		played                = excluded.played,
		play_count            = excluded.play_count,
		resume_position_ticks = excluded.resume_position_ticks,
		last_played_at        = excluded.last_played_at,
		updated_at            = excluded.updated_at`

func scanHistoryEntry(scanner rowScanner) (history.Entry, error) {
	var entry history.Entry
	var played int
	var lastPlayedAt, updatedAt string
	if err := scanner.Scan(
		&entry.UserID, &entry.WorkID, &entry.MediaType, &entry.ProviderIdentity, &played,
		&entry.PlayCount, &entry.ResumePositionTicks, &lastPlayedAt, &updatedAt,
	); err != nil {
		return history.Entry{}, err
	}
	entry.Played = played != 0

	var err error
	entry.LastPlayedAt, err = ParseTime(lastPlayedAt)
	if err != nil {
		return history.Entry{}, fmt.Errorf("parse last_played_at: %w", err)
	}
	entry.UpdatedAt, err = ParseTime(updatedAt)
	if err != nil {
		return history.Entry{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return entry, nil
}
