package events

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/node"
	"jellymesh/internal/policy"
)

// newIdentity generates a throwaway node identity backed by a fresh key pair
// in a temporary directory, so tests never touch a real identity on disk.
func newIdentity(t *testing.T, hostname string) *node.Identity {
	t.Helper()
	dir := t.TempDir()
	identity, err := node.LoadOrCreate(filepath.Join(dir, "node.key"), filepath.Join(dir, "node.crt"), hostname)
	if err != nil {
		t.Fatalf("load or create identity for %s: %v", hostname, err)
	}
	return identity
}

func TestSignVerifyDecodeRoundTripAdmission(t *testing.T) {
	identity := newIdentity(t, "owner.jellymesh.internal")
	admission := policy.Admission{
		GroupID:      "group-1",
		MemberID:     "member-1",
		InvitationID: "invitation-1",
		ApprovalID:   "approval-1",
		Sequence:     1,
		IssuedAt:     time.Unix(10, 0).UTC(),
	}

	envelope, err := Sign(identity, KindAdmission, "group-1", "owner-1", 1, time.Unix(20, 0).UTC(), admission)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if err := envelope.Verify(identity.PublicKey()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	var decoded policy.Admission
	if err := envelope.Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.GroupID != admission.GroupID ||
		decoded.MemberID != admission.MemberID ||
		decoded.InvitationID != admission.InvitationID ||
		decoded.ApprovalID != admission.ApprovalID ||
		decoded.Sequence != admission.Sequence ||
		!decoded.IssuedAt.Equal(admission.IssuedAt) {
		t.Fatalf("decoded admission does not match original: got %+v want %+v", decoded, admission)
	}
}

func TestSignVerifyDecodeRoundTripRevocation(t *testing.T) {
	identity := newIdentity(t, "admin.jellymesh.internal")
	revocation := policy.Revocation{
		GroupID:  "group-1",
		MemberID: "member-9",
		Sequence: 4,
		IssuedAt: time.Unix(42, 0).UTC(),
	}

	envelope, err := Sign(identity, KindRevocation, "group-1", "admin-1", 4, time.Unix(43, 0).UTC(), revocation)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if err := envelope.Verify(identity.PublicKey()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	var decoded policy.Revocation
	if err := envelope.Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.GroupID != revocation.GroupID ||
		decoded.MemberID != revocation.MemberID ||
		decoded.Sequence != revocation.Sequence ||
		!decoded.IssuedAt.Equal(revocation.IssuedAt) {
		t.Fatalf("decoded revocation does not match original: got %+v want %+v", decoded, revocation)
	}
}

// TestMutatingAnySingleFieldInvalidatesTheSignature is the most important
// test in this package: it proves every field the brief requires to be
// signed actually is, by mutating each one independently against an
// otherwise-valid envelope and confirming Verify rejects every case.
func TestMutatingAnySingleFieldInvalidatesTheSignature(t *testing.T) {
	identityA := newIdentity(t, "mutate-a.jellymesh.internal")
	identityB := newIdentity(t, "mutate-b.jellymesh.internal")

	admission := policy.Admission{
		GroupID:      "group-1",
		MemberID:     "member-1",
		InvitationID: "invitation-1",
		ApprovalID:   "approval-1",
		Sequence:     1,
		IssuedAt:     time.Unix(1000, 0).UTC(),
	}

	baseline, err := Sign(identityA, KindAdmission, "group-1", "member-1", 7, time.Unix(2000, 0).UTC(), admission)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := baseline.Verify(identityA.PublicKey()); err != nil {
		t.Fatalf("baseline envelope should verify: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*Envelope)
	}{
		{"kind", func(e *Envelope) { e.Kind = KindRevocation }},
		{"group id", func(e *Envelope) { e.GroupID = "group-2" }},
		{"issuer id", func(e *Envelope) { e.IssuerID = "member-2" }},
		{"issuer fingerprint", func(e *Envelope) { e.Issuer = identityB.Fingerprint() }},
		{"sequence", func(e *Envelope) { e.Sequence = 8 }},
		{"issued at", func(e *Envelope) { e.IssuedAt = e.IssuedAt.Add(time.Second) }},
		{"payload", func(e *Envelope) {
			e.Payload = append(append([]byte{}, e.Payload...), 'x')
		}},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := baseline
			mutation.mutate(&mutated)
			if err := mutated.Verify(identityA.PublicKey()); err == nil {
				t.Fatalf("mutating %s did not invalidate the signature", mutation.name)
			}
		})
	}
}

func TestVerifyWithDifferentNodesPublicKeyFailsWithInvalidSignature(t *testing.T) {
	identityA := newIdentity(t, "different-key-a.jellymesh.internal")
	identityB := newIdentity(t, "different-key-b.jellymesh.internal")

	envelope, err := Sign(identityA, KindRevocation, "group-1", "member-1", 3, time.Now(),
		policy.Revocation{GroupID: "group-1", MemberID: "member-1", Sequence: 3, IssuedAt: time.Now()})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if err := envelope.Verify(identityB.PublicKey()); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

// TestIssuerMismatchWhenSignerLiesAboutItsOwnIdentity constructs an envelope
// by hand (bypassing Sign's guarantee that Issuer always equals the signer's
// own fingerprint) where identity A genuinely signs a message that falsely
// claims to be issued by B. Checked against A's real public key, the raw
// Ed25519 signature is entirely valid, since A really did sign exactly these
// bytes — this is what the brief calls "the signature itself is intact for
// the original key". Verify must still refuse it, because the envelope's
// self-reported Issuer does not match the key that produced the signature.
func TestIssuerMismatchWhenSignerLiesAboutItsOwnIdentity(t *testing.T) {
	identityA := newIdentity(t, "lie-a.jellymesh.internal")
	identityB := newIdentity(t, "lie-b.jellymesh.internal")

	admission := policy.Admission{
		GroupID:      "group-1",
		MemberID:     "member-1",
		InvitationID: "invitation-1",
		ApprovalID:   "approval-1",
		Sequence:     1,
		IssuedAt:     time.Unix(500, 0).UTC(),
	}
	payloadBytes, err := json.Marshal(admission)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	issuedAt := time.Unix(1000, 0).UTC()
	falseIssuer := identityB.Fingerprint() // A signs, but the envelope claims to be B.
	signingBytes := canonicalSigningBytes(KindAdmission, "group-1", "member-1", falseIssuer, 1, issuedAt, payloadBytes)

	privateKey, err := signingKey(identityA)
	if err != nil {
		t.Fatalf("recover signing key: %v", err)
	}
	signature := ed25519.Sign(privateKey, signingBytes)

	envelope := Envelope{
		Kind:      KindAdmission,
		GroupID:   "group-1",
		IssuerID:  "member-1",
		Issuer:    falseIssuer,
		Sequence:  1,
		IssuedAt:  issuedAt,
		Payload:   payloadBytes,
		Signature: signature,
	}

	if err := envelope.Verify(identityA.PublicKey()); !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("expected ErrIssuerMismatch, got %v", err)
	}
}

func TestAdmissionSignatureCannotBePresentedAsRevocation(t *testing.T) {
	identity := newIdentity(t, "relabel.jellymesh.internal")
	admission := policy.Admission{
		GroupID:      "group-1",
		MemberID:     "member-1",
		InvitationID: "invitation-1",
		ApprovalID:   "approval-1",
		Sequence:     1,
		IssuedAt:     time.Now(),
	}

	envelope, err := Sign(identity, KindAdmission, "group-1", "member-1", 1, time.Now(), admission)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	envelope.Kind = KindRevocation
	if err := envelope.Verify(identity.PublicKey()); err == nil {
		t.Fatalf("expected verification to fail once the kind is relabeled as a revocation")
	}
}

func TestMarshalUnmarshalRoundTripStillVerifies(t *testing.T) {
	identity := newIdentity(t, "wire.jellymesh.internal")
	revocation := policy.Revocation{
		GroupID:  "group-1",
		MemberID: "member-9",
		Sequence: 4,
		IssuedAt: time.Unix(99, 500).UTC(),
	}

	original, err := Sign(identity, KindRevocation, "group-1", "owner-1", 4, time.Unix(99, 500).UTC(), revocation)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	data, err := original.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	roundTripped, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := roundTripped.Verify(identity.PublicKey()); err != nil {
		t.Fatalf("round-tripped envelope should still verify: %v", err)
	}

	var decoded policy.Revocation
	if err := roundTripped.Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.GroupID != revocation.GroupID ||
		decoded.MemberID != revocation.MemberID ||
		decoded.Sequence != revocation.Sequence ||
		!decoded.IssuedAt.Equal(revocation.IssuedAt) {
		t.Fatalf("decoded revocation does not match original: got %+v want %+v", decoded, revocation)
	}
}

func TestSequenceGuardRejectsZero(t *testing.T) {
	guard := NewSequenceGuard()
	if err := guard.Admit("group-1", 0); !errors.Is(err, ErrInvalidSequence) {
		t.Fatalf("expected ErrInvalidSequence, got %v", err)
	}
}

func TestSequenceGuardRejectsReplayAndStaleAcceptsMonotonicIncrease(t *testing.T) {
	guard := NewSequenceGuard()
	if err := guard.Admit("group-1", 5); err != nil {
		t.Fatalf("admit 5: %v", err)
	}
	if err := guard.Admit("group-1", 5); !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("expected ErrStaleSequence for an exact replay, got %v", err)
	}
	if err := guard.Admit("group-1", 3); !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("expected ErrStaleSequence for a stale sequence, got %v", err)
	}
	if err := guard.Admit("group-1", 6); err != nil {
		t.Fatalf("admit 6 (monotonic increase): %v", err)
	}
}

func TestSequenceGuardTracksGroupsIndependently(t *testing.T) {
	guard := NewSequenceGuard()
	if err := guard.Admit("group-1", 10); err != nil {
		t.Fatalf("admit group-1: %v", err)
	}
	if err := guard.Admit("group-2", 1); err != nil {
		t.Fatalf("group-2 should be unaffected by group-1's higher sequence: %v", err)
	}
	if err := guard.Admit("group-2", 1); !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("expected ErrStaleSequence for a replay within group-2, got %v", err)
	}
}
