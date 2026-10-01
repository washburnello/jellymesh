package grouplog

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"jellymesh/internal/group"
	"jellymesh/internal/node"
)

var epoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newIdentity(t *testing.T, name string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	identity, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
	if err != nil {
		t.Fatalf("identity %s: %v", name, err)
	}
	return identity
}

// fixture is a group founded by cedar, with walnut and then maple promoted to
// administrator (walnut first, so walnut is the eligible successor), and
// birch and spruce as ordinary members.
type fixture struct {
	log        *Log
	identities map[string]*node.Identity
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{identities: map[string]*node.Identity{}}
	for _, name := range []string{"cedar", "walnut", "maple", "birch", "spruce"} {
		f.identities[name] = newIdentity(t, name)
	}
	log, err := Create(f.identities["cedar"], "group-1", "cedar", "Cedar", "cedar.example.org", epoch)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.log = log
	for _, name := range []string{"walnut", "maple", "birch", "spruce"} {
		f.admit(t, "cedar", name, "invite-"+name)
	}
	f.sequence(t, "cedar", KindPromote, MemberBody{MemberID: "walnut"})
	f.sequence(t, "cedar", KindPromote, MemberBody{MemberID: "maple"})
	return f
}

func (f *fixture) proposal(t *testing.T, proposer string, kind Kind, body any) Proposal {
	t.Helper()
	proposal, err := NewProposal(f.identities[proposer], "group-1", proposer, kind, body, epoch)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	return proposal
}

func (f *fixture) sequence(t *testing.T, proposer string, kind Kind, body any) Event {
	t.Helper()
	event, err := f.log.Sequence(f.identities["cedar"], f.proposal(t, proposer, kind, body), epoch)
	if err != nil {
		t.Fatalf("sequence %s by %s: %v", kind, proposer, err)
	}
	return event
}

func (f *fixture) admit(t *testing.T, approver string, member string, invitation string) Event {
	t.Helper()
	return f.sequence(t, approver, KindAdmission, f.admissionBody(member, invitation))
}

func (f *fixture) admissionBody(member string, invitation string) AdmissionBody {
	return AdmissionBody{
		MemberID:       member,
		MemberKey:      f.identities[member].PublicKey(),
		FriendlyName:   member,
		PublicHostname: member + ".example.org",
		InvitationID:   invitation,
		InviterID:      "cedar",
	}
}

