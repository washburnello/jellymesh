package grouplog

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"jellymesh/internal/node"
)

var (
	// ErrDoesNotExtend means an event's sequence or previous hash does not
	// follow this log's head.
	ErrDoesNotExtend = errors.New("event does not extend the log head")

	// ErrGap means an event is ahead of this log. It is not invalid: the
	// events between are missing and must be fetched. Offer holds such an
	// event until they arrive rather than discarding it.
	ErrGap = errors.New("event is ahead of the log; the events before it are missing")

	// ErrFenced means an event belongs to an epoch that a succession has
	// already closed at this point in the log. A returning former owner's
	// events land here.
	ErrFenced = errors.New("event belongs to an epoch closed by succession")

	// ErrEquivocation means two different events, each correctly signed by
	// the owner of the same epoch, claim the same slot. That can only happen
	// through a compromised or misbehaving owner, or an owner restored from a
	// stale backup, so the log stops rather than choosing one.
	ErrEquivocation = errors.New("two correctly signed events claim the same slot")

	// ErrHalted means the log has stopped after detecting equivocation and
	// accepts nothing further until an operator intervenes.
	ErrHalted = errors.New("log is halted after equivocation")

	// ErrConflict means an event conflicts with this log in a way that is
	// neither a valid succession nor provable equivocation.
	ErrConflict = errors.New("event conflicts with the log")
)

// maxPending bounds how many out-of-order events Offer holds, so a peer
// cannot exhaust memory by sending events far ahead of the head.
const maxPending = 1024

// Equivocation is the evidence kept when two events claim one slot.
type Equivocation struct {
	Existing Event
	Received Event
}

// Outcome reports what Append did with an event.
type Outcome int

const (
	// Applied means the event extended the log.
	Applied Outcome = iota
	// Duplicate means the log already held this exact event.
	Duplicate
	// Superseded means the event was a succession that replaced events of
	// the closed epoch; Log.Superseded returns what was removed.
	Superseded
	// Held means Offer kept an out-of-order event to apply later.
	Held
)

// Log is a verified group log and the state derived from it.
type Log struct {
	events     []Event
	state      *State
	pending    map[uint64]Event
	superseded []Event
	halted     *Equivocation
}

// Create founds a group: it signs the genesis event with the owner's identity
// and returns a log containing it. The genesis hash, from Head, is what an
// invitation carries so that joiners can anchor the log.
func Create(identity *node.Identity, groupID string, ownerID string, friendlyName string, publicHostname string, issuedAt time.Time) (*Log, error) {
	if identity == nil {
		return nil, errors.New("identity is required")
	}
	payload, err := encodeBody(GenesisBody{
		OwnerID:        strings.TrimSpace(ownerID),
		OwnerKey:       identity.PublicKey(),
		FriendlyName:   friendlyName,
		PublicHostname: publicHostname,
	})
	if err != nil {
		return nil, err
	}
	genesis, err := signEvent(identity, Event{
		GroupID:  strings.TrimSpace(groupID),
		Epoch:    1,
		Sequence: 1,
		Kind:     KindGenesis,
		IssuedAt: issuedAt,
		SignerID: strings.TrimSpace(ownerID),
		Payload:  payload,
	})
	if err != nil {
		return nil, err
	}
	return Replay([]Event{genesis})
}

// Replay verifies events from genesis and returns the resulting log. State
// is a pure function of the events: any node replaying the same events gets
// the same state (C-PO-20). Stored logs are loaded this way too, so a
// tampered database is refused rather than trusted.
func Replay(events []Event) (*Log, error) {
	log := &Log{state: newState(), pending: make(map[uint64]Event)}
	for _, event := range events {
		if err := log.state.apply(event); err != nil {
			return nil, fmt.Errorf("replay event %d: %w", event.Sequence, err)
		}
		log.events = append(log.events, event)
	}
	return log, nil
}

// ReplayAnchored replays events and additionally requires that the first is
// the genesis event with the given hash. A joining node uses it with the
// genesis hash from its invitation, which is the only thing that ties a
// downloaded log to the group the node was actually invited to.
func ReplayAnchored(events []Event, genesis Hash) (*Log, error) {
	if len(events) == 0 || events[0].Hash() != genesis {
		return nil, fmt.Errorf("%w: log does not begin with the invited genesis", ErrNotGenesis)
	}
	return Replay(events)
}

