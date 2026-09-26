package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"jellymesh/internal/group"
	"jellymesh/internal/policy"
)

var (
	// ErrMembershipStateRequired means Save was asked to persist a nil state.
	// There is nothing meaningful to snapshot, so this is refused outright
	// rather than silently writing an empty group.
	ErrMembershipStateRequired = errors.New("a membership state is required")

	// ErrMembershipRolesRequired means the state's Roles field, which owns the
	// owner, admin pool, and member roster, was nil. Roles is authoritative
	// for exactly the fields group_state and members exist to persist, so a
	// state without it cannot be snapshotted.
	ErrMembershipRolesRequired = errors.New("membership state must include role state")

	// ErrMembershipGroupIDRequired means no group id was given to key the
	// snapshot, lookup, or delete by.
	ErrMembershipGroupIDRequired = errors.New("a group ID is required")
)

// MembershipRepository is the durable equivalent of policy.State together
// with the group.State it embeds: the owner, admin pool, member roster,
// succession and dissolution timers, publications (live and staged),
// opt-outs, and invitations that must survive a restart per C-PO-8.
//
// It does not introduce a second copy of the domain rules. Save and Load
// move a policy.State to and from SQLite as a single snapshot; the rules
// about what transitions are legal stay in internal/policy and
// internal/group, exactly as PeerRepository defers to transport for what
// "trusted" means and only stores the decision.
type MembershipRepository struct {
	database *DB
}

// NewMembershipRepository builds a MembershipRepository over an already-open
// database.
func NewMembershipRepository(database *DB) *MembershipRepository {
	return &MembershipRepository{database: database}
}

// Save writes the entire group state as one atomic snapshot, replacing
// whatever was previously stored for state.GroupID. It is a wholesale
// replace rather than a diff: the design's own words are that a partially
// applied membership change must never be observable, and the simplest way
// to guarantee that across five tables is to delete and rewrite all of them
// inside a single transaction rather than trying to reconcile row by row.
func (repo *MembershipRepository) Save(ctx context.Context, state *policy.State) error {
	if state == nil {
		return ErrMembershipStateRequired
	}
	if state.Roles == nil {
		return ErrMembershipRolesRequired
	}
	groupID := strings.TrimSpace(state.GroupID)
	if groupID == "" {
		return ErrMembershipGroupIDRequired
	}

	memberRows := buildMemberRows(state.Roles, state.EjectedMembers)

	return repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		// Deleting group_state cascades to members (see schema.go's FOREIGN
		// KEY ... ON DELETE CASCADE), but publications, opt_outs, and
		// invitations have no such foreign key, so they are cleared
		// explicitly. Order matters only in that group_state is recreated
		// before members is repopulated, since members.group_id references
		// it and foreign_keys=ON is enforced.
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_state WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("clear existing group state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM publications WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("clear existing publications: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM opt_outs WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("clear existing opt-outs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("clear existing invitations: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO group_state (
				group_id, owner_id, status, membership_sequence,
				owner_unavailable_since, succession_deadline, dissolution_deadline, last_notification_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			groupID, state.Roles.OwnerID, string(state.Roles.Status), int64(state.MembershipSequence),
			FormatTime(state.Roles.OwnerUnavailableSince), FormatTime(state.Roles.SuccessionDeadline),
			FormatTime(state.Roles.DissolutionDeadline), FormatTime(state.Roles.LastNotificationAt),
		); err != nil {
			return fmt.Errorf("insert group state: %w", err)
		}

		for nodeID, row := range memberRows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO members (group_id, node_id, is_admin, promoted_at, ejected)
				VALUES (?, ?, ?, ?, ?)`,
				groupID, nodeID, boolToInt(row.isAdmin), FormatTime(row.promotedAt), boolToInt(row.ejected),
			); err != nil {
				return fmt.Errorf("insert member %q: %w", nodeID, err)
			}
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

		if err := persistBlockedPeers(ctx, tx, state.BlockedPeers); err != nil {
			return err
		}

		return nil
	})
}

// Load reconstructs a policy.State, including its group.State roles, exactly
// as it was saved. The boolean reports whether the group existed; an unknown
// group id is not an error; the caller is expected to treat that as "first
// run, nothing to restore" rather than a failure.
func (repo *MembershipRepository) Load(ctx context.Context, groupID string) (*policy.State, bool, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, false, ErrMembershipGroupIDRequired
	}

	var (
		ownerID                                                                 string
		status                                                                  string
		membershipSequence                                                      int64
		ownerUnavailableSinceRaw, successionDeadlineRaw, dissolutionDeadlineRaw string
		lastNotificationAtRaw                                                   string
	)
	err := repo.database.SQL().QueryRowContext(ctx, `
		SELECT owner_id, status, membership_sequence,
		       owner_unavailable_since, succession_deadline, dissolution_deadline, last_notification_at
		FROM group_state WHERE group_id = ?`, groupID,
	).Scan(&ownerID, &status, &membershipSequence,
		&ownerUnavailableSinceRaw, &successionDeadlineRaw, &dissolutionDeadlineRaw, &lastNotificationAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load group state: %w", err)
	}

	roles := &group.State{
		GroupID: groupID,
		OwnerID: ownerID,
		Members: make(map[string]bool),
		Admins:  make(map[string]group.Administrator),
		Status:  group.Status(status),
	}
	if roles.OwnerUnavailableSince, err = ParseTime(ownerUnavailableSinceRaw); err != nil {
		return nil, false, fmt.Errorf("parse owner_unavailable_since: %w", err)
	}
	if roles.SuccessionDeadline, err = ParseTime(successionDeadlineRaw); err != nil {
		return nil, false, fmt.Errorf("parse succession_deadline: %w", err)
	}
	if roles.DissolutionDeadline, err = ParseTime(dissolutionDeadlineRaw); err != nil {
		return nil, false, fmt.Errorf("parse dissolution_deadline: %w", err)
	}
	if roles.LastNotificationAt, err = ParseTime(lastNotificationAtRaw); err != nil {
		return nil, false, fmt.Errorf("parse last_notification_at: %w", err)
	}

	state := &policy.State{
		GroupID:            groupID,
		Roles:              roles,
		Published:          make(map[string]policy.Publication),
		Candidates:         make(map[string]policy.Publication),
		OptOuts:            make(map[string]map[string]policy.OptOut),
		BlockedPeers:       make(map[string]bool),
		Invitations:        make(map[string]policy.Invitation),
		EjectedMembers:     make(map[string]bool),
		MembershipSequence: uint64(membershipSequence),
	}

	if err := loadMembers(ctx, repo.database.SQL(), groupID, roles, state.EjectedMembers); err != nil {
		return nil, false, err
	}
	if err := loadPublications(ctx, repo.database.SQL(), groupID, state); err != nil {
		return nil, false, err
	}
	if err := loadOptOuts(ctx, repo.database.SQL(), groupID, state); err != nil {
		return nil, false, err
	}
	if err := loadInvitations(ctx, repo.database.SQL(), groupID, state); err != nil {
		return nil, false, err
	}
	if err := loadBlockedPeers(ctx, repo.database.SQL(), state); err != nil {
		return nil, false, err
	}

	return state, true, nil
}

// Delete removes every trace of a group's stored state. Deleting a group
// that does not exist is not an error, mirroring RetentionRepository.Discard:
// the caller's intent ("this group should not be stored") is already
// satisfied.
func (repo *MembershipRepository) Delete(ctx context.Context, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return ErrMembershipGroupIDRequired
	}
	return repo.database.WithTx(ctx, func(tx *sql.Tx) error {
		// members cascades from group_state; publications, opt_outs, and
		// invitations have no foreign key and are cleared explicitly, exactly
		// as in Save.
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_state WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("delete group state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM publications WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("delete publications: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM opt_outs WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("delete opt-outs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE group_id = ?`, groupID); err != nil {
			return fmt.Errorf("delete invitations: %w", err)
		}
		return nil
	})
}

