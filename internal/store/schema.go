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

	// 2: blocks move out of the peers table.
	//
	// A block is a decision about a node, not about a key, so it is keyed by
	// node_id alone and can be recorded for a node this one has never seen
	// (C-PO-12); peers.blocked could not, because a peer row needs a
	// fingerprint. It also gives blocks a single writer: when the flag lived
	// on the peer row, a peer upsert or a membership snapshot could clear it
	// as a side effect, silently restoring transport trust.
	`
	CREATE TABLE blocks (
		node_id     TEXT PRIMARY KEY,
		blocked_at  TEXT NOT NULL
	);
	INSERT INTO blocks(node_id, blocked_at)
		SELECT node_id, updated_at FROM peers WHERE blocked = 1;
	ALTER TABLE peers DROP COLUMN blocked;
	`,

	// 3: the replicated group log (design-spec section 8). Events are stored
	// exactly as received and re-verified on every load, so this table is a
	// cache of the log rather than a source of trust.
	`
	CREATE TABLE group_log (
		group_id  TEXT NOT NULL,
		sequence  INTEGER NOT NULL,
		epoch     INTEGER NOT NULL,
		hash      TEXT NOT NULL,
		event     TEXT NOT NULL,
		PRIMARY KEY (group_id, sequence)
	);

	-- Old-epoch events removed by a succession, kept so their proposals can
	-- be re-proposed and so an operator can see what was undone.
	CREATE TABLE group_log_superseded (
		group_id       TEXT NOT NULL,
		hash           TEXT NOT NULL,
		sequence       INTEGER NOT NULL,
		event          TEXT NOT NULL,
		superseded_at  TEXT NOT NULL,
		PRIMARY KEY (group_id, hash)
	);

	-- Equivocation evidence. Its presence halts the group's log across
	-- restarts until an operator intervenes.
	CREATE TABLE group_log_evidence (
		group_id     TEXT PRIMARY KEY,
		existing     TEXT NOT NULL,
		received     TEXT NOT NULL,
		detected_at  TEXT NOT NULL
	);
	`,

	// 4: transport trust is derived from the group log's roster, so the
	// separate trust flag goes. Keeping it would leave two answers to "may
	// this key connect", which is how membership and trust drifted apart.
	`
	ALTER TABLE peers DROP COLUMN trusted;
	`,

	// 5: the roster, roles, and membership sequence now come from the group
	// log, so their tables go. What remains of group_state is this node's own
	// observation of the owner's availability, which is node-local.
	`
	DROP TABLE members;
	ALTER TABLE group_state DROP COLUMN membership_sequence;
	ALTER TABLE group_state RENAME TO owner_watch;
	`,

	// 6: a redeemed invitation records the invitee's key, taken from the
	// certificate it redeemed with, because the admission binds that key.
	// Approval is a signed proposal made on the approver's own node, so the
	// inviter no longer records an approval ID.
	`
	ALTER TABLE invitations ADD COLUMN member_key TEXT NOT NULL DEFAULT '';
	ALTER TABLE invitations ADD COLUMN friendly_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE invitations ADD COLUMN public_hostname TEXT NOT NULL DEFAULT '';
	ALTER TABLE invitations DROP COLUMN approval_id;
	CREATE INDEX idx_invitations_code ON invitations(code_hash);
	`,

	// 7: node-local flags. The first is the hold a restore places on
	// sequencing: a node restored from a backup may be missing events it had
	// already published, and must catch up before it sequences again.
	`
	CREATE TABLE node_flags (
		name   TEXT PRIMARY KEY,
		value  TEXT NOT NULL,
		set_at TEXT NOT NULL
	);
	`,

	// 8: proposals waiting for the owner. While the owner's node is
	// unreachable, a member's signed proposals queue here and are submitted
	// on each heartbeat until the owner sequences or refuses them
	// (conformance.md assumption A-5).
	`
	CREATE TABLE pending_proposals (
		proposal_id  TEXT PRIMARY KEY,
		group_id     TEXT NOT NULL,
		proposal     TEXT NOT NULL,
		queued_at    TEXT NOT NULL
	);
	CREATE INDEX idx_pending_proposals_group ON pending_proposals(group_id, queued_at);
	`,

	// 9: the source catalog (design-spec section 9, "The source catalog").
	// source_publications is this node's own publication decisions with their
	// declared roots; source_items is what it has published, with a revision
	// per item and a node-wide change sequence. Rows are never deleted: a
	// withdrawn item becomes a tombstone with a new sequence, so that every
	// destination learns of it.
	`
	CREATE TABLE source_publications (
		library_id       TEXT PRIMARY KEY,
		name             TEXT NOT NULL,
		collection_type  TEXT NOT NULL,
		roots            TEXT NOT NULL,
		state            TEXT NOT NULL CHECK (state IN ('published', 'paused')),
		paused_reason    TEXT NOT NULL DEFAULT '',
		published_at     TEXT NOT NULL,
		updated_at       TEXT NOT NULL
	);
	CREATE TABLE source_items (
		item_id     TEXT PRIMARY KEY,
		library_id  TEXT NOT NULL,
		parent_id   TEXT NOT NULL DEFAULT '',
		item_type   TEXT NOT NULL,
		revision    INTEGER NOT NULL,
		sequence    INTEGER NOT NULL UNIQUE,
		etag        TEXT NOT NULL DEFAULT '',
		checksum    TEXT NOT NULL DEFAULT '',
		tombstone   INTEGER NOT NULL DEFAULT 0,
		metadata    TEXT NOT NULL DEFAULT '',
		updated_at  TEXT NOT NULL
	);
	CREATE INDEX idx_source_items_library ON source_items(library_id, tombstone);
	`,

	// 10: the destination's view of each source's catalog (design-spec
	// section 9, "The destination catalog").
	`
	CREATE TABLE remote_items (
		source_node_id  TEXT NOT NULL,
		item_id         TEXT NOT NULL,
		library_id      TEXT NOT NULL,
		parent_id       TEXT NOT NULL DEFAULT '',
		item_type       TEXT NOT NULL,
		revision        INTEGER NOT NULL,
		metadata        TEXT NOT NULL,
		updated_at      TEXT NOT NULL,
		PRIMARY KEY (source_node_id, item_id)
	);
	CREATE INDEX idx_remote_items_library ON remote_items(source_node_id, library_id);
	-- Retention keeps a withdrawn item's metadata, not only its identity, so
	-- that it can be restored if the item returns within the grace period.
	ALTER TABLE retention ADD COLUMN metadata TEXT NOT NULL DEFAULT '';
	`,

	// 11: what the destination has materialized. Each playable item has a
	// random reference, which is what its .strm names and what the local relay
	// resolves. Deleting the row revokes the reference.
	`
	CREATE TABLE materialized (
		source_node_id  TEXT NOT NULL,
		item_id         TEXT NOT NULL,
		library_id      TEXT NOT NULL,
		reference       TEXT NOT NULL UNIQUE,
		path            TEXT NOT NULL,
		checksum        TEXT NOT NULL,
		created_at      TEXT NOT NULL,
		PRIMARY KEY (source_node_id, item_id)
	);
	`,

	// 12: the revision each materialized item was written at, so that the
	// relay's cache of an item's first bytes is never served for a different
	// revision of it.
	`
	ALTER TABLE materialized ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
	`,

	// 13: the folder and identifiers each work was first materialized under.
	// Jellyfin keys a user's state for an item under its path and provider
	// identifiers, so a work keeps both for as long as it is materialized,
	// and for as long after as Jellyfin keeps state it can reattach
	// (conformance M-10, assumption A-14).
	`
	CREATE TABLE work_pins (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		kind        TEXT NOT NULL,
		folder      TEXT NOT NULL UNIQUE,
		identifiers TEXT NOT NULL,
		created_at  TEXT NOT NULL,
		last_used   TEXT NOT NULL
	);
	`,

	// 14: the reference each withdrawn .strm path last held. Jellyfin reads
	// a .strm's URL only when it scans (A-12), so a film returning to the
	// same path, or an episode moving to another source (A-13), keeps its
	// reference, rebound to the new item, and plays before the next scan.
	// A retired reference resolves to nothing until it is reused.
	`
	CREATE TABLE retired_references (
		path       TEXT PRIMARY KEY,
		reference  TEXT NOT NULL UNIQUE,
		retired_at TEXT NOT NULL
	);
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
