package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"jellymesh/internal/catalog"
)

// RetentionRepository is the durable equivalent of catalog.RetentionStore: a
// deleted-item grace period that must survive a restart, because the whole
// point of the grace period is to outlast the moment the deletion happened.
type RetentionRepository struct {
	database *DB
	policy   catalog.RetentionPolicy
}

// NewRetentionRepository wraps database with the retention access pattern,
// applying the same policy validation as catalog.NewRetentionStore.
func NewRetentionRepository(database *DB, policy catalog.RetentionPolicy) (*RetentionRepository, error) {
	if policy.GracePeriod <= 0 {
		return nil, catalog.ErrInvalidRetentionPolicy
	}
	return &RetentionRepository{database: database, policy: policy}, nil
}

// RecordDeletion stores a grace-period record for an item removed at its
// source, defaulting a zero deletedAt to now and computing ExpiresAt as
// deletedAt plus the repository's grace period, exactly as
// RetentionStore.RecordDeletion does.
func (repo *RetentionRepository) RecordDeletion(ctx context.Context, sourceNodeID string, sourceLibraryID string, sourceItemID string, logicalWorkID string, deletedAt time.Time) (catalog.RetentionRecord, error) {
	sourceNodeID = strings.TrimSpace(sourceNodeID)
	sourceLibraryID = strings.TrimSpace(sourceLibraryID)
	sourceItemID = strings.TrimSpace(sourceItemID)
	logicalWorkID = strings.TrimSpace(logicalWorkID)
	if sourceNodeID == "" {
		return catalog.RetentionRecord{}, catalog.ErrSourceNodeIDRequired
	}
	if sourceLibraryID == "" {
		return catalog.RetentionRecord{}, catalog.ErrSourceLibraryIDRequired
	}
	if sourceItemID == "" {
		return catalog.RetentionRecord{}, catalog.ErrSourceItemIDRequired
	}
	if deletedAt.IsZero() {
		deletedAt = time.Now().UTC()
	}
	deletedAt = deletedAt.UTC()

	record := catalog.RetentionRecord{
		SourceNodeID:    sourceNodeID,
		SourceLibraryID: sourceLibraryID,
		SourceItemID:    sourceItemID,
		LogicalWorkID:   logicalWorkID,
		DeletedAt:       deletedAt,
		ExpiresAt:       deletedAt.Add(repo.policy.GracePeriod),
	}

	_, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO retention (
			source_node_id, source_library_id, source_item_id, logical_work_id,
			deleted_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_node_id, source_library_id, source_item_id) DO UPDATE SET
			logical_work_id = excluded.logical_work_id,
			deleted_at      = excluded.deleted_at,
			expires_at      = excluded.expires_at`,
		record.SourceNodeID, record.SourceLibraryID, record.SourceItemID, record.LogicalWorkID,
		FormatTime(record.DeletedAt), FormatTime(record.ExpiresAt),
	)
	if err != nil {
		return catalog.RetentionRecord{}, fmt.Errorf("record deletion: %w", err)
	}
	return record, nil
}

// Get returns the retention record for a source item, mirroring
// RetentionStore.Get including its found-or-not-found boolean.
func (repo *RetentionRepository) Get(ctx context.Context, sourceNodeID string, sourceLibraryID string, sourceItemID string) (catalog.RetentionRecord, bool, error) {
	sourceNodeID = strings.TrimSpace(sourceNodeID)
	sourceLibraryID = strings.TrimSpace(sourceLibraryID)
	sourceItemID = strings.TrimSpace(sourceItemID)
	row := repo.database.SQL().QueryRowContext(ctx, `
		SELECT source_node_id, source_library_id, source_item_id, logical_work_id,
		       deleted_at, expires_at
		FROM retention WHERE source_node_id = ? AND source_library_id = ? AND source_item_id = ?`,
		sourceNodeID, sourceLibraryID, sourceItemID)
	record, err := scanRetentionRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.RetentionRecord{}, false, nil
	}
	if err != nil {
		return catalog.RetentionRecord{}, false, fmt.Errorf("get retention record: %w", err)
	}
	return record, true, nil
}

// Discard removes a retention record, for when the source item reappears
// before its grace period elapses. Like RetentionStore.Discard, discarding a
// record that does not exist is not an error.
func (repo *RetentionRepository) Discard(ctx context.Context, sourceNodeID string, sourceLibraryID string, sourceItemID string) error {
	sourceNodeID = strings.TrimSpace(sourceNodeID)
	sourceLibraryID = strings.TrimSpace(sourceLibraryID)
	sourceItemID = strings.TrimSpace(sourceItemID)
	_, err := repo.database.SQL().ExecContext(ctx,
		`DELETE FROM retention WHERE source_node_id = ? AND source_library_id = ? AND source_item_id = ?`,
		sourceNodeID, sourceLibraryID, sourceItemID,
	)
	if err != nil {
		return fmt.Errorf("discard retention record: %w", err)
	}
	return nil
}

// Expire deletes every record whose grace period has elapsed as of now and
// reports how many were removed. This is a single DELETE against
// idx_retention_expires rather than a load-then-filter in Go: the sweep runs
// on every node on a timer, and turning it into a full table scan is the
// exact regression the index exists to prevent.
func (repo *RetentionRepository) Expire(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()

	result, err := repo.database.SQL().ExecContext(ctx,
		`DELETE FROM retention WHERE expires_at <= ?`, FormatTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("expire retention records: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired retention records: %w", err)
	}
	return int(affected), nil
}

// CountForSource reports how many retained items came from a given source
// node. It exists for the ejection path, where a departing node's retained
// catalog needs a size before it is torn down.
func (repo *RetentionRepository) CountForSource(ctx context.Context, sourceNodeID string) (int, error) {
	sourceNodeID = strings.TrimSpace(sourceNodeID)
	var count int
	err := repo.database.SQL().QueryRowContext(ctx,
		`SELECT count(*) FROM retention WHERE source_node_id = ?`, sourceNodeID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count retention records for source: %w", err)
	}
	return count, nil
}

func scanRetentionRecord(scanner rowScanner) (catalog.RetentionRecord, error) {
	var record catalog.RetentionRecord
	var deletedAt, expiresAt string
	if err := scanner.Scan(
		&record.SourceNodeID, &record.SourceLibraryID, &record.SourceItemID, &record.LogicalWorkID,
		&deletedAt, &expiresAt,
	); err != nil {
		return catalog.RetentionRecord{}, err
	}

	var err error
	record.DeletedAt, err = ParseTime(deletedAt)
	if err != nil {
		return catalog.RetentionRecord{}, fmt.Errorf("parse deleted_at: %w", err)
	}
	record.ExpiresAt, err = ParseTime(expiresAt)
	if err != nil {
		return catalog.RetentionRecord{}, fmt.Errorf("parse expires_at: %w", err)
	}
	return record, nil
}