// ListGroupIDs returns every group id with stored state, for a node that
// needs to enumerate what it belongs to on startup.
func (repo *MembershipRepository) ListGroupIDs(ctx context.Context) ([]string, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, `SELECT group_id FROM group_state ORDER BY group_id`)
	if err != nil {
		return nil, fmt.Errorf("list group ids: %w", err)
	}
	defer rows.Close()

	var groupIDs []string
	for rows.Next() {
		var groupID string
		if err := rows.Scan(&groupID); err != nil {
			return nil, fmt.Errorf("scan group id: %w", err)
		}
		groupIDs = append(groupIDs, groupID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list group ids: %w", err)
	}
	return groupIDs, nil
}

// ---------------------------------------------------------------------------
// Member, admin, and ejection merging.
// ---------------------------------------------------------------------------

// membershipRow is the merged, per-node-id shape of the members table: the
// union of what group.State.Members, group.State.Admins, and
// policy.State.EjectedMembers each know about one node.
type membershipRow struct {
	isAdmin    bool
	promotedAt time.Time
	ejected    bool
}

// buildMemberRows merges the three membership-adjacent maps policy.State and
// group.State keep (Members, Admins, EjectedMembers) into one row per node,
// matching the members table's single (group_id, node_id) primary key.
//
// The domain never puts a node in both Members and EjectedMembers at once:
// EjectMember and ApplyVerifiedRevocation always remove a node from
// Roles.Members before adding it to EjectedMembers, and
// ApplyVerifiedAdmission always clears EjectedMembers before adding the node
// back to Roles.Members (see internal/policy/policy.go). If that invariant
// is ever violated, active membership wins here, so a bug elsewhere cannot
// spuriously lock a current member out of its own group after a restart.
func buildMemberRows(roles *group.State, ejectedMembers map[string]bool) map[string]membershipRow {
	rows := make(map[string]membershipRow, len(roles.Members)+len(ejectedMembers))
	for nodeID := range roles.Members {
		rows[nodeID] = membershipRow{}
	}
	for nodeID, admin := range roles.Admins {
		row := rows[nodeID]
		row.isAdmin = true
		row.promotedAt = admin.PromotedAt
		rows[nodeID] = row
	}
	for nodeID := range ejectedMembers {
		if _, active := roles.Members[nodeID]; active {
			continue
		}
		rows[nodeID] = membershipRow{ejected: true}
	}
	return rows
}

func loadMembers(ctx context.Context, db *sql.DB, groupID string, roles *group.State, ejectedMembers map[string]bool) error {
	rows, err := db.QueryContext(ctx,
		`SELECT node_id, is_admin, promoted_at, ejected FROM members WHERE group_id = ?`, groupID)
	if err != nil {
		return fmt.Errorf("load members: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			nodeID           string
			isAdmin, ejected int
			promotedAtRaw    string
		)
		if err := rows.Scan(&nodeID, &isAdmin, &promotedAtRaw, &ejected); err != nil {
			return fmt.Errorf("scan member: %w", err)
		}
		if ejected != 0 {
			ejectedMembers[nodeID] = true
			continue
		}
		roles.Members[nodeID] = true
		if isAdmin != 0 {
			promotedAt, err := ParseTime(promotedAtRaw)
			if err != nil {
				return fmt.Errorf("parse promoted_at for %q: %w", nodeID, err)
			}
			roles.Admins[nodeID] = group.Administrator{PromotedAt: promotedAt}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load members: %w", err)
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