// State returns the current group state. Its accessors return copies, and its
// fields are exported only for reading.
func (log *Log) State() *State { return log.state }

// Head returns the head of the log.
func (log *Log) Head() Head { return log.state.Head }

// Genesis returns the hash of the genesis event.
func (log *Log) Genesis() Hash {
	if len(log.events) == 0 {
		return Hash{}
	}
	return log.events[0].Hash()
}

// EventsAfter returns the events following sequence, for serving a peer that
// is behind.
func (log *Log) EventsAfter(sequence uint64) []Event {
	if sequence >= uint64(len(log.events)) {
		return nil
	}
	return append([]Event(nil), log.events[sequence:]...)
}

// Superseded returns the old-epoch events removed by the most recent
// succession, so that any proposals they carried can be proposed again.
func (log *Log) Superseded() []Event { return append([]Event(nil), log.superseded...) }

// Halted returns the equivocation evidence if the log has stopped.
func (log *Log) Halted() (Equivocation, bool) {
	if log.halted == nil {
		return Equivocation{}, false
	}
	return *log.halted, true
}

// RestoreHalt re-establishes a halt recorded before a restart. The evidence
// must be two different events for one slot of this group; anything else is
// refused, so a corrupted record cannot silently halt or fail to halt.
func (log *Log) RestoreHalt(evidence Equivocation) error {
	existing, received := evidence.Existing, evidence.Received
	if existing.GroupID != log.state.GroupID || received.GroupID != log.state.GroupID ||
		existing.Sequence != received.Sequence || existing.Epoch != received.Epoch ||
		existing.Hash() == received.Hash() {
		return fmt.Errorf("%w: evidence does not describe two events for one slot", ErrConflict)
	}
	log.halted = &evidence
	return nil
}

// Append adds an event received from a peer or produced locally.
func (log *Log) Append(event Event) (Outcome, error) {
	if log.halted != nil {
		return 0, ErrHalted
	}
	head := log.state.Head.Sequence
	switch {
	case event.Sequence == head+1:
		if err := log.state.apply(event); err != nil {
			return 0, err
		}
		log.events = append(log.events, event)
		log.drainPending()
		return Applied, nil
	case event.Sequence > head+1:
		return 0, fmt.Errorf("%w: head is %d, event is %d", ErrGap, head, event.Sequence)
	case event.Sequence == 0:
		return 0, fmt.Errorf("%w: sequence zero", ErrDoesNotExtend)
	default:
		return log.reconcile(event)
	}
}

// Offer is Append for events arriving from the network, where order is not
// guaranteed. An event ahead of the head is held and applied once the events
// before it arrive, instead of being dropped.
func (log *Log) Offer(event Event) (Outcome, error) {
	outcome, err := log.Append(event)
	if errors.Is(err, ErrGap) {
		if _, held := log.pending[event.Sequence]; !held && len(log.pending) >= maxPending {
			return 0, err
		}
		log.pending[event.Sequence] = event
		return Held, nil
	}
	return outcome, err
}

// Pending reports how many out-of-order events are being held.
func (log *Log) Pending() int { return len(log.pending) }

func (log *Log) drainPending() {
	for {
		next, ok := log.pending[log.state.Head.Sequence+1]
		if !ok {
			break
		}
		delete(log.pending, next.Sequence)
		if err := log.state.apply(next); err != nil {
			// A held event that does not fit is discarded; the peer that
			// sent it will serve the correct one on the next fetch.
			continue
		}
		log.events = append(log.events, next)
	}
	for sequence := range log.pending {
		if sequence <= log.state.Head.Sequence {
			delete(log.pending, sequence)
		}
	}
}

