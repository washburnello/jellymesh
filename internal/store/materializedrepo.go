package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Materialized is one playable item the destination has written into its
// generated root.
type Materialized struct {
	SourceNodeID string
	ItemID       string
	LibraryID    string
	// Reference is the random, non-authenticating identifier the item's .strm
	// names. It lets the relay find the item; the relay's allow-list and the
	// destination's policy decide whether it may be played.
	Reference string
	// Path is the .strm's path relative to the generated root.
	Path string
	// Checksum is of everything written for the item, so an unchanged item is
	// not rewritten.
	Checksum string
	// Revision is the catalog revision the item was written at.
	Revision uint64
}

// MaterializedRepository records what has been materialized.
type MaterializedRepository struct {
	database *DB
}

func NewMaterializedRepository(database *DB) *MaterializedRepository {
	return &MaterializedRepository{database: database}
}

// NewReference returns a fresh random reference.
func NewReference() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

// Save records or updates an item.
func (repo *MaterializedRepository) Save(ctx context.Context, item Materialized) error {
	_, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO materialized (source_node_id, item_id, library_id, reference, path, checksum, revision, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_node_id, item_id) DO UPDATE SET
			library_id = excluded.library_id, reference = excluded.reference,
			path = excluded.path, checksum = excluded.checksum, revision = excluded.revision`,
		item.SourceNodeID, item.ItemID, item.LibraryID, item.Reference, item.Path, item.Checksum, item.Revision, FormatTime(nowUTC()))
	if err != nil {
		return fmt.Errorf("record materialized item: %w", err)
	}
	return nil
}

// Remove forgets an item, revoking its reference. The reference is kept
// against the item's path, so that whatever next appears at that path can
// reuse it (ReferenceFor), but until then it resolves to nothing.
func (repo *MaterializedRepository) Remove(ctx context.Context, sourceNodeID string, itemID string) error {
	transaction, err := repo.database.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var path, reference string
	err = transaction.QueryRowContext(ctx, `SELECT path, reference FROM materialized WHERE source_node_id = ? AND item_id = ?`,
		sourceNodeID, itemID).Scan(&path, &reference)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove materialized item: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM materialized WHERE source_node_id = ? AND item_id = ?`, sourceNodeID, itemID); err != nil {
		return fmt.Errorf("remove materialized item: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO retired_references (path, reference, retired_at) VALUES (?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET reference = excluded.reference, retired_at = excluded.retired_at`,
		path, reference, FormatTime(nowUTC())); err != nil {
		return fmt.Errorf("retire reference: %w", err)
	}
	return transaction.Commit()
}

// RetiredReferenceLife is how long a withdrawn path's reference is kept for
// reuse. Jellyfin rescans on the operator's schedule, often hourly, so a
// week covers any schedule with room to spare.
const RetiredReferenceLife = 7 * 24 * time.Hour

// ReferenceFor returns the reference for a new item at path: the one path
// last held, if it was retired within RetiredReferenceLife, and otherwise a
// fresh one. A reused reference is taken out of retirement.
func (repo *MaterializedRepository) ReferenceFor(ctx context.Context, path string) (string, error) {
	cutoff := FormatTime(nowUTC().Add(-RetiredReferenceLife))
	if _, err := repo.database.SQL().ExecContext(ctx, `DELETE FROM retired_references WHERE retired_at < ?`, cutoff); err != nil {
		return "", fmt.Errorf("prune retired references: %w", err)
	}
	var reference string
	err := repo.database.SQL().QueryRowContext(ctx, `DELETE FROM retired_references WHERE path = ? RETURNING reference`, path).Scan(&reference)
	if err == nil {
		return reference, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("reuse reference: %w", err)
	}
	return NewReference()
}

// ByReference resolves a reference.
func (repo *MaterializedRepository) ByReference(ctx context.Context, reference string) (Materialized, bool, error) {
	row := repo.database.SQL().QueryRowContext(ctx, `
		SELECT source_node_id, item_id, library_id, reference, path, checksum, revision FROM materialized WHERE reference = ?`, reference)
	var item Materialized
	err := row.Scan(&item.SourceNodeID, &item.ItemID, &item.LibraryID, &item.Reference, &item.Path, &item.Checksum, &item.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Materialized{}, false, nil
	}
	if err != nil {
		return Materialized{}, false, fmt.Errorf("resolve reference: %w", err)
	}
	return item, true, nil
}

// All returns every materialized item.
func (repo *MaterializedRepository) All(ctx context.Context) ([]Materialized, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, `
		SELECT source_node_id, item_id, library_id, reference, path, checksum, revision FROM materialized ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("list materialized items: %w", err)
	}
	defer rows.Close()
	var items []Materialized
	for rows.Next() {
		var item Materialized
		if err := rows.Scan(&item.SourceNodeID, &item.ItemID, &item.LibraryID, &item.Reference, &item.Path, &item.Checksum, &item.Revision); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
