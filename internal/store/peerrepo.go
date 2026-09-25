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

	// ErrPeerNotFound is returned by mutators that target a specific node_id
	// which does not exist in the peers table.
	ErrPeerNotFound = errors.New("peer not found")
)

// Peer is the durable record of a pairwise relationship with another
// Jellymesh node: the key that identifies it, whether this node has decided
// to trust it, and whether it has been pairwise blocked.
type Peer struct {
	NodeID         string
	Fingerprint    transport.Fingerprint
	FriendlyName   string
	PublicHostname string
	Trusted        bool
	Blocked        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PeerRepository is the durable store of peer trust decisions. It backs
// transport.TrustStore so that the mutual-TLS layer authorizes connections
// directly against what this node has actually decided, rather than against
// a copy of it kept in memory.
type PeerRepository struct {
	database *DB
}

// NewPeerRepository builds a PeerRepository over an already-open database.
func NewPeerRepository(database *DB) *PeerRepository {
	return &PeerRepository{database: database}
}

// Upsert writes peer, keyed by node_id. Every field is replaced with what is
// given, including the timestamps: like the rest of this codebase's domain
// structs (see policy.Invitation, policy.Publication), the caller owns time
// and passes the values it wants recorded, rather than the repository
// silently substituting its own clock.
//
// node_id is the identity; fingerprint must stay unique. If peer's
// fingerprint already belongs to a different node_id, that is a trust
// conflict, not a rename, and is rejected with ErrFingerprintInUse.
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

		_, err = tx.ExecContext(ctx, `
			INSERT INTO peers(node_id, fingerprint, friendly_name, public_hostname, trusted, blocked, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(node_id) DO UPDATE SET
				fingerprint     = excluded.fingerprint,
				friendly_name   = excluded.friendly_name,
				public_hostname = excluded.public_hostname,
				trusted         = excluded.trusted,
				blocked         = excluded.blocked,
				created_at      = excluded.created_at,
				updated_at      = excluded.updated_at
			`,
			nodeID, string(peer.Fingerprint), peer.FriendlyName, peer.PublicHostname,
			boolToInt(peer.Trusted), boolToInt(peer.Blocked),
			FormatTime(peer.CreatedAt), FormatTime(peer.UpdatedAt),
		)
		if err != nil {
			return fmt.Errorf("upsert peer: %w", err)
		}
		return nil
	})
}

// ByNodeID looks up a peer by its node identity.
func (repo *PeerRepository) ByNodeID(ctx context.Context, nodeID string) (Peer, bool, error) {
	row := repo.database.SQL().QueryRowContext(ctx, peerSelectColumns+` FROM peers WHERE node_id = ?`, nodeID)
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

// SetTrusted updates whether this node has decided to trust nodeID.
func (repo *PeerRepository) SetTrusted(ctx context.Context, nodeID string, trusted bool) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	result, err := repo.database.SQL().ExecContext(ctx,
		`UPDATE peers SET trusted = ?, updated_at = ? WHERE node_id = ?`,
		boolToInt(trusted), FormatTime(nowUTC()), nodeID,
	)
	if err != nil {
		return fmt.Errorf("update peer trust: %w", err)
	}
	return requireRowAffected(result)
}

// SetBlocked updates whether nodeID is pairwise blocked. A block is a media
// cut, independent of the trust decision: see IsTrusted.
func (repo *PeerRepository) SetBlocked(ctx context.Context, nodeID string, blocked bool) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	result, err := repo.database.SQL().ExecContext(ctx,
		`UPDATE peers SET blocked = ?, updated_at = ? WHERE node_id = ?`,
		boolToInt(blocked), FormatTime(nowUTC()), nodeID,
	)
	if err != nil {
		return fmt.Errorf("update peer block: %w", err)
	}
	return requireRowAffected(result)
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

// isTrustedQueryTimeout bounds how long IsTrusted will wait on the database.
// IsTrusted sits on the hot path for every inbound connection (see the
// comment on idx_peers_fingerprint in schema.go) and its signature has no
// error return, so a wedged database must not be able to hang a handshake
// forever; it must fail closed promptly instead.
const isTrustedQueryTimeout = 2 * time.Second

// IsTrusted satisfies transport.TrustStore. It fails closed: an empty
// fingerprint, an unknown fingerprint, a blocked peer, or any database error
// (including a closed or wedged database) all result in false. There is no
// error return to forget to check, precisely because this sits on an
// authorization path.
//
// A blocked peer is never trusted, even if trusted is set: a block is a
// pairwise media cut, and an authorized-but-blocked peer must not be allowed
// to pass the transport check regardless of the trust flag's value.
func (repo *PeerRepository) IsTrusted(fingerprint transport.Fingerprint) bool {
	if fingerprint == "" {
		return false
	}
	if repo == nil || repo.database == nil || repo.database.SQL() == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), isTrustedQueryTimeout)
	defer cancel()

	var trusted, blocked int
	err := repo.database.SQL().QueryRowContext(ctx,
		`SELECT trusted, blocked FROM peers WHERE fingerprint = ?`, string(fingerprint),
	).Scan(&trusted, &blocked)
	if err != nil {
		// Includes sql.ErrNoRows (unknown fingerprint) and any I/O or driver
		// failure (closed database, disk error, timeout): all fail closed.
		return false
	}
	return trusted != 0 && blocked == 0
}

const peerSelectColumns = `SELECT node_id, fingerprint, friendly_name, public_hostname, trusted, blocked, created_at, updated_at`

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
		trusted, blocked           int
		createdAtRaw, updatedAtRaw string
	)
	if err := row.Scan(&peer.NodeID, &fingerprint, &peer.FriendlyName, &peer.PublicHostname,
		&trusted, &blocked, &createdAtRaw, &updatedAtRaw); err != nil {
		return Peer{}, err
	}
	peer.Fingerprint = transport.Fingerprint(fingerprint)
	peer.Trusted = trusted != 0
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
