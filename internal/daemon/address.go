package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"jellymesh/internal/enrollment"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
)

// readvertiseInterval spaces this node's proposals to change its advertised
// address, so a refused one is not resent on every heartbeat.
const readvertiseInterval = 10 * time.Minute

// addressCheckTimeout bounds the owner's dial of a member's new address.
const addressCheckTimeout = 15 * time.Second

// vetProposal is the owner's check before it sequences a proposal (#62): a
// member's new address must answer with that member's key, as a joiner's
// must (C-NT-1). The owner's own change is not dialled, since a node often
// cannot reach its own public address from inside its network.
func (n *Node) vetProposal(group *membership.Group) func(context.Context, grouplog.Proposal) error {
	return func(ctx context.Context, proposal grouplog.Proposal) error {
		if proposal.Kind != grouplog.KindAddress {
			return nil
		}
		var body grouplog.AddressBody
		if err := json.Unmarshal(proposal.Body, &body); err != nil {
			return fmt.Errorf("%w: %v", grouplog.ErrMalformedPayload, err)
		}
		if body.MemberID == n.nodeID {
			return nil
		}
		var fingerprint node.Fingerprint
		var known bool
		_ = group.View(func(state *grouplog.State) {
			if member, ok := state.Member(body.MemberID); ok {
				fingerprint, known = member.Fingerprint, true
			}
		})
		if !known || !grouplog.ValidAddress(body.PublicHostname) {
			return nil // the log's own rules refuse it, with their reason
		}
		ctx, cancel := context.WithTimeout(ctx, addressCheckTimeout)
		defer cancel()
		return enrollment.CheckAdvertisedAddress(ctx, n.identity, body.PublicHostname, fingerprint)
	}
}

// AddressStatus says whether the address this node is configured to
// advertise is the one the group has.
type AddressStatus struct {
	Configured string `json:"configured"`
	InGroup    string `json:"in_group,omitempty"`
	// Change is the outcome of this node's last proposal to change it:
	// "sequenced", "queued" while the owner is away, or why it was refused.
	Change string `json:"change,omitempty"`
}

// readvertise proposes this node's configured address to the group when it
// differs from the address the group has, at most every
// readvertiseInterval. Members dial the new address once the owner, having
// reached this node there, sequences it.
func (n *Node) readvertise(ctx context.Context) {
	runtime, err := n.current()
	if err != nil {
		return
	}
	want := n.cfg.PublicAddress()
	var have string
	_ = runtime.group.View(func(state *grouplog.State) {
		if member, ok := state.Member(n.nodeID); ok {
			have = member.PublicHostname
		}
	})
	n.mutex.Lock()
	due := have != "" && have != want && grouplog.ValidAddress(want) && time.Since(n.readvertised) >= readvertiseInterval
	if due {
		n.readvertised = time.Now()
	}
	n.mutex.Unlock()
	if !due {
		return
	}
	outcome, err := n.Propose(ctx, grouplog.KindAddress, grouplog.AddressBody{MemberID: n.nodeID, PublicHostname: want})
	change := "sequenced"
	switch {
	case err != nil:
		change = "refused: " + err.Error()
	case outcome.Queued:
		change = "queued until the owner is reachable"
	}
	n.mutex.Lock()
	n.addressChange = change
	n.mutex.Unlock()
	n.logger.Printf("advertised address %s → %s: %s", have, want, change)
	if err == nil && outcome.Sequenced {
		// Members pull the log from the addresses they know, which no
		// longer include this node's; hand them the change directly.
		for _, peer := range n.otherMembers(runtime) {
			ctx, cancel := context.WithTimeout(ctx, addressCheckTimeout)
			if err := n.client.Push(ctx, peer, runtime.id, outcome.Event); err != nil {
				n.logger.Printf("telling %s of the new address: %v", peer.Address, err)
			}
			cancel()
		}
	}
}

func (n *Node) addressStatus() AddressStatus {
	status := AddressStatus{Configured: n.cfg.PublicAddress()}
	if runtime, err := n.current(); err == nil {
		_ = runtime.group.View(func(state *grouplog.State) {
			if member, ok := state.Member(n.nodeID); ok {
				status.InGroup = member.PublicHostname
			}
		})
	}
	n.mutex.Lock()
	status.Change = n.addressChange
	n.mutex.Unlock()
	return status
}
