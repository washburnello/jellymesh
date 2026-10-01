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
	"strconv"
	"sync"
	"time"

	"jellymesh/internal/audit"
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

// SequencingHold is implemented by a LogStore that can hold this node back
// from sequencing; store.GroupLogRepository does. A node restored from a
// backup may be missing events it had already published. If it sequenced
// before catching up, it would sign a second event for a slot its peers have
// already filled, which is equivocation, and every member would halt.
type SequencingHold interface {
	SequencingHeld(ctx context.Context) (bool, error)
	ReleaseSequencing(ctx context.Context) error
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

	// ErrSequencingHeld means this node was restored from a backup and has
	// not yet confirmed that it holds the latest log.
	ErrSequencingHeld = errors.New("this node was restored and must catch up with its peers before sequencing")
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
	audit  *audit.Log
	// vet, when set, checks a proposal against the world before the owner
	// sequences it, such as that a member's new address answers with its
	// key. The log's rules are checked as well, always.
	vet func(ctx context.Context, proposal grouplog.Proposal) error
}

// SetVet sets the check the owner runs on each proposal before sequencing
// it.
func (group *Group) SetVet(vet func(ctx context.Context, proposal grouplog.Proposal) error) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.vet = vet
}

// SetAudit records changes to the group log to log.
func (group *Group) SetAudit(log *audit.Log) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.audit = log
}

func (group *Group) record(ctx context.Context, action string, event grouplog.Event) {
	_ = group.audit.Record(ctx, event.SignerID, action, event.GroupID, map[string]string{
		"group_id": event.GroupID,
		"kind":     string(event.Kind),
		"sequence": strconv.FormatUint(event.Sequence, 10),
		"epoch":    strconv.FormatUint(event.Epoch, 10),
	})
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
		group.record(ctx, "group.equivocation", event)
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
	if outcome == grouplog.Superseded {
		group.record(ctx, "group.superseded", event)
	} else {
		group.record(ctx, "group.event_applied", event)
	}
	return outcome, nil
}

// Sequence is the owner's operation: it embeds a proposal as the next event
// and stores it before returning, so the event is never published unless it
// is durable. If storing fails the event is discarded, which keeps the owner
// from ever sequencing two different events for one slot.
func (group *Group) Sequence(ctx context.Context, identity *node.Identity, proposal grouplog.Proposal, now time.Time) (grouplog.Event, error) {
	// The check may dial out, so it runs before the lock, which
	// handshakes need.
	group.mutex.RLock()
	vet := group.vet
	group.mutex.RUnlock()
	if vet != nil {
		if err := vet(ctx, proposal); err != nil {
			return grouplog.Event{}, err
		}
	}
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.log == nil {
		return grouplog.Event{}, ErrGroupUnavailable
	}
	if err := group.checkHold(ctx); err != nil {
		return grouplog.Event{}, err
	}
	event, err := group.log.Sequence(identity, proposal, now)
	if err != nil {
		return grouplog.Event{}, err
	}
	if err := group.store.Save(ctx, group.log); err != nil {
		return grouplog.Event{}, group.reloadAfter(ctx, fmt.Errorf("store sequenced event: %w", err))
	}
	group.record(ctx, "group.event_sequenced", event)
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
	if err := group.checkHold(ctx); err != nil {
		return grouplog.Event{}, err
	}
	event, err := group.log.Claim(identity, claimantID, attestations, now)
	if err != nil {
		return grouplog.Event{}, err
	}
	if err := group.store.Save(ctx, group.log); err != nil {
		return grouplog.Event{}, group.reloadAfter(ctx, fmt.Errorf("store succession: %w", err))
	}
	group.record(ctx, "group.succession_claimed", event)
	return event, nil
}

// checkHold refuses to sequence while a restore hold is in place. It fails
// closed: a hold that cannot be read is treated as held.
func (group *Group) checkHold(ctx context.Context) error {
	hold, ok := group.store.(SequencingHold)
	if !ok {
		return nil
	}
	held, err := hold.SequencingHeld(ctx)
	if err != nil || held {
		return errors.Join(ErrSequencingHeld, err)
	}
	return nil
}

// ConfirmCaughtUp lifts a restore hold. The caller asserts that it has synced
// with peers and that no peer holds events in this node's epoch beyond its
// head, which replication.Sync establishes when it returns nothing to apply.
func (group *Group) ConfirmCaughtUp(ctx context.Context) error {
	hold, ok := group.store.(SequencingHold)
	if !ok {
		return nil
	}
	if err := hold.ReleaseSequencing(ctx); err != nil {
		return err
	}
	group.mutex.RLock()
	defer group.mutex.RUnlock()
	if group.log != nil {
		_ = group.audit.Record(ctx, "local", "group.sequencing_released", group.log.State().GroupID, map[string]string{"group_id": group.log.State().GroupID})
	}
	return nil
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
