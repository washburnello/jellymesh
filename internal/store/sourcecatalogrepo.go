package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SourcePublication is one of this node's own publication decisions.
type SourcePublication struct {
	LibraryID      string
	Name           string
	CollectionType string
	Roots          []string
	Paused         bool
	PausedReason   string
	PublishedAt    string
	UpdatedAt      string
}

// SourceItem is one row of the source catalog.
type SourceItem struct {
	ItemID    string
	LibraryID string
	ParentID  string
	ItemType  string
	Revision  uint64
	Sequence  uint64
	ETag      string
	Checksum  string
	Tombstone bool
	Metadata  string
}

// SourceCatalogRepository stores the source catalog. It holds no rules: the
// sourcecatalog package decides what is published and when an item changes.
type SourceCatalogRepository struct {
	database *DB
}

func NewSourceCatalogRepository(database *DB) *SourceCatalogRepository {
	return &SourceCatalogRepository{database: database}
}

// SavePublication records or replaces a publication decision.
func (repo *SourceCatalogRepository) SavePublication(ctx context.Context, publication SourcePublication) error {
	roots, err := json.Marshal(publication.Roots)
	if err != nil {
		return err
	}
	state := "published"
	if publication.Paused {
		state = "paused"
	}
	now := FormatTime(nowUTC())
	if publication.PublishedAt == "" {
		publication.PublishedAt = now
	}
	_, err = repo.database.SQL().ExecContext(ctx, `
		INSERT INTO source_publications (library_id, name, collection_type, roots, state, paused_reason, published_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(library_id) DO UPDATE SET
			name = excluded.name, collection_type = excluded.collection_type, roots = excluded.roots,
			state = excluded.state, paused_reason = excluded.paused_reason, updated_at = excluded.updated_at`,
		publication.LibraryID, publication.Name, publication.CollectionType, string(roots), state,
		publication.PausedReason, publication.PublishedAt, now)
	if err != nil {
		return fmt.Errorf("save publication: %w", err)
	}
	return nil
}

// DeletePublication removes a publication decision.
func (repo *SourceCatalogRepository) DeletePublication(ctx context.Context, libraryID string) error {
	if _, err := repo.database.SQL().ExecContext(ctx, `DELETE FROM source_publications WHERE library_id = ?`, libraryID); err != nil {
		return fmt.Errorf("delete publication: %w", err)
	}
	return nil
}

// Publications returns every publication decision, paused ones included.
func (repo *SourceCatalogRepository) Publications(ctx context.Context) ([]SourcePublication, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, `
		SELECT library_id, name, collection_type, roots, state, paused_reason, published_at, updated_at
		FROM source_publications ORDER BY library_id`)
	if err != nil {
		return nil, fmt.Errorf("list publications: %w", err)
	}
	defer rows.Close()
	var publications []SourcePublication
	for rows.Next() {
		var publication SourcePublication
		var roots, state string
		if err := rows.Scan(&publication.LibraryID, &publication.Name, &publication.CollectionType, &roots, &state,
			&publication.PausedReason, &publication.PublishedAt, &publication.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(roots), &publication.Roots); err != nil {
			return nil, fmt.Errorf("decode roots: %w", err)
		}
		publication.Paused = state == "paused"
		publications = append(publications, publication)
	}
	return publications, rows.Err()
}

// Item returns one catalog row.
func (repo *SourceCatalogRepository) Item(ctx context.Context, itemID string) (SourceItem, bool, error) {
	row := repo.database.SQL().QueryRowContext(ctx, sourceItemColumns+` FROM source_items WHERE item_id = ?`, itemID)
	item, err := scanSourceItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceItem{}, false, nil
	}
	return item, err == nil, err
}

// LiveItems returns a library's rows that are not tombstones.
func (repo *SourceCatalogRepository) LiveItems(ctx context.Context, libraryID string) ([]SourceItem, error) {
	return repo.query(ctx, sourceItemColumns+` FROM source_items WHERE library_id = ? AND tombstone = 0 ORDER BY sequence`, libraryID)
}

// Changes returns up to limit rows after sequence, in sequence order.
func (repo *SourceCatalogRepository) Changes(ctx context.Context, after uint64, limit int) ([]SourceItem, error) {
	return repo.query(ctx, sourceItemColumns+` FROM source_items WHERE sequence > ? ORDER BY sequence LIMIT ?`, after, limit)
}

// CountLive returns how many live items a library has.
func (repo *SourceCatalogRepository) CountLive(ctx context.Context, libraryID string) (int, error) {
	var count int
	err := repo.database.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_items WHERE library_id = ? AND tombstone = 0`, libraryID).Scan(&count)
	return count, err
}

// Record writes a row with the next change sequence, in one transaction, and
// returns the sequence it took. Every change to the catalog goes through it,
// so sequences only ever increase.
func (repo *SourceCatalogRepository) Record(ctx context.Context, item SourceItem) (uint64, error) {
	var sequence uint64
	err := repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM source_items`).Scan(&sequence); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO source_items (item_id, library_id, parent_id, item_type, revision, sequence, etag, checksum, tombstone, metadata, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(item_id) DO UPDATE SET
				library_id = excluded.library_id, parent_id = excluded.parent_id, item_type = excluded.item_type,
				revision = excluded.revision, sequence = excluded.sequence, etag = excluded.etag,
				checksum = excluded.checksum, tombstone = excluded.tombstone, metadata = excluded.metadata,
				updated_at = excluded.updated_at`,
			item.ItemID, item.LibraryID, item.ParentID, item.ItemType, item.Revision, sequence, item.ETag,
			item.Checksum, boolToInt(item.Tombstone), item.Metadata, FormatTime(nowUTC()))
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("record catalog change: %w", err)
	}
	return sequence, nil
}

const sourceItemColumns = `SELECT item_id, library_id, parent_id, item_type, revision, sequence, etag, checksum, tombstone, metadata`

func (repo *SourceCatalogRepository) query(ctx context.Context, statement string, args ...any) ([]SourceItem, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query catalog: %w", err)
	}
	defer rows.Close()
	var items []SourceItem
	for rows.Next() {
		item, err := scanSourceItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanSourceItem(row rowScanner) (SourceItem, error) {
	var item SourceItem
	var tombstone int
	err := row.Scan(&item.ItemID, &item.LibraryID, &item.ParentID, &item.ItemType, &item.Revision, &item.Sequence,
		&item.ETag, &item.Checksum, &tombstone, &item.Metadata)
	item.Tombstone = tombstone != 0
	return item, err
}