// rawEvent signs an event by hand, bypassing Log.Sequence's own checks, so
// tests can present receivers with events a misbehaving node might send.
func (f *fixture) rawEvent(t *testing.T, signer string, epochNumber uint64, sequence uint64, prev Hash, kind Kind, payload any) Event {
	t.Helper()
	encoded, err := encodeBody(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	event, err := signEvent(f.identities[signer], Event{
		GroupID: "group-1", Epoch: epochNumber, Sequence: sequence, PrevHash: prev,
		Kind: kind, IssuedAt: epoch, SignerID: signer, Payload: encoded,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return event
}

func (f *fixture) next(t *testing.T, signer string, kind Kind, payload any) Event {
	t.Helper()
	head := f.log.Head()
	return f.rawEvent(t, signer, head.Epoch, head.Sequence+1, head.Hash, kind, payload)
}

// follower is a second node holding its own copy of the log.
func (f *fixture) follower(t *testing.T) *Log {
	t.Helper()
	log, err := ReplayAnchored(f.log.EventsAfter(0), f.log.Genesis())
	if err != nil {
		t.Fatalf("follower replay: %v", err)
	}
	return log
}

func (f *fixture) attest(t *testing.T, attestor string, state *State) Attestation {
	t.Helper()
	attestation, err := Attest(f.identities[attestor], attestor, state,
		epoch, epoch.Add(group.OwnerSuccessionTimeout+time.Hour))
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	return attestation
}

// C-PO-20: state is a pure function of the log.
func TestReplayingTheSameLogYieldsIdenticalState(t *testing.T) {
	f := newFixture(t)
	f.sequence(t, "walnut", KindEjection, MemberBody{MemberID: "spruce"})
	f.sequence(t, "birch", KindLeave, MemberBody{MemberID: "birch"})

	first := f.follower(t)
	second, err := Replay(f.log.EventsAfter(0))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !reflect.DeepEqual(first.State(), second.State()) || !reflect.DeepEqual(first.State(), f.log.State()) {
		t.Fatal("replaying the same events produced different state")
	}
	if first.Head() != f.log.Head() {
		t.Fatalf("head %+v, want %+v", first.Head(), f.log.Head())
	}

	state := first.State()
	if state.IsMember("spruce") || !state.IsEjected("spruce") {
		t.Fatal("spruce should be ejected")
	}
	if state.IsMember("birch") || state.IsEjected("birch") {
		t.Fatal("birch left; it is gone but not ejected")
	}
}

// Stored or downloaded logs are re-verified, so a tampered event is refused.
func TestReplayRefusesATamperedEvent(t *testing.T) {
	f := newFixture(t)
	events := f.log.EventsAfter(0)
	events[2].IssuedAt = events[2].IssuedAt.Add(time.Second)
	if _, err := Replay(events); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("error = %v, want ErrInvalidSignature", err)
	}
}

// A joining node anchors the log to the genesis hash from its invitation.
func TestReplayAnchoredRefusesADifferentGroup(t *testing.T) {
	f := newFixture(t)
	other, err := Create(f.identities["walnut"], "group-1", "walnut", "Walnut", "walnut.example.org", epoch)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ReplayAnchored(other.EventsAfter(0), f.log.Genesis()); !errors.Is(err, ErrNotGenesis) {
		t.Fatalf("error = %v, want ErrNotGenesis", err)
	}
}

// C-PO-16: an admission applies from the log alone, on a node that never saw
// the invitation, and binds the member's key.
func TestAdmissionAppliesFromTheLogAloneAndBindsTheKey(t *testing.T) {
	f := newFixture(t)
	f.identities["juniper"] = newIdentity(t, "juniper")
	follower := f.follower(t)

	event := f.admit(t, "walnut", "juniper", "invite-juniper")
	if outcome, err := follower.Append(event); err != nil || outcome != Applied {
		t.Fatalf("follower append: %v, %v", outcome, err)
	}
	member, ok := follower.State().MemberByFingerprint(f.identities["juniper"].Fingerprint())
	if !ok || member.NodeID != "juniper" {
		t.Fatalf("the follower does not bind juniper's key: %+v, %v", member, ok)
	}
	if _, ok := follower.State().MemberByFingerprint(f.identities["cedar"].Fingerprint()); !ok {
		t.Fatal("the owner is a member by fingerprint")
	}
}

// C-TR-7 in the log: a node ID never changes key, and a key never moves to
// another node ID.
func TestAdmissionNeverRebindsAKey(t *testing.T) {
	f := newFixture(t)
	f.sequence(t, "cedar", KindEjection, MemberBody{MemberID: "spruce"})

	rekeyed := f.admissionBody("spruce", "invite-spruce-2")
	rekeyed.MemberKey = newIdentity(t, "spruce-new").PublicKey()
	if _, err := f.log.Sequence(f.identities["cedar"], f.proposal(t, "cedar", KindAdmission, rekeyed), epoch); !errors.Is(err, ErrFingerprintChanged) {
		t.Fatalf("re-keyed readmission: error = %v, want ErrFingerprintChanged", err)
	}

	borrowed := f.admissionBody("spruce", "invite-other")
	borrowed.MemberID = "impostor"
	if _, err := f.log.Sequence(f.identities["cedar"], f.proposal(t, "cedar", KindAdmission, borrowed), epoch); !errors.Is(err, ErrFingerprintInUse) {
		t.Fatalf("key under a new node ID: error = %v, want ErrFingerprintInUse", err)
	}

	// Readmission with the same key and a fresh invitation is allowed.
	f.admit(t, "cedar", "spruce", "invite-spruce-2")
	if !f.log.State().IsMember("spruce") || f.log.State().IsEjected("spruce") {
		t.Fatal("fresh readmission with the original key should succeed")
	}
}

func TestAnInvitationAdmitsOnce(t *testing.T) {
	f := newFixture(t)
	f.identities["juniper"] = newIdentity(t, "juniper")
	body := f.admissionBody("juniper", "invite-birch")
	if _, err := f.log.Sequence(f.identities["cedar"], f.proposal(t, "cedar", KindAdmission, body), epoch); !errors.Is(err, ErrInvitationConsumed) {
		t.Fatalf("error = %v, want ErrInvitationConsumed", err)
	}
}

// C-PO-17: only the owner of the epoch sequences.
func TestOnlyTheOwnerSequences(t *testing.T) {
	f := newFixture(t)
	proposal := f.proposal(t, "walnut", KindEjection, MemberBody{MemberID: "spruce"})

	if _, err := f.log.Sequence(f.identities["walnut"], proposal, epoch); !errors.Is(err, ErrWrongSigner) {
		t.Fatalf("administrator calling Sequence: error = %v, want ErrWrongSigner", err)
	}
	forged := f.next(t, "walnut", KindEjection, proposal)
	if _, err := f.log.Append(forged); !errors.Is(err, ErrWrongSigner) {
		t.Fatalf("administrator-signed event: error = %v, want ErrWrongSigner", err)
	}
	// An event naming the owner as signer but signed with another key.
	impersonated := f.next(t, "walnut", KindEjection, proposal)
	impersonated.SignerID = "cedar"
	if _, err := f.log.Append(impersonated); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("impersonated owner: error = %v, want ErrInvalidSignature", err)
	}
}

// C-PO-17: an event must extend the head by hash as well as by number.
func TestAnEventMustExtendTheHeadByHash(t *testing.T) {
	f := newFixture(t)
	head := f.log.Head()
	proposal := f.proposal(t, "cedar", KindEjection, MemberBody{MemberID: "spruce"})
	wrongParent := f.rawEvent(t, "cedar", head.Epoch, head.Sequence+1, Hash{1}, KindEjection, proposal)
	if _, err := f.log.Append(wrongParent); !errors.Is(err, ErrDoesNotExtend) {
		t.Fatalf("error = %v, want ErrDoesNotExtend", err)
	}
}

// C-PO-17: an event that arrives before the ones it follows is held, not
// dropped, and applies once the gap is filled.
func TestOutOfOrderEventsAreHeldAndApplied(t *testing.T) {
	f := newFixture(t)
	follower := f.follower(t)
	first := f.sequence(t, "walnut", KindEjection, MemberBody{MemberID: "spruce"})
	second := f.sequence(t, "cedar", KindDemote, MemberBody{MemberID: "maple"})

	if _, err := follower.Append(second); !errors.Is(err, ErrGap) {
		t.Fatalf("Append ahead of head: error = %v, want ErrGap", err)
	}
	if outcome, err := follower.Offer(second); err != nil || outcome != Held {
		t.Fatalf("Offer ahead of head: %v, %v", outcome, err)
	}
	if outcome, err := follower.Offer(first); err != nil || outcome != Applied {
		t.Fatalf("Offer filling the gap: %v, %v", outcome, err)
	}
	if follower.Head() != f.log.Head() || follower.Pending() != 0 {
		t.Fatalf("follower head %+v with %d pending, want %+v", follower.Head(), follower.Pending(), f.log.Head())
	}
}

func TestReceivingAnEventTwiceIsHarmless(t *testing.T) {
	f := newFixture(t)
	follower := f.follower(t)
	event := f.sequence(t, "walnut", KindEjection, MemberBody{MemberID: "spruce"})
	follower.Append(event)
	if outcome, err := follower.Append(event); err != nil || outcome != Duplicate {
		t.Fatalf("second append: %v, %v", outcome, err)
	}
}

// C-PO-18: an administrator's decision needs that administrator's signature,
// and receivers re-apply the role rules even to an owner-sequenced event.
func TestTheOwnerCannotFabricateAnAdministratorsDecision(t *testing.T) {
	f := newFixture(t)
	// A proposal naming walnut as proposer, signed with the owner's key.
	fabricated, err := NewProposal(f.identities["cedar"], "group-1", "walnut", KindEjection, MemberBody{MemberID: "spruce"}, epoch)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	if _, err := f.log.Append(f.next(t, "cedar", KindEjection, fabricated)); !errors.Is(err, ErrInvalidProposalSignature) {
		t.Fatalf("error = %v, want ErrInvalidProposalSignature", err)
	}
}

func TestReceiversReapplyTheRoleRules(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name     string
		proposer string
		kind     Kind
		body     any
		want     error
	}{
		{"administrator ejects administrator", "walnut", KindEjection, MemberBody{MemberID: "maple"}, ErrAdminProtected},
		{"administrator ejects owner", "walnut", KindEjection, MemberBody{MemberID: "cedar"}, ErrOwnerProtected},
		{"member ejects member", "birch", KindEjection, MemberBody{MemberID: "spruce"}, ErrNotAuthorized},
		{"member approves admission", "birch", KindAdmission, AdmissionBody{MemberID: "x", MemberKey: newIdentity(t, "x").PublicKey(), InvitationID: "i"}, ErrNotAuthorized},
		{"administrator promotes", "walnut", KindPromote, MemberBody{MemberID: "birch"}, ErrNotAuthorized},
		{"member leaves on behalf of another", "birch", KindLeave, MemberBody{MemberID: "spruce"}, ErrNotAuthorized},
		{"owner leaves", "cedar", KindLeave, MemberBody{MemberID: "cedar"}, ErrOwnerProtected},
		{"demote a non-administrator", "cedar", KindDemote, MemberBody{MemberID: "birch"}, ErrNotAdministrator},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Signed by the owner's key as sequencer, bypassing Sequence's
			// own checks: this is what a misbehaving owner would send.
			event := f.next(t, "cedar", tc.kind, f.proposal(t, tc.proposer, tc.kind, tc.body))
			before := f.log.Head()
			if _, err := f.log.Append(event); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if f.log.Head() != before {
				t.Fatal("a refused event changed the log")
			}
		})
	}
	// The owner may remove an administrator.
	f.sequence(t, "cedar", KindEjection, MemberBody{MemberID: "maple"})
	if f.log.State().IsMember("maple") {
		t.Fatal("the owner's ejection of an administrator should apply")
	}
}

