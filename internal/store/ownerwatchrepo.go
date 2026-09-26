package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"jellymesh/internal/group"
)

// OwnerWatchRepository stores this node's observation of each group owner's
// availability, so that an absence window survives a restart instead of
// starting again from zero.
type OwnerWatchRepository struct {
	database *DB
}

func NewOwnerWatchRepository(database *DB) *OwnerWatchRepository {
	return &OwnerWatchRepository{database: database}
}

func (repo *OwnerWatchRepository) Save(ctx context.Context, watch *group.Watch) error {
	if watch == nil || strings.TrimSpace(watch.GroupID) == "" {
		return ErrMembershipGroupIDRequired
	}
	_, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO owner_watch (
			group_id, owner_id, status,
			owner_unavailable_since, succession_deadline, dissolution_deadline, last_notification_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(group_id) DO UPDATE SET
			owner_id                = excluded.owner_id,
			status                  = excluded.status,
			owner_unavailable_since = excluded.owner_unavailable_since,
			succession_deadline     = excluded.succession_deadline,
			dissolution_deadline    = excluded.dissolution_deadline,
			last_notification_at    = excluded.last_notification_at`,
		watch.GroupID, watch.OwnerID, string(watch.Status),
		FormatTime(watch.OwnerUnavailableSince), FormatTime(watch.SuccessionDeadline),
		FormatTime(watch.DissolutionDeadline), FormatTime(watch.LastNotificationAt),
	)
	if err != nil {
		return fmt.Errorf("save owner watch: %w", err)
	}
	return nil
}

// Load returns the stored watch for groupID. The boolean reports whether one
// was stored.
func (repo *OwnerWatchRepository) Load(ctx context.Context, groupID string) (*group.Watch, bool, error) {
	watch := &group.Watch{GroupID: strings.TrimSpace(groupID)}
	var status, since, succession, dissolution, notified string
	err := repo.database.SQL().QueryRowContext(ctx, `
		SELECT owner_id, status, owner_unavailable_since, succession_deadline, dissolution_deadline, last_notification_at
		FROM owner_watch WHERE group_id = ?`, watch.GroupID,
	).Scan(&watch.OwnerID, &status, &since, &succession, &dissolution, &notified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load owner watch: %w", err)
	}
	watch.Status = group.Status(status)
	if watch.OwnerUnavailableSince, err = ParseTime(since); err != nil {
		return nil, false, fmt.Errorf("parse owner_unavailable_since: %w", err)
	}
	if watch.SuccessionDeadline, err = ParseTime(succession); err != nil {
		return nil, false, fmt.Errorf("parse succession_deadline: %w", err)
	}
	if watch.DissolutionDeadline, err = ParseTime(dissolution); err != nil {
		return nil, false, fmt.Errorf("parse dissolution_deadline: %w", err)
	}
	if watch.LastNotificationAt, err = ParseTime(notified); err != nil {
		return nil, false, fmt.Errorf("parse last_notification_at: %w", err)
	}
	return watch, true, nil
}
