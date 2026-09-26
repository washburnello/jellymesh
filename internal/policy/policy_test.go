package policy

import (
	"errors"
	"testing"
	"time"

	"jellymesh/internal/group"
)

func TestPublishedLibraryIsAutoAccepted(t *testing.T) {
	state := newTestState(t)
	publishTestLibrary(t, state, "cedar", "movies")

	if !state.CanConsume("cedar", "movies") {
		t.Fatal("published library should be auto-accepted")
	}
	if state.IsOptedOut("cedar", "movies") {
		t.Fatal("published library should not be opted out by default")
	}
}

func TestDestinationCanOptOutAndOptBackIn(t *testing.T) {
	state := newTestState(t)
	publishTestLibrary(t, state, "cedar", "movies")
	if err := state.SetOptOut("cedar", "movies", true); err != nil {
		t.Fatalf("set opt-out: %v", err)
	}
	if state.CanConsume("cedar", "movies") {
		t.Fatal("opted-out library must not be consumable")
	}
	if err := state.SetOptOut("cedar", "movies", false); err != nil {
		t.Fatalf("clear opt-out: %v", err)
	}
	if !state.CanConsume("cedar", "movies") {
		t.Fatal("clearing an opt-out should restore pool behavior")
	}
}

func TestOptOutRequiresPublishedLibrary(t *testing.T) {
	state := newTestState(t)
	if err := state.SetOptOut("cedar", "movies", true); !errors.Is(err, ErrPublicationRequired) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBlockOverridesPublishedLibrary(t *testing.T) {
	state := newTestState(t)
	publishTestLibrary(t, state, "cedar", "movies")
	if err := state.BlockPeer("cedar"); err != nil {
		t.Fatalf("block peer: %v", err)
	}
	if state.CanConsume("cedar", "movies") {
		t.Fatal("blocked peer must not be consumable")
	}
	if !state.IsPeerBlocked("cedar") {
		t.Fatal("peer should be blocked")
	}
}

func TestUnblockPreservesOptOut(t *testing.T) {
	state := newTestState(t)
	publishTestLibrary(t, state, "cedar", "movies")
	if err := state.SetOptOut("cedar", "movies", true); err != nil {
		t.Fatalf("set opt-out: %v", err)
	}
	if err := state.BlockPeer("cedar"); err != nil {
		t.Fatalf("block peer: %v", err)
	}
	if err := state.UnblockPeer("cedar"); err != nil {
		t.Fatalf("unblock peer: %v", err)
	}
	if state.CanConsume("cedar", "movies") {
		t.Fatal("unblock must not override an existing opt-out")
	}
}

func TestUnpublishRemovesAvailabilityButRetainsOptOut(t *testing.T) {
	state := newTestState(t)
	publishTestLibrary(t, state, "cedar", "movies")
	if err := state.SetOptOut("cedar", "movies", true); err != nil {
		t.Fatalf("set opt-out: %v", err)
	}
	if err := state.Unpublish("cedar", "movies"); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if state.CanConsume("cedar", "movies") {
		t.Fatal("unpublished library must not be consumable")
	}
	publishTestLibrary(t, state, "cedar", "movies")
	if !state.IsOptedOut("cedar", "movies") {
		t.Fatal("opt-out should survive republication")
	}
}

func TestLibraryStatus(t *testing.T) {
	state := newTestState(t)
	publishTestLibrary(t, state, "cedar", "movies")
	if status := state.LibraryStatus("cedar", "movies"); status != "movies:available" {
		t.Fatalf("unexpected status: %s", status)
	}
	if err := state.SetOptOut("cedar", "movies", true); err != nil {
		t.Fatalf("set opt-out: %v", err)
	}
	if status := state.LibraryStatus("cedar", "movies"); status != "movies:opted-out" {
		t.Fatalf("unexpected status: %s", status)
	}
}

// --- roles ------------------------------------------------------------------

func TestCreatorIsOwnerAndOwnerIsAdministrator(t *testing.T) {
	state := newTestState(t)
	if !state.IsOwner("cedar") {
		t.Fatal("the group creator should be the owner")
	}
	if !state.IsAdministrator("cedar") {
		t.Fatal("the owner should hold administrator privileges")
	}
	if !state.IsMember("cedar") {
		t.Fatal("the owner should be a member")
	}
}

func TestAnyMemberCanInviteButOnlyAdminsApprove(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "walnut")
	if err := state.CreateInvitation("walnut", "invite-1", "hash-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("member invitation: %v", err)
	}
	if err := state.RedeemInvitation("invite-1", "new-member", "fingerprint-1"); err != nil {
		t.Fatalf("redeem invitation: %v", err)
	}
	pending := state.PendingApprovals()
	if len(pending) != 1 || pending[0].InviterID != "walnut" || pending[0].InviteeID != "new-member" {
		t.Fatalf("unexpected pending approvals: %#v", pending)
	}
	// An ordinary member cannot approve.
	if err := state.ApproveInvitation("walnut", "invite-1", "approval-1"); !errors.Is(err, ErrNotAdministrator) {
		t.Fatalf("unexpected non-admin approval error: %v", err)
	}
	if err := state.ApproveInvitation("cedar", "invite-1", "approval-1"); err != nil {
		t.Fatalf("owner approval: %v", err)
	}
}

