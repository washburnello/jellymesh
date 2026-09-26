// Package membership holds a node's view of its group: the verified group log,
// kept durable, and the transport trust derived from it.
//
// Trust is not a separate decision (design-spec section 8, "What is replicated
// and what is not"). A peer may complete a mutual-TLS handshake exactly when
// its key belongs to an active member of the group log and this node has not
// blocked it. Admission therefore trusts a member on every node that applies
// the admission, and ejection untrusts it on every node that applies the
// ejection, with no second record to fall out of step.
package membership

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

// LogStore is where the group log is kept; store.GroupLogRepository
// satisfies it.
type LogStore interface {
	Load(ctx context.Context, groupID string) (*grouplog.Log, bool, error)
	Save(ctx context.Context, log *grouplog.Log) error
}

// BlockList reports this node's local blocks; store.PeerRepository
// satisfies it.
type BlockList interface {
	IsBlocked(ctx context.Context, nodeID string) (bool, error)
}

var (
	ErrGroupNotFound = errors.New("no log is stored for this group")

	// ErrGroupUnavailable means a save failed and the stored log could not be
	// reloaded either. The group refuses all operations, and all trust,
	// until it is opened again.
	ErrGroupUnavailable = errors.New("group log is unavailable after a storage failure")
)

// trustQueryTimeout bounds the block lookup inside IsTrusted, which runs on
// every inbound handshake and has no error return: a wedged database must
// fail the handshake promptly rather than hang it.
const trustQueryTimeout = 2 * time.Second

// Group is this node's copy of one group's log. It is safe for concurrent
// use: handshakes read trust while replication applies events.
type Group struct {
	mutex  sync.RWMutex
	log    *grouplog.Log
	store  LogStore
	blocks BlockList
}

// Found creates a new group owned by identity and stores its genesis event.
func Found(ctx context.Context, store LogStore, blocks BlockList, identity *node.Identity, groupID string, ownerID string, friendlyName string, publicHostname string, now time.Time) (*Group, error) {
	log, err := grouplog.Create(identity, groupID, ownerID, friendlyName, publicHostname, now)
	if err != nil {
		return nil, err
	}
	if err := store.Save(ctx, log); err != nil {
		return nil, fmt.Errorf("store genesis: %w", err)
	}
	return &Group{log: log, store: store, blocks: blocks}, nil
}

// Join adopts a log downloaded from a peer, anchored to the genesis hash the
// invitation carried, and stores it.
func Join(ctx context.Context, store LogStore, blocks BlockList, events []grouplog.Event, genesis grouplog.Hash) (*Group, error) {
	log, err := grouplog.ReplayAnchored(events, genesis)
	if err != nil {
		return nil, err
	}
	if err := store.Save(ctx, log); err != nil {
		return nil, fmt.Errorf("store joined log: %w", err)
	}
	return &Group{log: log, store: store, blocks: blocks}, nil
}

// Open loads a stored group, re-verifying its log from genesis.
func Open(ctx context.Context, store LogStore, blocks BlockList, groupID string) (*Group, error) {
	log, found, err := store.Load(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrGroupNotFound
	}
	return &Group{log: log, store: store, blocks: blocks}, nil
}

// Receive applies an event from a peer and stores the result. An event ahead
// of the head is held until the gap fills. Equivocation is stored as well, so
// that the halt it causes survives a restart.
func (group *Group) Receive(ctx context.Context, event grouplog.Event) (grouplog.Outcome, error) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.log == nil {
		return 0, ErrGroupUnavailable
	}
	outcome, err := group.log.Offer(event)
	if errors.Is(err, grouplog.ErrEquivocation) {
		if saveErr := group.store.Save(ctx, group.log); saveErr != nil {
			return 0, errors.Join(err, fmt.Errorf("store equivocation evidence: %w", saveErr))
		}
		return 0, err
	}
	if err != nil {
		return 0, err
	}
	if outcome == grouplog.Duplicate || outcome == grouplog.Held {
		return outcome, nil
	}
	if err := group.store.Save(ctx, group.log); err != nil {
		// The event is still valid and a peer will serve it again; restore
		// the stored log so memory never runs ahead of disk.
		return 0, group.reloadAfter(ctx, fmt.Errorf("store received event: %w", err))
	}
	return outcome, nil
}

