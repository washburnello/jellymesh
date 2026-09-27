package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// RemoteItem is the destination's record of one item a source offers.
type RemoteItem struct {
	SourceNodeID string
	ItemID       string
	LibraryID    string
	ParentID     string
	ItemType     string
	Revision     uint64
	Metadata     string
}

// RemoteOperation is one change to apply to the destination catalog: an
// upsert, or, when Remove is set, a withdrawal that is retained for the grace
// period with the item's last known metadata.
type RemoteOperation struct {
	Item          RemoteItem
	Remove        bool
	LogicalWorkID string
}

// RemoteCatalogRepository stores the destination's view of each source's
// catalog. The remotecatalog package decides what to apply; this applies it
// atomically together with the cursor, so that a page is either wholly
// applied and acknowledged or not applied at all.
type RemoteCatalogRepository struct {
	database *DB
	grace    time.Duration
}

func NewRemoteCatalogRepository(database *DB, grace time.Duration) *RemoteCatalogRepository {
	return &RemoteCatalogRepository{database: database, grace: grace}
}

// Revisions returns the stored revision of each of the given items.
func (repo *RemoteCatalogRepository) Revisions(ctx context.Context, sourceNodeID string, itemIDs []string) (map[string]RemoteItem, error) {
	found := map[string]RemoteItem{}
	for _, itemID := range itemIDs {
		var item RemoteItem
		err := repo.database.SQL().QueryRowContext(ctx, `
			SELECT source_node_id, item_id, library_id, parent_id, item_type, revision, metadata
			FROM remote_items WHERE source_node_id = ? AND item_id = ?`, sourceNodeID, itemID,
		).Scan(&item.SourceNodeID, &item.ItemID, &item.LibraryID, &item.ParentID, &item.ItemType, &item.Revision, &item.Metadata)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read remote item: %w", err)
		}
		found[itemID] = item
	}
	return found, nil
}

// Apply writes operations and advances the source's cursor in one
// transaction.
func (repo *RemoteCatalogRepository) Apply(ctx context.Context, sourceNodeID string, operations []RemoteOperation, cursor uint64, at time.Time) error {
	return repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		now := FormatTime(at)
		for _, operation := range operations {
			item := operation.Item
			if operation.Remove {
				if _, err := tx.ExecContext(ctx, `DELETE FROM remote_items WHERE source_node_id = ? AND item_id = ?`, sourceNodeID, item.ItemID); err != nil {
					return fmt.Errorf("remove remote item: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO retention (source_node_id, source_library_id, source_item_id, logical_work_id, deleted_at, expires_at, metadata)
					VALUES (?, ?, ?, ?, ?, ?, ?)
					ON CONFLICT (source_node_id, source_library_id, source_item_id) DO UPDATE SET
						logical_work_id = excluded.logical_work_id, deleted_at = excluded.deleted_at,
						expires_at = excluded.expires_at, metadata = excluded.metadata`,
					sourceNodeID, item.LibraryID, item.ItemID, operation.LogicalWorkID, now,
					FormatTime(at.Add(repo.grace)), item.Metadata); err != nil {
					return fmt.Errorf("retain remote item: %w", err)
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO remote_items (source_node_id, item_id, library_id, parent_id, item_type, revision, metadata, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (source_node_id, item_id) DO UPDATE SET
					library_id = excluded.library_id, parent_id = excluded.parent_id, item_type = excluded.item_type,
					revision = excluded.revision, metadata = excluded.metadata, updated_at = excluded.updated_at`,
				sourceNodeID, item.ItemID, item.LibraryID, item.ParentID, item.ItemType, item.Revision, item.Metadata, now); err != nil {
				return fmt.Errorf("upsert remote item: %w", err)
			}
			// An item that returns is live again, and no longer retained.
			if _, err := tx.ExecContext(ctx, `DELETE FROM retention WHERE source_node_id = ? AND source_item_id = ?`, sourceNodeID, item.ItemID); err != nil {
				return fmt.Errorf("discard retention: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sync_cursors(source_node_id, cursor, state, last_success_at, last_failure_at, failure_category, consecutive_failures)
			VALUES (?, ?, ?, ?, '', '', 0)
			ON CONFLICT(source_node_id) DO UPDATE SET
				cursor = excluded.cursor, state = excluded.state,
				last_success_at = excluded.last_success_at, consecutive_failures = 0`,
			sourceNodeID, strconv.FormatUint(cursor, 10), SyncStateAvailable, now)
		if err != nil {
			return fmt.Errorf("advance cursor: %w", err)
		}
		return nil
	})
}

// RemoveLibrary drops every record of one source library, as when the
// destination opts out of it. Opting out is a decision about visibility, not
// a deletion at the source, so nothing is retained.
func (repo *RemoteCatalogRepository) RemoveLibrary(ctx context.Context, sourceNodeID string, libraryID string) (int, error) {
	result, err := repo.database.SQL().ExecContext(ctx,
		`DELETE FROM remote_items WHERE source_node_id = ? AND library_id = ?`, sourceNodeID, libraryID)
	if err != nil {
		return 0, fmt.Errorf("remove remote library: %w", err)
	}
	removed, _ := result.RowsAffected()
	return int(removed), nil
}

// RemoveSource drops every record of a source, as when it leaves or is
// ejected. Its retention records are kept for the grace period.
func (repo *RemoteCatalogRepository) RemoveSource(ctx context.Context, sourceNodeID string) error {
	if _, err := repo.database.SQL().ExecContext(ctx, `DELETE FROM remote_items WHERE source_node_id = ?`, sourceNodeID); err != nil {
		return fmt.Errorf("remove remote source: %w", err)
	}
	return nil
}

// Items returns a source's records, optionally for one library.
func (repo *RemoteCatalogRepository) Items(ctx context.Context, sourceNodeID string, libraryID string) ([]RemoteItem, error) {
	statement := `SELECT source_node_id, item_id, library_id, parent_id, item_type, revision, metadata FROM remote_items WHERE source_node_id = ?`
	args := []any{sourceNodeID}
	if libraryID != "" {
		statement += ` AND library_id = ?`
		args = append(args, libraryID)
	}
	rows, err := repo.database.SQL().QueryContext(ctx, statement+` ORDER BY item_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list remote items: %w", err)
	}
	defer rows.Close()
	var items []RemoteItem
	for rows.Next() {
		var item RemoteItem
		if err := rows.Scan(&item.SourceNodeID, &item.ItemID, &item.LibraryID, &item.ParentID, &item.ItemType, &item.Revision, &item.Metadata); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ResetCursor makes the next sync of a source start from the beginning, as
// when the destination opts back in to one of its libraries. Items it already
// holds come back at the same revision and change nothing.
func (repo *RemoteCatalogRepository) ResetCursor(ctx context.Context, sourceNodeID string) error {
	_, err := repo.database.SQL().ExecContext(ctx, `UPDATE sync_cursors SET cursor = '0' WHERE source_node_id = ?`, sourceNodeID)
	return err
}
