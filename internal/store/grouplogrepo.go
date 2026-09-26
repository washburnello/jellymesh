package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"jellymesh/internal/grouplog"
)

// GroupLogRepository stores group logs.
//
// It holds no rules of its own. Load replays and re-verifies every stored
// event through internal/grouplog, so a tampered database produces an error
// rather than a trusted state, and Save makes the stored copy match a log
// that grouplog has already verified.
type GroupLogRepository struct {
	database *DB
}

func NewGroupLogRepository(database *DB) *GroupLogRepository {
	return &GroupLogRepository{database: database}
}

// Load returns the stored log for groupID, verified from genesis. The boolean
// reports whether any log is stored. A recorded equivocation is restored, so a
// halted log stays halted across a restart.
func (repo *GroupLogRepository) Load(ctx context.Context, groupID string) (*grouplog.Log, bool, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, false, ErrMembershipGroupIDRequired
	}
	rows, err := repo.database.SQL().QueryContext(ctx,
		`SELECT sequence, event FROM group_log WHERE group_id = ? ORDER BY sequence`, groupID)
	if err != nil {
		return nil, false, fmt.Errorf("load group log: %w", err)
	}
	var events []grouplog.Event
	for rows.Next() {
		var sequence uint64
		var encoded string
		if err := rows.Scan(&sequence, &encoded); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan group log: %w", err)
		}
		event, err := grouplog.UnmarshalEvent([]byte(encoded))
		if err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("event %d: %w", sequence, err)
		}
		if event.Sequence != sequence {
			rows.Close()
			return nil, false, fmt.Errorf("event stored at %d claims sequence %d", sequence, event.Sequence)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, fmt.Errorf("load group log: %w", err)
	}
	rows.Close()
	if len(events) == 0 {
		return nil, false, nil
	}

	log, err := grouplog.Replay(events)
	if err != nil {
		return nil, false, fmt.Errorf("verify stored group log: %w", err)
	}

	var existing, received string
	err = repo.database.SQL().QueryRowContext(ctx,
		`SELECT existing, received FROM group_log_evidence WHERE group_id = ?`, groupID,
	).Scan(&existing, &received)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, false, fmt.Errorf("load equivocation evidence: %w", err)
	default:
		evidence, err := decodeEvidence(existing, received)
		if err != nil {
			return nil, false, err
		}
		if err := log.RestoreHalt(evidence); err != nil {
			return nil, false, err
		}
	}
	return log, true, nil
}

// Save makes the stored log match log, in one transaction. Events the store
// holds beyond the point where the two diverge, which only a succession
// causes, are moved to group_log_superseded rather than deleted. Save must
// complete before the owner publishes an event it has just sequenced: an
// owner that published an event it had not stored could, after a crash,
// sequence a different event for the same slot.
func (repo *GroupLogRepository) Save(ctx context.Context, log *grouplog.Log) error {
	if log == nil {
		return errors.New("a group log is required")
	}
	groupID := log.State().GroupID
	events := log.EventsAfter(0)

	return repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		stored, err := storedHashes(ctx, tx, groupID)
		if err != nil {
			return err
		}

		common := 0
		for common < len(stored) && common < len(events) && stored[common].hash == events[common].Hash().String() {
			common++
		}

		now := FormatTime(nowUTC())
		for _, row := range stored[common:] {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO group_log_superseded (group_id, hash, sequence, event, superseded_at)
				SELECT group_id, hash, sequence, event, ? FROM group_log WHERE group_id = ? AND sequence = ?`,
				now, groupID, row.sequence,
			); err != nil {
				return fmt.Errorf("keep superseded event %d: %w", row.sequence, err)
			}
		}
		if len(stored) > common {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM group_log WHERE group_id = ? AND sequence > ?`, groupID, common,
			); err != nil {
				return fmt.Errorf("remove superseded events: %w", err)
			}
		}

		for _, event := range events[common:] {
			encoded, err := event.Marshal()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO group_log (group_id, sequence, epoch, hash, event) VALUES (?, ?, ?, ?, ?)`,
				groupID, event.Sequence, event.Epoch, event.Hash().String(), string(encoded),
			); err != nil {
				return fmt.Errorf("store event %d: %w", event.Sequence, err)
			}
		}

		if evidence, halted := log.Halted(); halted {
			existing, err := evidence.Existing.Marshal()
			if err != nil {
				return err
			}
			received, err := evidence.Received.Marshal()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO group_log_evidence (group_id, existing, received, detected_at)
				VALUES (?, ?, ?, ?)`,
				groupID, string(existing), string(received), now,
			); err != nil {
				return fmt.Errorf("store equivocation evidence: %w", err)
			}
		}
		return nil
	})
}