// Sequence is the owner's operation: it embeds a proposal as the next event
// and stores it before returning, so the event is never published unless it
// is durable. If storing fails the event is discarded, which keeps the owner
// from ever sequencing two different events for one slot.
func (group *Group) Sequence(ctx context.Context, identity *node.Identity, proposal grouplog.Proposal, now time.Time) (grouplog.Event, error) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.log == nil {
		return grouplog.Event{}, ErrGroupUnavailable
	}
	event, err := group.log.Sequence(identity, proposal, now)
	if err != nil {
		return grouplog.Event{}, err
	}
	if err := group.store.Save(ctx, group.log); err != nil {
		return grouplog.Event{}, group.reloadAfter(ctx, fmt.Errorf("store sequenced event: %w", err))
	}
	return event, nil
}

// Claim is the eligible successor's operation, stored before returning for
// the same reason as Sequence.
func (group *Group) Claim(ctx context.Context, identity *node.Identity, claimantID string, attestations []grouplog.Attestation, now time.Time) (grouplog.Event, error) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.log == nil {
		return grouplog.Event{}, ErrGroupUnavailable
	}
	event, err := group.log.Claim(identity, claimantID, attestations, now)
	if err != nil {
		return grouplog.Event{}, err
	}
	if err := group.store.Save(ctx, group.log); err != nil {
		return grouplog.Event{}, group.reloadAfter(ctx, fmt.Errorf("store succession: %w", err))
	}
	return event, nil
}

// reloadAfter replaces the in-memory log with the stored one after a failed
// save. If even that fails the group refuses everything, including trust,
// rather than carry on from a state that disk does not hold.
func (group *Group) reloadAfter(ctx context.Context, cause error) error {
	log, found, err := group.store.Load(ctx, group.log.State().GroupID)
	if err != nil || !found {
		group.log = nil
		return errors.Join(cause, fmt.Errorf("reload stored log: found=%v: %w", found, err))
	}
	group.log = log
	return cause
}

// IsTrusted satisfies transport.TrustStore. It fails closed: an empty
// fingerprint, a key that belongs to no active member, a blocked member, an
// unusable group, and any error looking up the block all return false.
func (group *Group) IsTrusted(fingerprint transport.Fingerprint) bool {
	if fingerprint == "" || group == nil {
		return false
	}
	group.mutex.RLock()
	defer group.mutex.RUnlock()
	if group.log == nil {
		return false
	}
	if _, halted := group.log.Halted(); halted {
		// An equivocating owner means this node no longer knows which roster
		// is true, so it trusts nobody until an operator intervenes.
		return false
	}
	member, ok := group.log.State().MemberByFingerprint(fingerprint)
	if !ok {
		return false
	}
	if group.blocks == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustQueryTimeout)
	defer cancel()
	blocked, err := group.blocks.IsBlocked(ctx, member.NodeID)
	return err == nil && !blocked
}

// Head returns the head of this node's log, for the heartbeat exchange.
func (group *Group) Head() grouplog.Head {
	group.mutex.RLock()
	defer group.mutex.RUnlock()
	if group.log == nil {
		return grouplog.Head{}
	}
	return group.log.Head()
}

// EventsAfter returns the events after sequence, for serving a peer.
func (group *Group) EventsAfter(sequence uint64) []grouplog.Event {
	group.mutex.RLock()
	defer group.mutex.RUnlock()
	if group.log == nil {
		return nil
	}
	return group.log.EventsAfter(sequence)
}

// View runs read against the current group state under the group's read
// lock. The state must not be retained after read returns, because applying
// the next event mutates it.
func (group *Group) View(read func(state *grouplog.State)) error {
	group.mutex.RLock()
	defer group.mutex.RUnlock()
	if group.log == nil {
		return ErrGroupUnavailable
	}
	read(group.log.State())
	return nil
}