func TestAProposalAppliesOnce(t *testing.T) {
	f := newFixture(t)
	proposal := f.proposal(t, "cedar", KindPromote, MemberBody{MemberID: "birch"})
	if _, err := f.log.Sequence(f.identities["cedar"], proposal, epoch); err != nil {
		t.Fatalf("first: %v", err)
	}
	f.sequence(t, "cedar", KindDemote, MemberBody{MemberID: "birch"})
	if _, err := f.log.Sequence(f.identities["cedar"], proposal, epoch); !errors.Is(err, ErrProposalReplayed) {
		t.Fatalf("replayed proposal: error = %v, want ErrProposalReplayed", err)
	}
}

// C-PO-19: two correctly signed events for one slot halt the log and are kept.
func TestEquivocationHaltsTheLogAndKeepsEvidence(t *testing.T) {
	f := newFixture(t)
	follower := f.follower(t)
	first := f.next(t, "cedar", KindEjection, f.proposal(t, "cedar", KindEjection, MemberBody{MemberID: "spruce"}))
	second := f.next(t, "cedar", KindEjection, f.proposal(t, "cedar", KindEjection, MemberBody{MemberID: "birch"}))

	if _, err := follower.Append(first); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := follower.Append(second); !errors.Is(err, ErrEquivocation) {
		t.Fatalf("second: error = %v, want ErrEquivocation", err)
	}
	evidence, halted := follower.Halted()
	if !halted || evidence.Existing.Hash() != first.Hash() || evidence.Received.Hash() != second.Hash() {
		t.Fatal("the log should halt and keep both events")
	}
	if _, err := follower.Append(f.next(t, "cedar", KindDemote, f.proposal(t, "cedar", KindDemote, MemberBody{MemberID: "maple"}))); !errors.Is(err, ErrHalted) {
		t.Fatalf("after halt: error = %v, want ErrHalted", err)
	}
}