// A promoted administrator must be able to approve invitations. The previous
// coordinator model allowed only one node to do this, which contradicted the
// specification.
func TestPromotedAdministratorCanApproveInvitations(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "walnut")
	if err := state.Roles.PromoteAdmin("cedar", "walnut"); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	if err := state.CreateInvitation("cedar", "invite-2", "hash-2", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	if err := state.RedeemInvitation("invite-2", "maple", "fingerprint-maple"); err != nil {
		t.Fatalf("redeem invitation: %v", err)
	}
	if err := state.ApproveInvitation("walnut", "invite-2", "approval-2"); err != nil {
		t.Fatalf("administrator approval should be permitted: %v", err)
	}
}

func TestAdministratorCanDenyInvitation(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "walnut")
	if err := state.CreateInvitation("cedar", "invite-1", "hash-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	if err := state.RedeemInvitation("invite-1", "new-member", "fingerprint-1"); err != nil {
		t.Fatalf("redeem invitation: %v", err)
	}
	if err := state.DenyInvitation("walnut", "invite-1"); !errors.Is(err, ErrNotAdministrator) {
		t.Fatalf("unexpected non-admin denial error: %v", err)
	}
	if err := state.DenyInvitation("cedar", "invite-1"); err != nil {
		t.Fatalf("owner denial: %v", err)
	}
	invitation, ok := state.InvitationByID("invite-1")
	if !ok || invitation.Status != InvitationDenied {
		t.Fatalf("unexpected invitation status: %#v", invitation)
	}
}

func TestInvitationCannotBeReused(t *testing.T) {
	state := newTestState(t)
	if err := state.CreateInvitation("cedar", "invite-1", "hash-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	if err := state.RedeemInvitation("invite-1", "new-member", "fingerprint-1"); err != nil {
		t.Fatalf("redeem invitation: %v", err)
	}
	if err := state.RedeemInvitation("invite-1", "other-member", "fingerprint-2"); !errors.Is(err, ErrInvitationAlreadyUsed) {
		t.Fatalf("unexpected reuse error: %v", err)
	}
}

func TestExpiredInvitationCannotBeRedeemed(t *testing.T) {
	state := newTestState(t)
	if err := state.CreateInvitation("cedar", "invite-1", "hash-1", time.Now().Add(-time.Minute)); !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("unexpected expired-create error: %v", err)
	}
}

func TestInvitationExpiryUsesTheInjectedClock(t *testing.T) {
	state := newTestState(t)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	state.Now = func() time.Time { return base }
	if err := state.CreateInvitation("cedar", "invite-1", "hash-1", base.Add(time.Hour)); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	// Advance past expiry without waiting.
	state.Now = func() time.Time { return base.Add(2 * time.Hour) }
	if err := state.RedeemInvitation("invite-1", "new-member", "fingerprint-1"); !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("expected expiry under the injected clock: %v", err)
	}
}

// --- membership events ------------------------------------------------------

func TestOwnerCanEjectAndRequireFreshAdmissionToRejoin(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "maple")
	if err := state.EjectMember("maple", "maple"); !errors.Is(err, ErrNotAdministrator) {
		t.Fatalf("unexpected non-admin ejection error: %v", err)
	}
	if err := state.EjectMember("cedar", "maple"); err != nil {
		t.Fatalf("eject member: %v", err)
	}
	if !state.IsEjected("maple") {
		t.Fatal("member should be ejected")
	}
	if state.PublishedLibraryCount("maple") != 0 {
		t.Fatal("ejection must remove the rejected member's published library records")
	}
	if state.CandidateLibraryCount("maple") != 0 {
		t.Fatal("ejection must also clear staged candidate publications")
	}
	if state.CanConsume("maple", "movies") {
		t.Fatal("ejected member's published media must not be consumable")
	}

	// Rejoin requires a new invitation, fresh approval, and a fresh publication.
	if err := state.CreateInvitation("cedar", "invite-new", "hash-new", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create rejoin invitation: %v", err)
	}
	if err := state.RedeemInvitation("invite-new", "maple", "fingerprint-new"); err != nil {
		t.Fatalf("redeem rejoin invitation: %v", err)
	}
	if err := state.ApproveInvitation("cedar", "invite-new", "approval-new"); err != nil {
		t.Fatalf("approve rejoin invitation: %v", err)
	}
	admission := Admission{
		GroupID:      "group-1",
		MemberID:     "maple",
		InvitationID: "invite-new",
		ApprovalID:   "approval-new",
		Sequence:     state.MembershipSequence + 1,
		IssuedAt:     time.Now().UTC(),
	}
	if err := state.ApplyVerifiedAdmission(admission); !errors.Is(err, ErrInsufficientPublications) {
		t.Fatalf("expected admission to require a new publication: %v", err)
	}
	// A non-member stages through the candidate set.
	stageTestLibrary(t, state, "maple", "movies")
	if err := state.ApplyVerifiedAdmission(admission); err != nil {
		t.Fatalf("fresh admission: %v", err)
	}
	if state.IsEjected("maple") || !state.CanConsume("maple", "movies") {
		t.Fatal("fresh admission should return the member to normal pool behavior")
	}
	if state.CandidateLibraryCount("maple") != 0 {
		t.Fatal("admission should promote candidates out of the staging set")
	}
}