// reconcile handles an event for a slot this log has already filled.
func (log *Log) reconcile(event Event) (Outcome, error) {
	index := event.Sequence - 1
	existing := log.events[index]
	if existing.Hash() == event.Hash() {
		return Duplicate, nil
	}
	if index == 0 {
		// Anyone can sign a genesis event. A different one proves nothing
		// about this group's owner; the invitation's genesis hash decides
		// which log a node follows, so this is a conflict, never a halt.
		return 0, fmt.Errorf("%w: a different genesis event", ErrConflict)
	}

	// Check the event against the state this log had just before that slot.
	// Anything that fails here is simply invalid; it proves nothing about
	// the owner and must not halt the log.
	before, err := Replay(log.events[:index])
	if err != nil {
		return 0, fmt.Errorf("rebuild state before slot %d: %w", event.Sequence, err)
	}
	candidate := before.state
	if err := candidate.apply(event); err != nil {
		if event.Epoch < existing.Epoch {
			return 0, fmt.Errorf("%w: %v", ErrFenced, err)
		}
		return 0, err
	}

	switch {
	case event.Kind == KindSuccession && event.Epoch > log.maxEpochFrom(index):
		// A valid succession for a slot that old-epoch events filled. Those
		// events were sequenced by an owner a quorum had already attested
		// absent, so the succession wins and they are set aside.
		log.superseded = append([]Event(nil), log.events[index:]...)
		log.events = append(append([]Event(nil), log.events[:index]...), event)
		log.state = candidate
		log.pending = make(map[uint64]Event)
		return Superseded, nil
	case event.Epoch < existing.Epoch:
		return 0, ErrFenced
	case event.Epoch == existing.Epoch:
		// Both events are valid against the same prior state and share an
		// epoch, so the same key signed both: the owner, or a claimant that
		// signed two different claims.
		evidence := Equivocation{Existing: existing, Received: event}
		log.halted = &evidence
		return 0, fmt.Errorf("%w: slot %d in epoch %d", ErrEquivocation, event.Sequence, event.Epoch)
	default:
		return 0, fmt.Errorf("%w: slot %d", ErrConflict, event.Sequence)
	}
}

func (log *Log) maxEpochFrom(index uint64) uint64 {
	var highest uint64
	for _, event := range log.events[index:] {
		if event.Epoch > highest {
			highest = event.Epoch
		}
	}
	return highest
}

// Sequence is the owner's operation: it embeds a proposal in a new event,
// signs it, and applies it. It refuses unless identity is the owner, and the
// event is checked by exactly the rules every receiver will apply, so the
// owner cannot produce an event its own log would refuse.
func (log *Log) Sequence(identity *node.Identity, proposal Proposal, issuedAt time.Time) (Event, error) {
	if log.halted != nil {
		return Event{}, ErrHalted
	}
	state := log.state
	owner, ok := state.Member(state.OwnerID)
	if !ok || identity == nil || owner.Fingerprint != identity.Fingerprint() {
		return Event{}, ErrWrongSigner
	}
	payload, err := encodeBody(proposal)
	if err != nil {
		return Event{}, err
	}
	event, err := signEvent(identity, Event{
		GroupID:  state.GroupID,
		Epoch:    state.Epoch,
		Sequence: state.Head.Sequence + 1,
		PrevHash: state.Head.Hash,
		Kind:     proposal.Kind,
		IssuedAt: issuedAt,
		SignerID: state.OwnerID,
		Payload:  payload,
	})
	if err != nil {
		return Event{}, err
	}
	if _, err := log.Append(event); err != nil {
		return Event{}, err
	}
	return event, nil
}

// Claim is the eligible successor's operation: it appends a succession event
// justified by attestations. The attestations must name heads this log
// contains, so the claimant fetches up to the highest of them first.
func (log *Log) Claim(identity *node.Identity, claimantID string, attestations []Attestation, issuedAt time.Time) (Event, error) {
	if log.halted != nil {
		return Event{}, ErrHalted
	}
	state := log.state
	claimantID = strings.TrimSpace(claimantID)
	claimant, ok := state.Member(claimantID)
	if !ok || identity == nil || claimant.Fingerprint != identity.Fingerprint() {
		return Event{}, ErrNotSuccessor
	}
	payload, err := encodeBody(SuccessionBody{AbsentOwnerID: state.OwnerID, Attestations: attestations})
	if err != nil {
		return Event{}, err
	}
	event, err := signEvent(identity, Event{
		GroupID:  state.GroupID,
		Epoch:    state.Epoch + 1,
		Sequence: state.Head.Sequence + 1,
		PrevHash: state.Head.Hash,
		Kind:     KindSuccession,
		IssuedAt: issuedAt,
		SignerID: claimantID,
		Payload:  payload,
	})
	if err != nil {
		return Event{}, err
	}
	if _, err := log.Append(event); err != nil {
		return Event{}, err
	}
	return event, nil
}
