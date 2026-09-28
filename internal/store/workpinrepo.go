package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// WorkPin is the folder and identifiers a work was first materialized under.
type WorkPin struct {
	ID          int64
	Kind        string
	Folder      string
	Identifiers map[string]string
	LastUsed    time.Time
}

// WorkPinRepository keeps work pins.
type WorkPinRepository struct {
	database *DB
}

func NewWorkPinRepository(database *DB) *WorkPinRepository {
	return &WorkPinRepository{database: database}
}

// All returns every pin, oldest first.
func (repo *WorkPinRepository) All(ctx context.Context) ([]WorkPin, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, `SELECT id, kind, folder, identifiers, last_used FROM work_pins ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list work pins: %w", err)
	}
	defer rows.Close()
	var pins []WorkPin
	for rows.Next() {
		var pin WorkPin
		var identifiers, lastUsed string
		if err := rows.Scan(&pin.ID, &pin.Kind, &pin.Folder, &identifiers, &lastUsed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(identifiers), &pin.Identifiers); err != nil {
			return nil, fmt.Errorf("work pin %d: %w", pin.ID, err)
		}
		if pin.LastUsed, err = ParseTime(lastUsed); err != nil {
			return nil, fmt.Errorf("work pin %d: %w", pin.ID, err)
		}
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}

// Add records a new pin, last used at pin.LastUsed or else now, and returns
// it with its ID.
func (repo *WorkPinRepository) Add(ctx context.Context, pin WorkPin) (WorkPin, error) {
	identifiers, err := json.Marshal(pin.Identifiers)
	if err != nil {
		return pin, err
	}
	now := pin.LastUsed
	if now.IsZero() {
		now = nowUTC()
	}
	result, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO work_pins (kind, folder, identifiers, created_at, last_used) VALUES (?, ?, ?, ?, ?)`,
		pin.Kind, pin.Folder, string(identifiers), FormatTime(now), FormatTime(now))
	if err != nil {
		return pin, fmt.Errorf("add work pin: %w", err)
	}
	if pin.ID, err = result.LastInsertId(); err != nil {
		return pin, err
	}
	pin.LastUsed = now
	return pin, nil
}

// Touch marks pins as in use at now, writing only those last marked more
// than a day before, so that a pass does not rewrite every pin.
func (repo *WorkPinRepository) Touch(ctx context.Context, ids []int64, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	transaction, err := repo.database.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for _, id := range ids {
		if _, err := transaction.ExecContext(ctx, `UPDATE work_pins SET last_used = ? WHERE id = ? AND last_used < ?`,
			FormatTime(now), id, FormatTime(now.Add(-24*time.Hour))); err != nil {
			return fmt.Errorf("touch work pin: %w", err)
		}
	}
	return transaction.Commit()
}

// Prune forgets pins unused since before cutoff, and returns how many.
func (repo *WorkPinRepository) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := repo.database.SQL().ExecContext(ctx, `DELETE FROM work_pins WHERE last_used < ?`, FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune work pins: %w", err)
	}
	return result.RowsAffected()
}