// A conflicting event that is not validly signed is just invalid. It must not
// let a non-owner halt the log.
func TestAForgedConflictDoesNotHaltTheLog(t *testing.T) {
	f := newFixture(t)
	follower := f.follower(t)
	head := f.log.Head()
	legitimate := f.sequence(t, "cedar", KindEjection, MemberBody{MemberID: "spruce"})
	follower.Append(legitimate)

	forged := f.rawEvent(t, "walnut", head.Epoch, head.Sequence+1, head.Hash, KindEjection,
		f.proposal(t, "walnut", KindEjection, MemberBody{MemberID: "birch"}))
	forged.SignerID = "cedar"
	if _, err := follower.Append(forged); errors.Is(err, ErrEquivocation) || err == nil {
		t.Fatalf("forged conflict: error = %v, want a verification failure", err)
	}
	fakeGenesis, _ := Create(f.identities["birch"], "group-1", "birch", "", "", epoch)
	if _, err := follower.Append(fakeGenesis.EventsAfter(0)[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("fake genesis: error = %v, want ErrConflict", err)
	}
	if _, halted := follower.Halted(); halted {
		t.Fatal("a non-owner must not be able to halt the log")
	}
}

// C-PO-14: attestations are signed by their attestors, and a claim needs a
// quorum of them.
func TestSuccessionRequiresSignedAttestationsFromAQuorum(t *testing.T) {
	f := newFixture(t)
	state := f.log.State()
	// Five members: four non-owner voices, majority three, so the claimant
	// needs two attestations besides its own.
	if required := group.RequiredAbsenceAttestations(len(state.Members())); required != 2 {
		t.Fatalf("required = %d, want 2", required)
	}
	if successor, _ := state.EligibleSuccessor(); successor != "walnut" {
		t.Fatalf("eligible successor = %q, want walnut", successor)
	}

	// Written by the claimant in other members' names.
	forged := []Attestation{f.attest(t, "walnut", state), f.attest(t, "walnut", state)}
	forged[0].AttestorID, forged[1].AttestorID = "birch", "spruce"
	if _, err := f.log.Claim(f.identities["walnut"], "walnut", forged, epoch); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("attestations written by the claimant: error = %v, want ErrInvalidAttestation", err)
	}

	tampered := f.attest(t, "birch", state)
	tampered.UnreachableSince = tampered.UnreachableSince.Add(-time.Hour)
	if _, err := f.log.Claim(f.identities["walnut"], "walnut", []Attestation{tampered, f.attest(t, "spruce", state)}, epoch); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("tampered attestation: error = %v, want ErrInvalidAttestation", err)
	}

	tooRecent, err := Attest(f.identities["birch"], "birch", state, epoch, epoch.Add(time.Hour))
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if _, err := f.log.Claim(f.identities["walnut"], "walnut", []Attestation{tooRecent, f.attest(t, "spruce", state)}, epoch); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("attestation inside the window: error = %v, want ErrInvalidAttestation", err)
	}

	if _, err := f.log.Claim(f.identities["walnut"], "walnut", []Attestation{f.attest(t, "birch", state)}, epoch); !errors.Is(err, ErrInsufficientAttestations) {
		t.Fatalf("one attestation: error = %v, want ErrInsufficientAttestations", err)
	}
	duplicate := f.attest(t, "birch", state)
	if _, err := f.log.Claim(f.identities["walnut"], "walnut", []Attestation{duplicate, duplicate}, epoch); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("duplicate attestor: error = %v, want ErrInvalidAttestation", err)
	}

	if _, err := f.log.Claim(f.identities["maple"], "maple", []Attestation{f.attest(t, "birch", state), f.attest(t, "spruce", state)}, epoch); !errors.Is(err, ErrNotSuccessor) {
		t.Fatalf("junior administrator claiming: error = %v, want ErrNotSuccessor", err)
	}

	if _, err := f.log.Claim(f.identities["walnut"], "walnut", []Attestation{f.attest(t, "birch", state), f.attest(t, "spruce", state)}, epoch); err != nil {
		t.Fatalf("valid claim: %v", err)
	}
	if !f.log.State().IsOwner("walnut") || f.log.State().Epoch != 2 {
		t.Fatalf("walnut should own epoch 2; owner %q epoch %d", f.log.State().OwnerID, f.log.State().Epoch)
	}
	if !f.log.State().IsMember("cedar") || f.log.State().IsAdministrator("cedar") {
		t.Fatal("the former owner continues as an ordinary member")
	}
}

