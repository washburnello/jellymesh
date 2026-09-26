package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"jellymesh/internal/transport"
)

var (
	ErrNodeIDRequired      = errors.New("a node ID is required")
	ErrFingerprintRequired = errors.New("a fingerprint is required")

	// ErrFingerprintInUse means the fingerprint being assigned to one node
	// already identifies a different node. Two nodes sharing a fingerprint
	// would mean one could present the other's key and be authorized as it,
	// so this is refused outright rather than silently moving the key over.
	ErrFingerprintInUse = errors.New("fingerprint already belongs to a different node")

	// ErrFingerprintChanged means an upsert tried to give an existing node a
	// different key. There is no key rotation (design-spec.md section 8): a
	// node with a new key is a new peer that must be enrolled afresh, so the
	// old record has to be removed explicitly rather than re-keyed in place,
	// which would carry the old key's trust over to the new one.
	ErrFingerprintChanged = errors.New("a known node cannot change its fingerprint; remove it and enroll the new key")

	// ErrPeerNotFound is returned by mutators that target a specific node_id
	// which does not exist in the peers table.
	ErrPeerNotFound = errors.New("peer not found")
)

// Peer is this node's directory entry for another Jellymesh node: the key
// that identifies it, how to reach it, and whether it is blocked.
//
// A peer record is not an authorization. Transport trust is derived from the
// group log's roster (see internal/membership), so a directory entry can
// neither grant nor retain access.
type Peer struct {
	NodeID         string
	Fingerprint    transport.Fingerprint
	FriendlyName   string
	PublicHostname string
	// Blocked is read-only: it is reported by lookups and ignored by Upsert.
	// Blocks live in their own table and change only through SetBlocked.
	Blocked   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PeerRepository is this node's peer directory and its record of local
// blocks. Blocks are the one security decision it holds, and they only ever
// remove access.
type PeerRepository struct {
	database *DB
}

// NewPeerRepository builds a PeerRepository over an already-open database.
func NewPeerRepository(database *DB) *PeerRepository {
	return &PeerRepository{database: database}
}

// Upsert records peer, keyed by node_id.
//
// Creating a peer records every field given. Updating an existing peer
// changes only its descriptive fields (friendly name, public hostname,
// updated_at). Blocks are deliberately not writable here: they change only
// through SetBlocked, so refreshing a peer's display name can never lift one.
//
// node_id is the identity and fingerprint must stay unique. A fingerprint that
// already belongs to a different node is ErrFingerprintInUse, and a different
// fingerprint for a known node is ErrFingerprintChanged.
func (repo *PeerRepository) Upsert(ctx context.Context, peer Peer) error {
	nodeID := strings.TrimSpace(peer.NodeID)
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	if peer.Fingerprint == "" {
		return ErrFingerprintRequired
	}

	return repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		var conflictingNodeID string
		err := tx.QueryRowContext(ctx,
			`SELECT node_id FROM peers WHERE fingerprint = ? AND node_id != ?`,
			string(peer.Fingerprint), nodeID,
		).Scan(&conflictingNodeID)
		switch {
		case err == nil:
			return fmt.Errorf("%w: fingerprint %q is already assigned to node %q", ErrFingerprintInUse, peer.Fingerprint, conflictingNodeID)
		case errors.Is(err, sql.ErrNoRows):
			// No other node holds this fingerprint; clear to proceed.
		default:
			return fmt.Errorf("check fingerprint conflict: %w", err)
		}

		var existingFingerprint string
		err = tx.QueryRowContext(ctx,
			`SELECT fingerprint FROM peers WHERE node_id = ?`, nodeID,
		).Scan(&existingFingerprint)
		switch {
		case err == nil:
			if existingFingerprint != string(peer.Fingerprint) {
				return fmt.Errorf("%w: node %q", ErrFingerprintChanged, nodeID)
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE peers SET friendly_name = ?, public_hostname = ?, updated_at = ?
				WHERE node_id = ?`,
				peer.FriendlyName, peer.PublicHostname, FormatTime(peer.UpdatedAt), nodeID,
			)
			if err != nil {
				return fmt.Errorf("update peer: %w", err)
			}
			return nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("look up existing peer: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO peers(node_id, fingerprint, friendly_name, public_hostname, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			nodeID, string(peer.Fingerprint), peer.FriendlyName, peer.PublicHostname,
			FormatTime(peer.CreatedAt), FormatTime(peer.UpdatedAt),
		)
		if err != nil {
			return fmt.Errorf("insert peer: %w", err)
		}
		return nil
	})
}

// ByNodeID looks up a peer by its node identity.
func (repo *PeerRepository) ByNodeID(ctx context.Context, nodeID string) (Peer, bool, error) {
	row := repo.database.SQL().QueryRowContext(ctx, peerSelectColumns+` FROM peers WHERE node_id = ?`, strings.TrimSpace(nodeID))
	return scanPeer(row)
}

// ByFingerprint looks up a peer by its key fingerprint, which is what the
// transport layer authenticates connections against.
func (repo *PeerRepository) ByFingerprint(ctx context.Context, fingerprint transport.Fingerprint) (Peer, bool, error) {
	row := repo.database.SQL().QueryRowContext(ctx, peerSelectColumns+` FROM peers WHERE fingerprint = ?`, string(fingerprint))
	return scanPeer(row)
}

// List returns every known peer.
func (repo *PeerRepository) List(ctx context.Context) ([]Peer, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, peerSelectColumns+` FROM peers ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	defer rows.Close()

	var peers []Peer
	for rows.Next() {
		peer, err := scanPeerRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan peer: %w", err)
		}
		peers = append(peers, peer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	return peers, nil
}

// SetBlocked records or lifts a pairwise block on nodeID. A block is a media
// cut: a blocked member fails the transport trust check even though it
// remains in the roster.
//
// It is keyed by node ID and needs no peer record, so a node can be blocked
// before it has ever connected (C-PO-12), and removing a peer record does not
// lift its block. This is the only function that lifts a block.
func (repo *PeerRepository) SetBlocked(ctx context.Context, nodeID string, blocked bool) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	var err error
	if blocked {
		_, err = repo.database.SQL().ExecContext(ctx,
			`INSERT OR IGNORE INTO blocks(node_id, blocked_at) VALUES (?, ?)`,
			nodeID, FormatTime(nowUTC()),
		)
	} else {
		_, err = repo.database.SQL().ExecContext(ctx, `DELETE FROM blocks WHERE node_id = ?`, nodeID)
	}
	if err != nil {
		return fmt.Errorf("update peer block: %w", err)
	}
	return nil
}

// IsBlocked reports whether nodeID is blocked, whether or not it has a peer
// record.
func (repo *PeerRepository) IsBlocked(ctx context.Context, nodeID string) (bool, error) {
	var blocked int
	err := repo.database.SQL().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM blocks WHERE node_id = ?)`, strings.TrimSpace(nodeID),
	).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("check block: %w", err)
	}
	return blocked != 0, nil
}

// Remove deletes a peer's trust record entirely.
func (repo *PeerRepository) Remove(ctx context.Context, nodeID string) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	result, err := repo.database.SQL().ExecContext(ctx, `DELETE FROM peers WHERE node_id = ?`, nodeID)
	if err != nil {
		return fmt.Errorf("remove peer: %w", err)
	}
	return requireRowAffected(result)
}

const peerSelectColumns = `SELECT node_id, fingerprint, friendly_name, public_hostname,
	EXISTS(SELECT 1 FROM blocks WHERE blocks.node_id = peers.node_id), created_at, updated_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting a single
// scan routine serve single-row and multi-row lookups.
func scanPeer(row rowScanner) (Peer, bool, error) {
	peer, err := scanPeerRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, false, nil
	}
	if err != nil {
		return Peer{}, false, err
	}
	return peer, true, nil
}

func scanPeerRow(row rowScanner) (Peer, error) {
	var (
		peer                       Peer
		fingerprint                string
		blocked                    int
		createdAtRaw, updatedAtRaw string
	)
	if err := row.Scan(&peer.NodeID, &fingerprint, &peer.FriendlyName, &peer.PublicHostname,
		&blocked, &createdAtRaw, &updatedAtRaw); err != nil {
		return Peer{}, err
	}
	peer.Fingerprint = transport.Fingerprint(fingerprint)
	peer.Blocked = blocked != 0

	createdAt, err := ParseTime(createdAtRaw)
	if err != nil {
		return Peer{}, fmt.Errorf("parse created_at: %w", err)
	}
	peer.CreatedAt = createdAt

	updatedAt, err := ParseTime(updatedAtRaw)
	if err != nil {
		return Peer{}, fmt.Errorf("parse updated_at: %w", err)
	}
	peer.UpdatedAt = updatedAt

	return peer, nil
}

// requireRowAffected turns a zero-row-affected mutation into ErrPeerNotFound,
// so a caller cannot mistake "no such peer" for "updated as requested".
func requireRowAffected(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if affected == 0 {
		return ErrPeerNotFound
	}
	return nil
}
