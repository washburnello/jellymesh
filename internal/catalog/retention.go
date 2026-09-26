package catalog

import (
	"errors"
	"strings"
	"time"
)

const DefaultDeletionGracePeriod = 7 * 24 * time.Hour

var (
	ErrSourceNodeIDRequired    = errors.New("source node ID is required")
	ErrSourceLibraryIDRequired = errors.New("source library ID is required")
	ErrSourceItemIDRequired    = errors.New("source item ID is required")
	ErrInvalidRetentionPolicy  = errors.New("retention grace period must be positive")
)

type RetentionPolicy struct {
	GracePeriod time.Duration
}

type RetentionRecord struct {
	SourceNodeID    string
	SourceLibraryID string
	SourceItemID    string
	LogicalWorkID   string
	DeletedAt       time.Time
	ExpiresAt       time.Time
}

type RetentionStore struct {
	policy  RetentionPolicy
	records map[string]RetentionRecord
}

func NewRetentionStore(policy RetentionPolicy) (*RetentionStore, error) {
	if policy.GracePeriod <= 0 {
		return nil, ErrInvalidRetentionPolicy
	}
	return &RetentionStore{
		policy:  policy,
		records: make(map[string]RetentionRecord),
	}, nil
}

func (store *RetentionStore) RecordDeletion(sourceNodeID string, sourceLibraryID string, sourceItemID string, logicalWorkID string, deletedAt time.Time) (RetentionRecord, error) {
	if store == nil {
		return RetentionRecord{}, errors.New("retention store is nil")
	}
	sourceNodeID = strings.TrimSpace(sourceNodeID)
	sourceLibraryID = strings.TrimSpace(sourceLibraryID)
	sourceItemID = strings.TrimSpace(sourceItemID)
	logicalWorkID = strings.TrimSpace(logicalWorkID)
	if sourceNodeID == "" {
		return RetentionRecord{}, ErrSourceNodeIDRequired
	}
	if sourceLibraryID == "" {
		return RetentionRecord{}, ErrSourceLibraryIDRequired
	}
	if sourceItemID == "" {
		return RetentionRecord{}, ErrSourceItemIDRequired
	}
	if deletedAt.IsZero() {
		deletedAt = time.Now().UTC()
	}
	deletedAt = deletedAt.UTC()
	record := RetentionRecord{
		SourceNodeID:    sourceNodeID,
		SourceLibraryID: sourceLibraryID,
		SourceItemID:    sourceItemID,
		LogicalWorkID:   logicalWorkID,
		DeletedAt:       deletedAt,
		ExpiresAt:       deletedAt.Add(store.policy.GracePeriod),
	}
	store.records[retentionKey(sourceNodeID, sourceLibraryID, sourceItemID)] = record
	return record, nil
}

func (store *RetentionStore) Get(sourceNodeID string, sourceLibraryID string, sourceItemID string) (RetentionRecord, bool) {
	if store == nil {
		return RetentionRecord{}, false
	}
	record, ok := store.records[retentionKey(sourceNodeID, sourceLibraryID, sourceItemID)]
	return record, ok
}

func (store *RetentionStore) Discard(sourceNodeID string, sourceLibraryID string, sourceItemID string) {
	if store == nil {
		return
	}
	delete(store.records, retentionKey(sourceNodeID, sourceLibraryID, sourceItemID))
}

func (store *RetentionStore) Expire(now time.Time) int {
	if store == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	expired := 0
	for key, record := range store.records {
		if !now.Before(record.ExpiresAt) {
			delete(store.records, key)
			expired++
		}
	}
	return expired
}

func retentionKey(sourceNodeID string, sourceLibraryID string, sourceItemID string) string {
	return sourceNodeID + "/" + sourceLibraryID + "/" + sourceItemID
}