const sequencingHoldFlag = "sequencing_hold"

// HoldSequencing records that this node must not sequence until it has
// confirmed it holds the latest log. Restoring from a backup sets it.
func (repo *GroupLogRepository) HoldSequencing(ctx context.Context) error {
	_, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO node_flags (name, value, set_at) VALUES (?, '1', ?)
		ON CONFLICT(name) DO UPDATE SET value = '1', set_at = excluded.set_at`,
		sequencingHoldFlag, FormatTime(nowUTC()))
	if err != nil {
		return fmt.Errorf("hold sequencing: %w", err)
	}
	return nil
}

// SequencingHeld reports whether the hold is in place.
func (repo *GroupLogRepository) SequencingHeld(ctx context.Context) (bool, error) {
	var held int
	err := repo.database.SQL().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM node_flags WHERE name = ? AND value = '1')`, sequencingHoldFlag).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("read sequencing hold: %w", err)
	}
	return held != 0, nil
}

// ReleaseSequencing lifts the hold.
func (repo *GroupLogRepository) ReleaseSequencing(ctx context.Context) error {
	if _, err := repo.database.SQL().ExecContext(ctx, `DELETE FROM node_flags WHERE name = ?`, sequencingHoldFlag); err != nil {
		return fmt.Errorf("release sequencing hold: %w", err)
	}
	return nil
}

// ListGroupIDs returns every group this node holds a log for, for a node
// enumerating what it belongs to on startup.
func (repo *GroupLogRepository) ListGroupIDs(ctx context.Context) ([]string, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, `SELECT DISTINCT group_id FROM group_log ORDER BY group_id`)
	if err != nil {
		return nil, fmt.Errorf("list group ids: %w", err)
	}
	defer rows.Close()
	var groupIDs []string
	for rows.Next() {
		var groupID string
		if err := rows.Scan(&groupID); err != nil {
			return nil, fmt.Errorf("scan group id: %w", err)
		}
		groupIDs = append(groupIDs, groupID)
	}
	return groupIDs, rows.Err()
}

// Superseded returns the events a succession removed from this node's log.
func (repo *GroupLogRepository) Superseded(ctx context.Context, groupID string) ([]grouplog.Event, error) {
	rows, err := repo.database.SQL().QueryContext(ctx,
		`SELECT event FROM group_log_superseded WHERE group_id = ? ORDER BY sequence`, strings.TrimSpace(groupID))
	if err != nil {
		return nil, fmt.Errorf("load superseded events: %w", err)
	}
	defer rows.Close()
	var events []grouplog.Event
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan superseded event: %w", err)
		}
		event, err := grouplog.UnmarshalEvent([]byte(encoded))
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

type storedHash struct {
	sequence uint64
	hash     string
}

func storedHashes(ctx context.Context, tx *sql.Tx, groupID string) ([]storedHash, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT sequence, hash FROM group_log WHERE group_id = ? ORDER BY sequence`, groupID)
	if err != nil {
		return nil, fmt.Errorf("read stored log: %w", err)
	}
	defer rows.Close()
	var hashes []storedHash
	for rows.Next() {
		var row storedHash
		if err := rows.Scan(&row.sequence, &row.hash); err != nil {
			return nil, fmt.Errorf("scan stored log: %w", err)
		}
		hashes = append(hashes, row)
	}
	return hashes, rows.Err()
}

func decodeEvidence(existing string, received string) (grouplog.Equivocation, error) {
	first, err := grouplog.UnmarshalEvent([]byte(existing))
	if err != nil {
		return grouplog.Equivocation{}, fmt.Errorf("equivocation evidence: %w", err)
	}
	second, err := grouplog.UnmarshalEvent([]byte(received))
	if err != nil {
		return grouplog.Equivocation{}, fmt.Errorf("equivocation evidence: %w", err)
	}
	return grouplog.Equivocation{Existing: first, Received: second}, nil
}
