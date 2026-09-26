package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"jellymesh/internal/policy"
)

var (
	// ErrPolicyStateRequired means Save was asked to persist a nil state.
	ErrPolicyStateRequired = errors.New("a policy state is required")

	// ErrMembershipGroupIDRequired means no group id was given to key the
	// snapshot, lookup, or delete by.
	ErrMembershipGroupIDRequired = errors.New("a group ID is required")
)

// PolicyRepository stores a node's local policy for a group: publications
// (live and staged), opt-outs, invitations, and blocks.
//
// Membership is not stored here. The roster, roles, and ejections are derived
// from the group log, which GroupLogRepository stores and re-verifies; keeping
// a second copy here is what let the two disagree before.
type PolicyRepository struct {
	database *DB
}

// NewPolicyRepository builds a PolicyRepository over an already-open database.
func NewPolicyRepository(database *DB) *PolicyRepository {
	return &PolicyRepository{database: database}
}

// Save writes the state as one atomic snapshot, replacing what was stored for
// state.GroupID, except for blocks, which Save only adds (see below).
func (repo *PolicyRepository) Save(ctx context.Context, state *policy.State) error {
	if state == nil {
		return ErrPolicyStateRequired
	}
	groupID := strings.TrimSpace(state.GroupID)
	if groupID == "" {
		return ErrMembershipGroupIDRequired
	}

	return repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		if err := deletePolicy(ctx, tx, groupID); err != nil {
			return err
		}
		if err := insertPublications(ctx, tx, groupID, state.Published, "published"); err != nil {
			return err
		}
		if err := insertPublications(ctx, tx, groupID, state.Candidates, "candidate"); err != nil {
			return err
		}

		for sourceNodeID, libraries := range state.OptOuts {
			for libraryID, optOut := range libraries {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO opt_outs (group_id, source_node_id, library_id, updated_at)
					VALUES (?, ?, ?, ?)`,
					groupID, sourceNodeID, libraryID, FormatTime(optOut.UpdatedAt),
				); err != nil {
					return fmt.Errorf("insert opt-out %q/%q: %w", sourceNodeID, libraryID, err)
				}
			}
		}

		for invitationID, invitation := range state.Invitations {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO invitations (
					invitation_id, group_id, inviter_id, invitee_id, code_hash,
					fingerprint, approval_id, status, created_at, expires_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				invitationID, groupID, invitation.InviterID, invitation.InviteeID, invitation.CodeHash,
				invitation.Fingerprint, invitation.ApprovalID, string(invitation.Status),
				FormatTime(invitation.CreatedAt), FormatTime(invitation.ExpiresAt),
			); err != nil {
				return fmt.Errorf("insert invitation %q: %w", invitationID, err)
			}
		}

		return persistBlockedPeers(ctx, tx, state.BlockedPeers)
	})
}

// Load reconstructs the policy state for groupID. A group with nothing stored
// loads as an empty state, which is what a node that has just joined has.
func (repo *PolicyRepository) Load(ctx context.Context, groupID string) (*policy.State, error) {
	state, err := policy.NewState(groupID)
	if err != nil {
		return nil, ErrMembershipGroupIDRequired
	}
	groupID = state.GroupID
	if err := loadPublications(ctx, repo.database.SQL(), groupID, state); err != nil {
		return nil, err
	}
	if err := loadOptOuts(ctx, repo.database.SQL(), groupID, state); err != nil {
		return nil, err
	}
	if err := loadInvitations(ctx, repo.database.SQL(), groupID, state); err != nil {
		return nil, err
	}
	if err := loadBlockedPeers(ctx, repo.database.SQL(), state); err != nil {
		return nil, err
	}
	return state, nil
}

// Delete removes a group's stored policy. Blocks are kept: they are decisions
// about nodes, not about one group.
func (repo *PolicyRepository) Delete(ctx context.Context, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return ErrMembershipGroupIDRequired
	}
	return repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		return deletePolicy(ctx, tx, groupID)
	})
}

func deletePolicy(ctx context.Context, tx *sql.Tx, groupID string) error {
	for _, table := range []string{"publications", "opt_outs", "invitations"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Publications: live and staged candidates share one table, distinguished by
// the state column the schema already constrains to ('candidate','published').
// ---------------------------------------------------------------------------

func insertPublications(ctx context.Context, tx *sql.Tx, groupID string, publications map[string]policy.Publication, publicationState string) error {
	for _, publication := range publications {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO publications (
				group_id, source_node_id, library_id, library_name, collection_type, state, published_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			groupID, publication.SourceNodeID, publication.Library.ID, publication.Library.Name,
			publication.Library.CollectionType, publicationState, FormatTime(publication.PublishedAt),
		); err != nil {
			return fmt.Errorf("insert %s publication %q/%q: %w", publicationState, publication.SourceNodeID, publication.Library.ID, err)
		}
	}
	return nil
}

func loadPublications(ctx context.Context, db *sql.DB, groupID string, state *policy.State) error {
	rows, err := db.QueryContext(ctx, `
		SELECT source_node_id, library_id, library_name, collection_type, state, published_at
		FROM publications WHERE group_id = ?`, groupID)
	if err != nil {
		return fmt.Errorf("load publications: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sourceNodeID, libraryID, libraryName, collectionType, publicationState, publishedAtRaw string
		if err := rows.Scan(&sourceNodeID, &libraryID, &libraryName, &collectionType, &publicationState, &publishedAtRaw); err != nil {
			return fmt.Errorf("scan publication: %w", err)
		}
		publishedAt, err := ParseTime(publishedAtRaw)
		if err != nil {
			return fmt.Errorf("parse published_at for %q/%q: %w", sourceNodeID, libraryID, err)
		}
		publication := policy.Publication{
			GroupID:      groupID,
			SourceNodeID: sourceNodeID,
			Library: policy.Library{
				ID:             libraryID,
				Name:           libraryName,
				CollectionType: collectionType,
			},
			PublishedAt: publishedAt,
		}
		key := membershipPublicationKey(sourceNodeID, libraryID)
		switch publicationState {
		case "published":
			state.Published[key] = publication
		case "candidate":
			state.Candidates[key] = publication
		default:
			return fmt.Errorf("unknown publication state %q for %q/%q", publicationState, sourceNodeID, libraryID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load publications: %w", err)
	}
	return nil
}

// membershipPublicationKey mirrors policy's own unexported publicationKey
// format (sourceNodeID + "/" + libraryID) so a reconstructed Published or
// Candidates map is keyed exactly the way policy.State's own methods
// (Publish, Unpublish, IsOptedOut, CanConsume, ...) expect. Duplicated here
// because that helper is not exported; if policy ever changes the format,
// this must change with it.
func membershipPublicationKey(sourceNodeID string, libraryID string) string {
	return strings.TrimSpace(sourceNodeID) + "/" + strings.TrimSpace(libraryID)
}

// ---------------------------------------------------------------------------
// Opt-outs and invitations map onto their tables directly, field for field.
// ---------------------------------------------------------------------------

func loadOptOuts(ctx context.Context, db *sql.DB, groupID string, state *policy.State) error {
	rows, err := db.QueryContext(ctx,
		`SELECT source_node_id, library_id, updated_at FROM opt_outs WHERE group_id = ?`, groupID)
	if err != nil {
		return fmt.Errorf("load opt-outs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sourceNodeID, libraryID, updatedAtRaw string
		if err := rows.Scan(&sourceNodeID, &libraryID, &updatedAtRaw); err != nil {
			return fmt.Errorf("scan opt-out: %w", err)
		}
		updatedAt, err := ParseTime(updatedAtRaw)
		if err != nil {
			return fmt.Errorf("parse updated_at for opt-out %q/%q: %w", sourceNodeID, libraryID, err)
		}
		if state.OptOuts[sourceNodeID] == nil {
			state.OptOuts[sourceNodeID] = make(map[string]policy.OptOut)
		}
		state.OptOuts[sourceNodeID][libraryID] = policy.OptOut{
			GroupID:      groupID,
			SourceNodeID: sourceNodeID,
			LibraryID:    libraryID,
			UpdatedAt:    updatedAt,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load opt-outs: %w", err)
	}
	return nil
}

func loadInvitations(ctx context.Context, db *sql.DB, groupID string, state *policy.State) error {
	rows, err := db.QueryContext(ctx, `
		SELECT invitation_id, inviter_id, invitee_id, code_hash, fingerprint, approval_id, status, created_at, expires_at
		FROM invitations WHERE group_id = ?`, groupID)
	if err != nil {
		return fmt.Errorf("load invitations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var invitationID, inviterID, inviteeID, codeHash, fingerprint, approvalID, status, createdAtRaw, expiresAtRaw string
		if err := rows.Scan(&invitationID, &inviterID, &inviteeID, &codeHash, &fingerprint, &approvalID, &status, &createdAtRaw, &expiresAtRaw); err != nil {
			return fmt.Errorf("scan invitation: %w", err)
		}
		createdAt, err := ParseTime(createdAtRaw)
		if err != nil {
			return fmt.Errorf("parse created_at for invitation %q: %w", invitationID, err)
		}
		expiresAt, err := ParseTime(expiresAtRaw)
		if err != nil {
			return fmt.Errorf("parse expires_at for invitation %q: %w", invitationID, err)
		}
		state.Invitations[invitationID] = policy.Invitation{
			GroupID:      groupID,
			InvitationID: invitationID,
			InviterID:    inviterID,
			InviteeID:    inviteeID,
			CodeHash:     codeHash,
			Fingerprint:  fingerprint,
			ApprovalID:   approvalID,
			Status:       policy.InvitationStatus(status),
			CreatedAt:    createdAt,
			ExpiresAt:    expiresAt,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load invitations: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Blocked peers.
//
// A block is a local pairwise decision about a node rather than group state
// (see C-BL-4), so it lives in the blocks table that PeerRepository.SetBlocked
// owns, and policy.State.BlockedPeers is a view over it.
//
// Save only ever adds blocks. It must not clear a block that is absent from
// the snapshot, because blocks are also written outside policy.State through
// PeerRepository.SetBlocked; an earlier version mirrored the snapshot onto the
// peers table and so lifted any such block, restoring transport trust to the
// blocked peer as a side effect of an unrelated save. The consequence is that
// policy.State.UnblockPeer is not durable on its own: lifting a block is an
// explicit decision and goes through PeerRepository.SetBlocked. That errs
// towards staying blocked, which is the safe direction.
// ---------------------------------------------------------------------------

func persistBlockedPeers(ctx context.Context, tx *sql.Tx, blockedPeers map[string]bool) error {
	now := FormatTime(nowUTC())
	for nodeID, blocked := range blockedPeers {
		if !blocked || strings.TrimSpace(nodeID) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO blocks(node_id, blocked_at) VALUES (?, ?)`,
			strings.TrimSpace(nodeID), now,
		); err != nil {
			return fmt.Errorf("persist block for %q: %w", nodeID, err)
		}
	}
	return nil
}

func loadBlockedPeers(ctx context.Context, db *sql.DB, state *policy.State) error {
	rows, err := db.QueryContext(ctx, `SELECT node_id FROM blocks`)
	if err != nil {
		return fmt.Errorf("load blocked peers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return fmt.Errorf("scan blocked peer: %w", err)
		}
		state.BlockedPeers[nodeID] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load blocked peers: %w", err)
	}
	return nil
}