func TestVerifiedRevocationPropagatesEjection(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "maple")
	revocation := Revocation{
		GroupID:  "group-1",
		MemberID: "maple",
		Sequence: state.MembershipSequence + 1,
		IssuedAt: time.Now().UTC(),
	}
	if err := state.ApplyVerifiedRevocation("cedar", revocation); err != nil {
		t.Fatalf("apply verified revocation: %v", err)
	}
	if !state.IsEjected("maple") {
		t.Fatal("verified revocation should eject the member")
	}
	if state.PublishedLibraryCount("maple") != 0 {
		t.Fatal("verified revocation should remove the member's published library records")
	}
	if state.CanConsume("maple", "movies") {
		t.Fatal("revoked member's published media must not be consumable")
	}
	if err := state.ApplyVerifiedRevocation("cedar", revocation); !errors.Is(err, ErrStaleRevocation) {
		t.Fatalf("unexpected stale revocation error: %v", err)
	}
}

func TestVerifiedRevocationCannotTargetOwner(t *testing.T) {
	state := newTestState(t)
	if err := state.ApplyVerifiedRevocation("cedar", Revocation{
		GroupID:  "group-1",
		MemberID: "cedar",
		Sequence: 1,
		IssuedAt: time.Now().UTC(),
	}); !errors.Is(err, ErrCannotEjectOwner) {
		t.Fatalf("unexpected owner revocation error: %v", err)
	}
}

func TestOwnerCannotEjectItself(t *testing.T) {
	state := newTestState(t)
	if err := state.EjectMember("cedar", "cedar"); !errors.Is(err, ErrCannotEjectOwner) {
		t.Fatalf("unexpected self-ejection error: %v", err)
	}
}

func TestAdministratorCannotEjectAnotherAdministrator(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "walnut")
	admitTestMember(t, state, "maple")
	if err := state.Roles.PromoteAdmin("cedar", "walnut"); err != nil {
		t.Fatalf("promote walnut: %v", err)
	}
	if err := state.Roles.PromoteAdmin("cedar", "maple"); err != nil {
		t.Fatalf("promote maple: %v", err)
	}
	if err := state.EjectMember("walnut", "maple"); err == nil {
		t.Fatal("an administrator must not be able to eject another administrator")
	}
	if !state.IsMember("maple") {
		t.Fatal("the targeted administrator should still be a member")
	}
}

func TestStateRequiresOwner(t *testing.T) {
	if _, err := NewState("group-1", ""); !errors.Is(err, ErrOwnerIDRequired) {
		t.Fatalf("unexpected constructor error: %v", err)
	}
	if _, err := NewState("", "cedar"); !errors.Is(err, ErrGroupIDRequired) {
		t.Fatalf("unexpected constructor error: %v", err)
	}
}

// --- publication ------------------------------------------------------------

func TestNonMemberCannotPublishIntoTheLivePool(t *testing.T) {
	state := newTestState(t)
	err := state.Publish(Publication{
		GroupID:      "group-1",
		SourceNodeID: "stranger",
		Library:      Library{ID: "movies", Name: "Movies", CollectionType: "movies"},
	})
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member must not publish into the live pool: %v", err)
	}
	if state.PublishedLibraryCount("stranger") != 0 {
		t.Fatal("no live publication should have been recorded")
	}
}