// A claim may not discard events an attestor had already applied.
func TestAClaimMustContainEveryAttestedHead(t *testing.T) {
	f := newFixture(t)
	claimant := f.follower(t)
	f.sequence(t, "cedar", KindEjection, MemberBody{MemberID: "spruce"})
	aheadOfClaimant := f.log.State()

	attestations := []Attestation{f.attest(t, "birch", aheadOfClaimant), f.attest(t, "maple", aheadOfClaimant)}
	if _, err := claimant.Claim(f.identities["walnut"], "walnut", attestations, epoch); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("claim behind an attested head: error = %v, want ErrInvalidAttestation", err)
	}
}

// C-PO-15: after succession, the former owner is fenced, and a member that
// applied the former owner's later events truncates and follows the claim.
func TestSuccessionFencesTheFormerOwner(t *testing.T) {
	f := newFixture(t)
	minority := f.follower(t)
	base := f.log.State()
	attestations := []Attestation{f.attest(t, "birch", base), f.attest(t, "spruce", base)}

	// Partitioned from the majority, the old owner keeps sequencing, and one
	// member still reaches it.
	stale := f.next(t, "cedar", KindEjection, f.proposal(t, "cedar", KindEjection, MemberBody{MemberID: "maple"}))
	if _, err := minority.Append(stale); err != nil {
		t.Fatalf("minority applies the stale event: %v", err)
	}

	claim, err := f.log.Claim(f.identities["walnut"], "walnut", attestations, epoch)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The majority refuses the old owner's event for the same slot.
	if _, err := f.log.Append(stale); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale event after claim: error = %v, want ErrFenced", err)
	}
	// And any later old-epoch event.
	later := f.rawEvent(t, "cedar", 1, claim.Sequence+1, claim.Hash(), KindDemote,
		f.proposal(t, "cedar", KindDemote, MemberBody{MemberID: "maple"}))
	if _, err := f.log.Append(later); !errors.Is(err, ErrFenced) {
		t.Fatalf("old-epoch event past the claim: error = %v, want ErrFenced", err)
	}

	// The minority receives the claim, truncates, and converges.
	outcome, err := minority.Append(claim)
	if err != nil || outcome != Superseded {
		t.Fatalf("minority receives claim: %v, %v", outcome, err)
	}
	if minority.Head() != f.log.Head() || !reflect.DeepEqual(minority.State(), f.log.State()) {
		t.Fatal("the minority should converge on the majority's log")
	}
	if !minority.State().IsMember("maple") {
		t.Fatal("the superseded ejection must be undone")
	}
	if superseded := minority.Superseded(); len(superseded) != 1 || superseded[0].Hash() != stale.Hash() {
		t.Fatal("the superseded event should be kept so its proposal can be re-proposed")
	}

	// The new owner sequences normally in the new epoch.
	proposal, _ := NewProposal(f.identities["walnut"], "group-1", "walnut", KindEjection, MemberBody{MemberID: "spruce"}, epoch)
	if _, err := f.log.Sequence(f.identities["walnut"], proposal, epoch); err != nil {
		t.Fatalf("new owner sequences: %v", err)
	}
	if _, err := f.log.Sequence(f.identities["cedar"], f.proposal(t, "cedar", KindLeave, MemberBody{MemberID: "cedar"}), epoch); !errors.Is(err, ErrWrongSigner) {
		t.Fatalf("former owner sequencing: error = %v, want ErrWrongSigner", err)
	}
}

