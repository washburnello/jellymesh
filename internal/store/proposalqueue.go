package store

import (
	"context"
	"encoding/json"
	"fmt"

	"jellymesh/internal/grouplog"
)

// ProposalQueue holds a member's signed proposals until the owner's node
// sequences or refuses them.
type ProposalQueue struct {
	database *DB
}

func NewProposalQueue(database *DB) *ProposalQueue {
	return &ProposalQueue{database: database}
}

// Enqueue stores a proposal. Queuing the same proposal twice is harmless.
func (queue *ProposalQueue) Enqueue(ctx context.Context, proposal grouplog.Proposal) error {
	encoded, err := json.Marshal(proposal)
	if err != nil {
		return err
	}
	if _, err := queue.database.SQL().ExecContext(ctx, `
		INSERT OR IGNORE INTO pending_proposals (proposal_id, group_id, proposal, queued_at) VALUES (?, ?, ?, ?)`,
		proposal.ID, proposal.GroupID, string(encoded), FormatTime(nowUTC()),
	); err != nil {
		return fmt.Errorf("queue proposal: %w", err)
	}
	return nil
}

// Pending returns a group's queued proposals, oldest first, so that decisions
// reach the owner in the order they were made.
func (queue *ProposalQueue) Pending(ctx context.Context, groupID string) ([]grouplog.Proposal, error) {
	rows, err := queue.database.SQL().QueryContext(ctx,
		`SELECT proposal FROM pending_proposals WHERE group_id = ? ORDER BY queued_at, proposal_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list queued proposals: %w", err)
	}
	defer rows.Close()
	var proposals []grouplog.Proposal
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var proposal grouplog.Proposal
		if err := json.Unmarshal([]byte(encoded), &proposal); err != nil {
			return nil, fmt.Errorf("decode queued proposal: %w", err)
		}
		proposals = append(proposals, proposal)
	}
	return proposals, rows.Err()
}

// Remove drops a proposal once the owner has sequenced or refused it.
func (queue *ProposalQueue) Remove(ctx context.Context, proposalID string) error {
	if _, err := queue.database.SQL().ExecContext(ctx, `DELETE FROM pending_proposals WHERE proposal_id = ?`, proposalID); err != nil {
		return fmt.Errorf("remove queued proposal: %w", err)
	}
	return nil
}
