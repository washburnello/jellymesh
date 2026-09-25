package group

import (
	"errors"
	"testing"
	"time"
)

func TestOwnerPromotesAndAdminsHaveLimitedPower(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "maple")
	if err := state.PromoteAdmin("cedar", "maple"); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	if !state.IsAdministrator("maple") {
		t.Fatal("maple should be an administrator")
	}
	if err := state.Eject("maple", "cedar"); !errors.Is(err, ErrOwnerProtected) {
		t.Fatalf("unexpected owner-protection error: %v", err)
	}
	admitTestMember(t, state, "walnut")
	if err := state.PromoteAdmin("cedar", "walnut"); err != nil {
		t.Fatalf("promote second admin: %v", err)
	}
	if err := state.Eject("maple", "walnut"); !errors.Is(err, ErrAdminProtected) {
		t.Fatalf("unexpected admin-protection error: %v", err)
	}
	if err := state.Eject("maple", "walnut"); !errors.Is(err, ErrAdminProtected) {
		t.Fatalf("unexpected repeated admin-protection error: %v", err)
	}
}

func TestOwnerSuccessionComesFromAdminPool(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "maple")
	admitTestMember(t, state, "walnut")
	if err := state.PromoteAdminAt("cedar", "maple", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("promote maple: %v", err)
	}
	if err := state.PromoteAdminAt("cedar", "walnut", time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("promote walnut: %v", err)
	}
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := state.MarkOwnerUnavailable(start); err != nil {
		t.Fatalf("mark owner unavailable: %v", err)
	}
	deadline := start.Add(15 * 24 * time.Hour)
	decision := state.Advance(deadline)
	if decision.SuccessionEligible != "maple" || decision.Dissolve {
		t.Fatalf("unexpected succession decision: %#v", decision)
	}
	// Eligibility alone must not move ownership. Succession is applied only by
	// an explicit claim backed by a quorum of attestations.
	if state.OwnerID != "cedar" {
		t.Fatalf("Advance must not promote on a local timer, owner is now %s", state.OwnerID)
	}
	if err := state.ClaimOwnership("maple", attestations(state, "maple", start, "walnut"), deadline); err != nil {
		t.Fatalf("claim ownership: %v", err)
	}
	if state.OwnerID != "maple" {
		t.Fatalf("oldest administrator did not become owner: %s", state.OwnerID)
	}
	if state.Status != StatusActive {
		t.Fatalf("unexpected status after succession: %s", state.Status)
	}
}

func TestSuccessionRequiresAQuorumOfAttestations(t *testing.T) {
	state, start, deadline := successionFixture(t)
	// maple is the oldest admin and the eligible successor. Members are cedar
	// (absent owner), maple, walnut, birch, alder: four others, quorum three.
	// Five members: cedar (absent owner), maple (claimant), walnut, birch,
	// alder. Majority of the four non-owner members is three, and the
	// claimant supplies one voice, so two attestations are required.
	if got := RequiredAbsenceAttestations(len(state.Members)); got != 2 {
		t.Fatalf("unexpected requirement for %d members: %d", len(state.Members), got)
	}
	if err := state.ClaimOwnership("maple", attestations(state, "maple", start, "walnut"), deadline); !errors.Is(err, ErrInsufficientAttestation) {
		t.Fatalf("one attestation should not satisfy a requirement of two: %v", err)
	}
	if state.OwnerID != "cedar" {
		t.Fatal("a failed claim must not move ownership")
	}
	if err := state.ClaimOwnership("maple", attestations(state, "maple", start, "walnut", "birch", "alder"), deadline); err != nil {
		t.Fatalf("a full quorum should succeed: %v", err)
	}
	if state.OwnerID != "maple" {
		t.Fatalf("owner did not change: %s", state.OwnerID)
	}
}