func TestEventsSurviveTheWireEncoding(t *testing.T) {
	f := newFixture(t)
	var decoded []Event
	for _, event := range f.log.EventsAfter(0) {
		data, err := event.Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		back, err := UnmarshalEvent(data)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		decoded = append(decoded, back)
	}
	log, err := ReplayAnchored(decoded, f.log.Genesis())
	if err != nil {
		t.Fatalf("replay decoded: %v", err)
	}
	if log.Head() != f.log.Head() {
		t.Fatal("wire round trip changed the log")
	}
}

// Every field of an event, proposal, and attestation is covered by its
// signature, so none can be altered in transit.
func TestEverySignedFieldIsCovered(t *testing.T) {
	f := newFixture(t)
	cedarKey := f.identities["cedar"].PublicKey()
	event := f.log.EventsAfter(0)[1]
	eventMutations := map[string]func(*Event){
		"group":     func(e *Event) { e.GroupID = "group-2" },
		"epoch":     func(e *Event) { e.Epoch++ },
		"sequence":  func(e *Event) { e.Sequence++ },
		"prev hash": func(e *Event) { e.PrevHash[0] ^= 1 },
		"kind":      func(e *Event) { e.Kind = KindEjection },
		"issued at": func(e *Event) { e.IssuedAt = e.IssuedAt.Add(time.Nanosecond) },
		"signer":    func(e *Event) { e.SignerID = "walnut" },
		"payload":   func(e *Event) { e.Payload = append([]byte(nil), e.Payload...); e.Payload[0] ^= 1 },
	}
	if err := event.verify(cedarKey); err != nil {
		t.Fatalf("unaltered event: %v", err)
	}
	for name, mutate := range eventMutations {
		altered := event
		mutate(&altered)
		if altered.verify(cedarKey) == nil {
			t.Errorf("event %s is not covered by the signature", name)
		}
	}

	proposal := f.proposal(t, "cedar", KindEjection, MemberBody{MemberID: "spruce"})
	proposalMutations := map[string]func(*Proposal){
		"id":        func(p *Proposal) { p.ID += "x" },
		"group":     func(p *Proposal) { p.GroupID = "group-2" },
		"kind":      func(p *Proposal) { p.Kind = KindLeave },
		"proposer":  func(p *Proposal) { p.ProposerID = "walnut" },
		"issued at": func(p *Proposal) { p.IssuedAt = p.IssuedAt.Add(time.Nanosecond) },
		"body":      func(p *Proposal) { p.Body = []byte(`{"member_id":"birch"}`) },
	}
	for name, mutate := range proposalMutations {
		altered := proposal
		mutate(&altered)
		if altered.verify(cedarKey) == nil {
			t.Errorf("proposal %s is not covered by the signature", name)
		}
	}

	attestation := f.attest(t, "birch", f.log.State())
	birchKey := f.identities["birch"].PublicKey()
	attestationMutations := map[string]func(*Attestation){
		"group":             func(a *Attestation) { a.GroupID = "group-2" },
		"epoch":             func(a *Attestation) { a.Epoch++ },
		"absent owner":      func(a *Attestation) { a.AbsentOwnerID = "walnut" },
		"attestor":          func(a *Attestation) { a.AttestorID = "spruce" },
		"unreachable since": func(a *Attestation) { a.UnreachableSince = a.UnreachableSince.Add(-time.Nanosecond) },
		"issued at":         func(a *Attestation) { a.IssuedAt = a.IssuedAt.Add(time.Nanosecond) },
		"head epoch":        func(a *Attestation) { a.Head.Epoch++ },
		"head sequence":     func(a *Attestation) { a.Head.Sequence-- },
		"head hash":         func(a *Attestation) { a.Head.Hash[0] ^= 1 },
	}
	for name, mutate := range attestationMutations {
		altered := attestation
		mutate(&altered)
		if altered.verify(birchKey) == nil {
			t.Errorf("attestation %s is not covered by the signature", name)
		}
	}
}