func TestAdmissionRuleIsSatisfiedByStagedCandidates(t *testing.T) {
	state := newTestState(t)
	if err := state.ValidateMemberAdmission("new-member"); !errors.Is(err, ErrInsufficientPublications) {
		t.Fatalf("unexpected admission error: %v", err)
	}
	stageTestLibrary(t, state, "new-member", "movies")
	if err := state.ValidateMemberAdmission("new-member"); err != nil {
		t.Fatalf("staged candidate should satisfy the admission rule: %v", err)
	}
	if state.CanConsume("new-member", "movies") {
		t.Fatal("a staged candidate must not be consumable before admission")
	}
}

func TestMemberCannotStageCandidates(t *testing.T) {
	state := newTestState(t)
	err := state.PublishCandidate(Publication{
		GroupID:      "group-1",
		SourceNodeID: "cedar",
		Library:      Library{ID: "movies", Name: "Movies", CollectionType: "movies"},
	})
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("an admitted member should publish directly, not stage: %v", err)
	}
}

func TestPublishedLibraryCountOnlyCountsSource(t *testing.T) {
	state := newTestState(t)
	publishTestLibrary(t, state, "cedar", "movies")
	admitTestMember(t, state, "maple")
	if count := state.PublishedLibraryCount("cedar"); count != 1 {
		t.Fatalf("unexpected cedar publication count: %d", count)
	}
	if count := state.PublishedLibraryCount("walnut"); count != 0 {
		t.Fatalf("unexpected walnut publication count: %d", count)
	}
}

// --- helpers ----------------------------------------------------------------

func stageTestLibrary(t *testing.T, state *State, nodeID string, libraryID string) {
	t.Helper()
	if err := state.PublishCandidate(Publication{
		GroupID:      "group-1",
		SourceNodeID: nodeID,
		Library:      Library{ID: libraryID, Name: libraryID, CollectionType: libraryID},
	}); err != nil {
		t.Fatalf("stage candidate library: %v", err)
	}
}

func publishTestLibrary(t *testing.T, state *State, nodeID string, libraryID string) {
	t.Helper()
	if err := state.Publish(Publication{
		GroupID:      "group-1",
		SourceNodeID: nodeID,
		Library:      Library{ID: libraryID, Name: libraryID, CollectionType: libraryID},
	}); err != nil {
		t.Fatalf("publish library: %v", err)
	}
}

func admitTestMember(t *testing.T, state *State, nodeID string) {
	t.Helper()
	stageTestLibrary(t, state, nodeID, "movies")
	invitationID := "invite-" + nodeID
	if err := state.CreateInvitation("cedar", invitationID, "hash-"+nodeID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create test invitation: %v", err)
	}
	if err := state.RedeemInvitation(invitationID, nodeID, "fingerprint-"+nodeID); err != nil {
		t.Fatalf("redeem test invitation: %v", err)
	}
	approvalID := "approval-" + nodeID
	if err := state.ApproveInvitation("cedar", invitationID, approvalID); err != nil {
		t.Fatalf("approve test invitation: %v", err)
	}
	if err := state.ApplyVerifiedAdmission(Admission{
		GroupID:      "group-1",
		MemberID:     nodeID,
		InvitationID: invitationID,
		ApprovalID:   approvalID,
		Sequence:     state.MembershipSequence + 1,
		IssuedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("admit test member: %v", err)
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

// C-PO-4 on the event path: a verified revocation enforces the same role
// protections as a local ejection.
func TestVerifiedRevocationEnforcesAdministratorProtection(t *testing.T) {
	state := newTestState(t)
	admitTestMember(t, state, "maple")
	admitTestMember(t, state, "walnut")
	if err := state.Roles.PromoteAdmin("cedar", "maple"); err != nil {
		t.Fatalf("promote maple: %v", err)
	}
	if err := state.Roles.PromoteAdmin("cedar", "walnut"); err != nil {
		t.Fatalf("promote walnut: %v", err)
	}
	revocation := Revocation{
		GroupID:  "group-1",
		MemberID: "maple",
		Sequence: state.MembershipSequence + 1,
		IssuedAt: time.Now().UTC(),
	}

	if err := state.ApplyVerifiedRevocation("walnut", revocation); !errors.Is(err, group.ErrAdminProtected) {
		t.Fatalf("administrator revoking an administrator: error = %v, want ErrAdminProtected", err)
	}
	if !state.IsAdministrator("maple") || !state.IsMember("maple") {
		t.Fatal("a refused revocation must leave the target administrator in place")
	}
	if err := state.ApplyVerifiedRevocation("birch", revocation); !errors.Is(err, ErrNotAdministrator) {
		t.Fatalf("revocation from a non-administrator: error = %v, want ErrNotAdministrator", err)
	}
	if err := state.ApplyVerifiedRevocation("cedar", revocation); err != nil {
		t.Fatalf("owner revoking an administrator: %v", err)
	}
	if state.IsMember("maple") {
		t.Fatal("the owner's revocation should remove the administrator")
	}
}