// A partitioned or malicious node must not be able to manufacture its own
// quorum. This is the split-brain case the timer-driven design allowed.
func TestSuccessionRejectsManufacturedQuorum(t *testing.T) {
	state, start, deadline := successionFixture(t)
	selfSigned := attestations(state, "maple", start, "walnut", "birch")
	selfSigned = append(selfSigned, AbsenceAttestation{
		GroupID: state.GroupID, AbsentOwnerID: "cedar",
		AttestorID: "maple", UnreachableSince: start,
	})
	if err := state.ClaimOwnership("maple", selfSigned, deadline); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("a claimant must not attest for itself: %v", err)
	}
	duplicated := attestations(state, "maple", start, "walnut", "birch")
	duplicated = append(duplicated, duplicated[0])
	if err := state.ClaimOwnership("maple", duplicated, deadline); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("duplicate attestors must be rejected: %v", err)
	}
	outsider := attestations(state, "maple", start, "walnut", "birch")
	outsider = append(outsider, AbsenceAttestation{
		GroupID: state.GroupID, AbsentOwnerID: "cedar",
		AttestorID: "stranger", UnreachableSince: start,
	})
	if err := state.ClaimOwnership("maple", outsider, deadline); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("a non-member must not attest: %v", err)
	}
	if state.OwnerID != "cedar" {
		t.Fatal("no rejected claim may move ownership")
	}
}

func TestSuccessionRejectsRecentObservations(t *testing.T) {
	state, start, deadline := successionFixture(t)
	recent := attestations(state, "maple", start.Add(14*24*time.Hour), "walnut", "birch", "alder")
	if err := state.ClaimOwnership("maple", recent, deadline); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("attestations that only noticed the owner recently must not support the claim: %v", err)
	}
}

func TestOnlyTheEligibleSuccessorMayClaim(t *testing.T) {
	state, start, deadline := successionFixture(t)
	if err := state.ClaimOwnership("walnut", attestations(state, "walnut", start, "maple", "birch", "alder"), deadline); !errors.Is(err, ErrNotSuccessor) {
		t.Fatalf("a younger administrator must not claim ahead of the oldest: %v", err)
	}
}

func TestClaimBeforeTheDeadlineIsRejected(t *testing.T) {
	state, start, _ := successionFixture(t)
	early := start.Add(14 * 24 * time.Hour)
	if err := state.ClaimOwnership("maple", attestations(state, "maple", start, "walnut", "birch", "alder"), early); !errors.Is(err, ErrSuccessionNotDue) {
		t.Fatalf("a claim before the absence window elapses must be rejected: %v", err)
	}
}

func TestRequiredAbsenceAttestationSizes(t *testing.T) {
	for members, want := range map[int]int{1: 0, 2: 0, 3: 1, 5: 2, 20: 9} {
		if got := RequiredAbsenceAttestations(members); got != want {
			t.Fatalf("RequiredAbsenceAttestations(%d) = %d, want %d", members, got, want)
		}
	}
}

// successionFixture builds a five-member group whose owner has been absent past
// the deadline, with maple as the oldest administrator.
func successionFixture(t *testing.T) (*State, time.Time, time.Time) {
	t.Helper()
	state := newTestState(t)
	for _, id := range []string{"maple", "walnut", "birch", "alder"} {
		admitTestMember(t, state, id)
	}
	if err := state.PromoteAdminAt("cedar", "maple", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("promote maple: %v", err)
	}
	if err := state.PromoteAdminAt("cedar", "walnut", time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("promote walnut: %v", err)
	}
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := state.MarkOwnerUnavailable(start); err != nil {
		t.Fatalf("mark owner unavailable: %v", err)
	}
	return state, start, start.Add(15 * 24 * time.Hour)
}

func attestations(state *State, claimant string, since time.Time, attestors ...string) []AbsenceAttestation {
	out := make([]AbsenceAttestation, 0, len(attestors))
	for _, id := range attestors {
		out = append(out, AbsenceAttestation{
			GroupID:          state.GroupID,
			AbsentOwnerID:    state.OwnerID,
			AttestorID:       id,
			UnreachableSince: since,
			IssuedAt:         since,
		})
	}
	return out
}

func TestGroupDissolvesWithoutOwnerOrAdmins(t *testing.T) {
	state := newTestState(t)
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := state.MarkOwnerUnavailable(start); err != nil {
		t.Fatalf("mark owner unavailable: %v", err)
	}
	if state.Status != StatusDissolutionPending {
		t.Fatalf("unexpected status: %s", state.Status)
	}
	decision := state.Advance(start.Add(15 * 24 * time.Hour))
	if !decision.Dissolve || state.Status != StatusDissolved {
		t.Fatalf("group did not dissolve: %#v, %s", decision, state.Status)
	}
}

func newTestState(t *testing.T) *State {
	t.Helper()
	state, err := NewState("group-1", "cedar")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	return state
}

func admitTestMember(t *testing.T, state *State, memberID string) {
	t.Helper()
	state.Members[memberID] = true
}
