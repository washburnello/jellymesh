package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
		INSERT INTO materialized (source_node_id, item_id, library_id, reference, path, checksum, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_node_id, item_id) DO UPDATE SET
			library_id = excluded.library_id, reference = excluded.reference,
			path = excluded.path, checksum = excluded.checksum`,
		item.SourceNodeID, item.ItemID, item.LibraryID, item.Reference, item.Path, item.Checksum, FormatTime(nowUTC()))
	if err != nil {
		return fmt.Errorf("record materialized item: %w", err)
	}
	return nil
}

// Remove forgets an item, revoking its reference.
func (repo *MaterializedRepository) Remove(ctx context.Context, sourceNodeID string, itemID string) error {
	if _, err := repo.database.SQL().ExecContext(ctx,
		`DELETE FROM materialized WHERE source_node_id = ? AND item_id = ?`, sourceNodeID, itemID); err != nil {
		return fmt.Errorf("remove materialized item: %w", err)
	}
	return nil
}

// ByReference resolves a reference.
func (repo *MaterializedRepository) ByReference(ctx context.Context, reference string) (Materialized, bool, error) {
	row := repo.database.SQL().QueryRowContext(ctx, `
		SELECT source_node_id, item_id, library_id, reference, path, checksum FROM materialized WHERE reference = ?`, reference)
	var item Materialized
	err := row.Scan(&item.SourceNodeID, &item.ItemID, &item.LibraryID, &item.Reference, &item.Path, &item.Checksum)
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
		SELECT source_node_id, item_id, library_id, reference, path, checksum FROM materialized ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("list materialized items: %w", err)
	}
	defer rows.Close()
	var items []Materialized
	for rows.Next() {
		var item Materialized
		if err := rows.Scan(&item.SourceNodeID, &item.ItemID, &item.LibraryID, &item.Reference, &item.Path, &item.Checksum); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
