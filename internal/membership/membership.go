// Package membership wires the group policy model to the layers that enforce
// it: durable state and the mutual-TLS trust store.
//
// The packages beneath this one are deliberately ignorant of each other.
// internal/policy decides who is a member, internal/events proves an event was
// signed, internal/transport decides who may open a connection, and
// internal/store remembers all of it. That separation is worth keeping, but it
// leaves real gaps at the seams, because a decision taken in one layer has no
// effect in another until something joins them:
//
//   - Ejecting a member removed it from the roster but left its fingerprint
//     trusted, so a removed or compromised node could still complete a
//     handshake (C-OP-3). Compromise recovery depends entirely on this,
//     because re-enrollment is only as good as the revocation that precedes it.
//   - A signed event proves who issued it, not that the issuer was entitled to
//     issue it. Signature verification alone accepts a correctly signed
//     admission from any ordinary member (C-PO-10).
//   - The replay guard lived only in memory, so a restarted node began with no
//     high-water mark and would re-admit a sequence it had already superseded
//     (C-PO-11).
//
// This package closes those three, and is the only place that needs to know
// about all four of the layers below it.
package membership

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	"jellymesh/internal/events"
	"jellymesh/internal/policy"
	"jellymesh/internal/store"
	"jellymesh/internal/transport"
)

var (
	ErrNotAuthorized   = errors.New("the event issuer is not an owner or administrator of this group")
	ErrIssuerUnknown   = errors.New("the event issuer is not a known peer")
	ErrGroupMismatch   = errors.New("the event is for a different group")
	ErrUnsupportedKind = errors.New("the event kind cannot be applied to membership state")
)

// PeerDirectory is the part of the peer store this package needs. Taking an
// interface rather than the concrete repository keeps the dependency one-way
// and makes the authorization path testable without a database.
type PeerDirectory interface {
	ByNodeID(ctx context.Context, nodeID string) (store.Peer, bool, error)
	SetTrusted(ctx context.Context, nodeID string, trusted bool) error
}

// Applier verifies group events and applies them to policy state, keeping the
// trust store and the replay guard consistent as it goes.
type Applier struct {
	state *policy.State
	peers PeerDirectory
	guard *events.SequenceGuard
}

// NewApplier seeds the replay guard from the durable membership sequence, so a
// node that has just restarted does not accept an event it already superseded.
// Starting a fresh guard at zero after every restart would silently reopen the
// replay window that the guard exists to close.
func NewApplier(state *policy.State, peers PeerDirectory) *Applier {
	guard := events.NewSequenceGuard()
	if state != nil && state.MembershipSequence > 0 {
		guard.Seed(state.GroupID, state.MembershipSequence)
	}
	return &Applier{state: state, peers: peers, guard: guard}
}

// Apply verifies an envelope end to end and applies it.
//
// Verification is in four parts, and all of them are required. The signature
// proves the envelope was not tampered with and identifies the signing key.
// The group check rejects an event replayed from another group. The role check
// is what signature verification cannot do on its own: a correctly signed
// admission from an ordinary member is cryptographically valid and must still
// be refused. The sequence guard rejects replays of events already applied.
func (applier *Applier) Apply(ctx context.Context, envelope events.Envelope, issuerKey ed25519.PublicKey) error {
	if applier == nil || applier.state == nil {
		return errors.New("applier has no policy state")
	}
	if err := envelope.Verify(issuerKey); err != nil {
		return fmt.Errorf("verify event: %w", err)
	}
	if envelope.GroupID != applier.state.GroupID {
		return ErrGroupMismatch
	}
	if err := applier.authorizeIssuer(ctx, envelope); err != nil {
		return err
	}
	if err := applier.guard.Admit(envelope.GroupID, envelope.Sequence); err != nil {
		return err
	}

	switch envelope.Kind {
	case events.KindAdmission:
		var admission policy.Admission
		if err := envelope.Decode(&admission); err != nil {
			return fmt.Errorf("decode admission: %w", err)
		}
		if err := applier.state.ApplyVerifiedAdmission(admission); err != nil {
			return err
		}
		// An admitted member becomes reachable at the transport layer.
		return applier.setTrust(ctx, admission.MemberID, true)

	case events.KindRevocation:
		var revocation policy.Revocation
		if err := envelope.Decode(&revocation); err != nil {
			return fmt.Errorf("decode revocation: %w", err)
		}
		if err := applier.state.ApplyVerifiedRevocation(revocation); err != nil {
			return err
		}
		return applier.setTrust(ctx, revocation.MemberID, false)

	default:
		return ErrUnsupportedKind
	}
}

// authorizeIssuer enforces that only an owner or administrator may issue
// membership events, and that the fingerprint in the envelope is the one this
// node has recorded for that member. Without the second half, a member could
// present an envelope naming an administrator as issuer while signing with its
// own key; the signature check alone would pass against whichever key the
// caller supplied.
func (applier *Applier) authorizeIssuer(ctx context.Context, envelope events.Envelope) error {
	if !applier.state.IsAdministrator(envelope.IssuerID) {
		return fmt.Errorf("%w: %s", ErrNotAuthorized, envelope.IssuerID)
	}
	peer, found, err := applier.peers.ByNodeID(ctx, envelope.IssuerID)
	if err != nil {
		return fmt.Errorf("look up issuer: %w", err)
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrIssuerUnknown, envelope.IssuerID)
	}
	if peer.Fingerprint != transport.Fingerprint(envelope.Issuer) {
		return fmt.Errorf("%w: issuer fingerprint does not match the recorded peer", ErrNotAuthorized)
	}
	return nil
}

// Eject removes an ordinary member and revokes its transport trust in the same
// operation. These must not drift apart: a node removed from the roster whose
// fingerprint stays trusted can still open a connection, which is precisely
// what compromise recovery by re-enrollment relies on not happening.
func (applier *Applier) Eject(ctx context.Context, actorID string, memberID string) error {
	if err := applier.state.EjectMember(actorID, memberID); err != nil {
		return err
	}
	return applier.setTrust(ctx, memberID, false)
}

// setTrust updates the peer's transport trust, tolerating a peer this node has
// never seen. A membership event can legitimately name a node that has not yet
// connected, and that is not an error.
func (applier *Applier) setTrust(ctx context.Context, nodeID string, trusted bool) error {
	_, found, err := applier.peers.ByNodeID(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("look up peer: %w", err)
	}
	if !found {
		return nil
	}
	if err := applier.peers.SetTrusted(ctx, nodeID, trusted); err != nil {
		return fmt.Errorf("update peer trust: %w", err)
	}
	return nil
}

// State exposes the underlying policy state for callers that need to read it.
func (applier *Applier) State() *policy.State { return applier.state }