// C-PO-9: events, proposals, and attestations are signed under distinct
// domain tags, so a signature made for one can never verify as another, and
// none can collide with the node key's other use in mutual TLS.
func TestSignaturesAreDomainSeparated(t *testing.T) {
	f := newFixture(t)
	event := f.log.EventsAfter(0)[1]
	proposal := f.proposal(t, "cedar", KindEjection, MemberBody{MemberID: "spruce"})
	attestation := f.attest(t, "cedar", f.log.State())

	domains := map[string][]byte{
		eventDomain:       event.signingBytes(),
		proposalDomain:    proposal.signingBytes(),
		attestationDomain: attestation.signingBytes(),
	}
	if len(domains) != 3 {
		t.Fatal("the three domain tags must differ")
	}
	for domain, signed := range domains {
		if string(signed[:len(domain)]) != domain {
			t.Fatalf("signed bytes do not begin with their domain tag %q", domain)
		}
	}
	// A proposal's signature presented as an event's does not verify.
	forged := event
	forged.Signature = proposal.Signature
	if forged.verify(f.identities["cedar"].PublicKey()) == nil {
		t.Fatal("a proposal signature must not verify as an event signature")
	}
}

// C-NT-8: a member changes its own advertised address, and every node that
// replays the log dials the new one; no one can change another member's, an
// address must be a plain host and port, and a non-member cannot propose.
func TestAMemberChangesOnlyItsOwnAddress(t *testing.T) {
	f := newFixture(t)
	event := f.sequence(t, "birch", KindAddress, AddressBody{MemberID: "birch", PublicHostname: "birch.example.org:10000"})
	if member, _ := f.log.State().Member("birch"); member.PublicHostname != "birch.example.org:10000" {
		t.Fatalf("birch's address: %q", member.PublicHostname)
	}
	follower := f.follower(t)
	if member, _ := follower.State().Member("birch"); member.PublicHostname != "birch.example.org:10000" || follower.Head().Hash != event.Hash() {
		t.Fatalf("a follower's view of birch: %q", member.PublicHostname)
	}
	f.sequence(t, "cedar", KindAddress, AddressBody{MemberID: "cedar", PublicHostname: "cedar.example.ts.net:10000"})
	if member, _ := f.log.State().Member("cedar"); member.PublicHostname != "cedar.example.ts.net:10000" {
		t.Fatalf("the owner's own address: %q", member.PublicHostname)
	}

	for _, tc := range []struct {
		name     string
		proposer string
		body     AddressBody
		want     error
	}{
		{"for another member", "birch", AddressBody{MemberID: "spruce", PublicHostname: "evil.example:8443"}, ErrNotAuthorized},
		{"by the owner for a member", "cedar", AddressBody{MemberID: "spruce", PublicHostname: "evil.example:8443"}, ErrNotAuthorized},
		{"no port", "birch", AddressBody{MemberID: "birch", PublicHostname: "birch.example.org"}, ErrInvalidAddress},
		{"a URL", "birch", AddressBody{MemberID: "birch", PublicHostname: "https://birch.example.org:8443/x"}, ErrInvalidAddress},
		{"port zero", "birch", AddressBody{MemberID: "birch", PublicHostname: "birch.example.org:0"}, ErrInvalidAddress},
		{"empty", "birch", AddressBody{MemberID: "birch"}, ErrInvalidAddress},
		{"a non-member", "birch", AddressBody{MemberID: "nobody", PublicHostname: "x.example:1"}, ErrNotMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := f.next(t, "cedar", KindAddress, f.proposal(t, tc.proposer, KindAddress, tc.body))
			before := f.log.Head()
			if _, err := f.log.Append(event); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if f.log.Head() != before {
				t.Fatal("a refused address change changed the log")
			}
		})
	}
}
