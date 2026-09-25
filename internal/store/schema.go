package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order, each inside its own transaction. They are
// append-only: once a migration ships, it is never edited, because other nodes
// have already applied it. Corrections arrive as a new migration.
var migrations = []string{
	// 1: initial schema.
	`
	CREATE TABLE peers (
		node_id          TEXT PRIMARY KEY,
		fingerprint      TEXT NOT NULL UNIQUE,
		friendly_name    TEXT NOT NULL DEFAULT '',
		public_hostname  TEXT NOT NULL DEFAULT '',
		trusted          INTEGER NOT NULL DEFAULT 0,
		blocked          INTEGER NOT NULL DEFAULT 0,
		created_at       TEXT NOT NULL,
		updated_at       TEXT NOT NULL
	);
	-- Peers are authorized by key fingerprint, never by hostname, so the
	-- fingerprint lookup is on the hot path for every inbound connection.
	CREATE UNIQUE INDEX idx_peers_fingerprint ON peers(fingerprint);

	CREATE TABLE group_state (
		group_id                 TEXT PRIMARY KEY,
		owner_id                 TEXT NOT NULL,
		status                   TEXT NOT NULL,
		membership_sequence      INTEGER NOT NULL DEFAULT 0,
		owner_unavailable_since  TEXT NOT NULL DEFAULT '',
		succession_deadline      TEXT NOT NULL DEFAULT '',
		dissolution_deadline     TEXT NOT NULL DEFAULT '',
		last_notification_at     TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE members (
		group_id     TEXT NOT NULL,
		node_id      TEXT NOT NULL,
		is_admin     INTEGER NOT NULL DEFAULT 0,
		promoted_at  TEXT NOT NULL DEFAULT '',
		ejected      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (group_id, node_id),
		FOREIGN KEY (group_id) REFERENCES group_state(group_id) ON DELETE CASCADE
	);
	CREATE INDEX idx_members_admins ON members(group_id, is_admin) WHERE is_admin = 1;

	-- state distinguishes a live publication from one staged by a node that is
	-- not yet admitted, which is what lets a joining or rejoining server
	-- satisfy the non-empty-publication admission rule before it is a member.
	CREATE TABLE publications (
		group_id         TEXT NOT NULL,
		source_node_id   TEXT NOT NULL,
		library_id       TEXT NOT NULL,
		library_name     TEXT NOT NULL,
		collection_type  TEXT NOT NULL,
		state            TEXT NOT NULL CHECK (state IN ('candidate','published')),
		published_at     TEXT NOT NULL,
		PRIMARY KEY (group_id, source_node_id, library_id)
	);
	CREATE INDEX idx_publications_source ON publications(source_node_id, state);

	CREATE TABLE opt_outs (
		group_id        TEXT NOT NULL,
		source_node_id  TEXT NOT NULL,
		library_id      TEXT NOT NULL,
		updated_at      TEXT NOT NULL,
		PRIMARY KEY (group_id, source_node_id, library_id)
	);

	CREATE TABLE invitations (
		invitation_id  TEXT PRIMARY KEY,
		group_id       TEXT NOT NULL,
		inviter_id     TEXT NOT NULL,
		invitee_id     TEXT NOT NULL DEFAULT '',
		code_hash      TEXT NOT NULL,
		fingerprint    TEXT NOT NULL DEFAULT '',
		approval_id    TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL,
		created_at     TEXT NOT NULL,
		expires_at     TEXT NOT NULL
	);
	CREATE INDEX idx_invitations_status ON invitations(group_id, status);

	-- The ledger is keyed by logical work rather than by generated Jellyfin
	-- item id, so that watched state survives an item being purged and later
	-- reappearing from a different source.
	CREATE TABLE history (
		local_user_id          TEXT NOT NULL,
		work_id                TEXT NOT NULL,
		media_type             TEXT NOT NULL DEFAULT '',
		provider_identity      TEXT NOT NULL DEFAULT '',
		played                 INTEGER NOT NULL DEFAULT 0,
		play_count             INTEGER NOT NULL DEFAULT 0,
		resume_position_ticks  INTEGER NOT NULL DEFAULT 0,
		last_played_at         TEXT NOT NULL DEFAULT '',
		updated_at             TEXT NOT NULL,
		PRIMARY KEY (local_user_id, work_id)
	);
	CREATE INDEX idx_history_work ON history(work_id);

	CREATE TABLE retention (
		source_node_id     TEXT NOT NULL,
		source_library_id  TEXT NOT NULL,
		source_item_id     TEXT NOT NULL,
		logical_work_id    TEXT NOT NULL DEFAULT '',
		deleted_at         TEXT NOT NULL,
		expires_at         TEXT NOT NULL,
		PRIMARY KEY (source_node_id, source_library_id, source_item_id)
	);
	-- The expiry sweep is the reason this package exists rather than a
	-- key-value store: without this index it is a full scan of every retained
	-- item on every pass.
	CREATE INDEX idx_retention_expires ON retention(expires_at);

	CREATE TABLE sync_cursors (
		source_node_id        TEXT PRIMARY KEY,
		cursor                TEXT NOT NULL DEFAULT '',
		state                 TEXT NOT NULL DEFAULT 'unknown',
		last_success_at       TEXT NOT NULL DEFAULT '',
		last_failure_at       TEXT NOT NULL DEFAULT '',
		failure_category      TEXT NOT NULL DEFAULT '',
		consecutive_failures  INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE audit_events (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		occurred_at TEXT NOT NULL,
		actor       TEXT NOT NULL DEFAULT '',
		action      TEXT NOT NULL,
		subject     TEXT NOT NULL DEFAULT '',
		detail      TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_audit_occurred ON audit_events(occurred_at);
	`,
}

func (database *DB) migrate(ctx context.Context) error {
	if _, err := database.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	current, err := database.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("%w: database is at %d, this build knows %d", ErrSchemaTooNew, current, len(migrations))
	}

	for index := current; index < len(migrations); index++ {
		version := index + 1
		statement := migrations[index]
		err := database.WithTx(ctx, func(transaction *sql.Tx) error {
			if _, err := transaction.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply migration %d: %w", version, err)
			}
			if _, err := transaction.ExecContext(ctx,
				`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
				version, FormatTime(nowUTC()),
			); err != nil {
				return fmt.Errorf("record migration %d: %w", version, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
