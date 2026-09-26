package policy

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/node"
)

// The replicated group state is what policy consults in production.
var _ Roster = (*grouplog.State)(nil)

// fakeRoster stands in for the group log so that policy rules can be tested
// without building one.
type fakeRoster struct {
	members map[string]bool
	admins  map[string]bool
	ejected map[string]bool
}

func newFakeRoster(members ...string) *fakeRoster {
	roster := &fakeRoster{members: map[string]bool{}, admins: map[string]bool{"cedar": true}, ejected: map[string]bool{}}
	roster.members["cedar"] = true
	for _, member := range members {
		roster.members[member] = true
	}
	return roster
}

func (roster *fakeRoster) IsMember(nodeID string) bool        { return roster.members[nodeID] }
func (roster *fakeRoster) IsAdministrator(nodeID string) bool { return roster.admins[nodeID] }
func (roster *fakeRoster) IsEjected(nodeID string) bool       { return roster.ejected[nodeID] }

func (roster *fakeRoster) eject(nodeID string) {
	delete(roster.members, nodeID)
	delete(roster.admins, nodeID)
	roster.ejected[nodeID] = true
}

func newTestState(t *testing.T) *State {
	t.Helper()
	state, err := NewState("group-1")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	return state
}

func library(id string) Library {
	return Library{ID: id, Name: id, CollectionType: id}
}

func publishTestLibrary(t *testing.T, state *State, roster Roster, nodeID string, libraryID string) {
	t.Helper()
	if err := state.Publish(roster, Publication{GroupID: "group-1", SourceNodeID: nodeID, Library: library(libraryID)}); err != nil {
		t.Fatalf("publish library: %v", err)
	}
}

func stageTestLibrary(t *testing.T, state *State, roster Roster, nodeID string, libraryID string) {
	t.Helper()
	if err := state.PublishCandidate(roster, Publication{GroupID: "group-1", SourceNodeID: nodeID, Library: library(libraryID)}); err != nil {
		t.Fatalf("stage library: %v", err)
	}
}

// redeemTestInvitation creates an invitation from cedar and redeems it for
// nodeID, leaving it awaiting approval.
func redeemTestInvitation(t *testing.T, state *State, roster Roster, nodeID string) string {
	t.Helper()
	invitationID := "invite-" + nodeID
	if err := state.CreateInvitation(roster, "cedar", invitationID, "hash-"+nodeID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	if err := state.RedeemInvitation(roster, invitationID, nodeID, "fingerprint-"+nodeID); err != nil {
		t.Fatalf("redeem invitation: %v", err)
	}
	return invitationID
}

func TestStateRequiresAGroup(t *testing.T) {
	if _, err := NewState(" "); !errors.Is(err, ErrGroupIDRequired) {
		t.Fatalf("error = %v, want ErrGroupIDRequired", err)
	}
}

// --- publication and opt-out ---------------------------------------------

func TestPublishedLibraryIsAutoAccepted(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	publishTestLibrary(t, state, roster, "cedar", "movies")
	if !state.CanConsume(roster, "cedar", "movies") {
		t.Fatal("a published library should be consumable without an explicit accept")
	}
	if state.CanConsume(roster, "cedar", "tv") {
		t.Fatal("an unpublished library must never be consumable")
	}
}

func TestNonMemberCannotPublishIntoTheLivePool(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	err := state.Publish(roster, Publication{GroupID: "group-1", SourceNodeID: "stranger", Library: library("movies")})
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("error = %v, want ErrNotMember", err)
	}
	if state.PublishedLibraryCount("stranger") != 0 {
		t.Fatal("a refused publication must not be recorded")
	}
}

func TestDestinationCanOptOutAndOptBackIn(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	publishTestLibrary(t, state, roster, "cedar", "movies")
	if err := state.SetOptOut("cedar", "movies", true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	if state.CanConsume(roster, "cedar", "movies") {
		t.Fatal("an opted-out library must not be consumable")
	}
	if err := state.SetOptOut("cedar", "movies", false); err != nil {
		t.Fatalf("opt back in: %v", err)
	}
	if !state.CanConsume(roster, "cedar", "movies") {
		t.Fatal("opting back in should restore the library")
	}
}

func TestOptOutRequiresPublishedLibrary(t *testing.T) {
	state := newTestState(t)
	if err := state.SetOptOut("cedar", "movies", true); !errors.Is(err, ErrPublicationRequired) {
		t.Fatalf("error = %v, want ErrPublicationRequired", err)
	}
}

func TestBlockOverridesPublishedLibrary(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	publishTestLibrary(t, state, roster, "cedar", "movies")
	if err := state.BlockPeer("cedar"); err != nil {
		t.Fatalf("block: %v", err)
	}
	if state.CanConsume(roster, "cedar", "movies") {
		t.Fatal("a blocked peer's library must not be consumable")
	}
	if !roster.IsMember("cedar") {
		t.Fatal("a block leaves both nodes group members")
	}
}

func TestUnblockPreservesOptOut(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	publishTestLibrary(t, state, roster, "cedar", "movies")
	publishTestLibrary(t, state, roster, "cedar", "tv")
	if err := state.SetOptOut("cedar", "movies", true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	state.BlockPeer("cedar")
	state.UnblockPeer("cedar")
	if state.CanConsume(roster, "cedar", "movies") {
		t.Fatal("unblocking must not undo an explicit opt-out")
	}
	if !state.CanConsume(roster, "cedar", "tv") {
		t.Fatal("unblocking should restore libraries that were not opted out")
	}
}

func TestUnpublishRemovesAvailabilityButRetainsOptOut(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	publishTestLibrary(t, state, roster, "cedar", "movies")
	state.SetOptOut("cedar", "movies", true)
	if err := state.Unpublish("cedar", "movies"); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if state.CanConsume(roster, "cedar", "movies") {
		t.Fatal("an unpublished library must not be consumable")
	}
	publishTestLibrary(t, state, roster, "cedar", "movies")
	if state.CanConsume(roster, "cedar", "movies") {
		t.Fatal("republishing must not silently undo an earlier opt-out")
	}
}

func TestLibraryStatus(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("maple")
	publishTestLibrary(t, state, roster, "cedar", "movies")
	if status := state.LibraryStatus(roster, "cedar", "movies"); status != "movies:available" {
		t.Fatalf("status = %q", status)
	}
	state.SetOptOut("cedar", "movies", true)
	if status := state.LibraryStatus(roster, "cedar", "movies"); status != "movies:opted-out" {
		t.Fatalf("status = %q", status)
	}
	roster.eject("maple")
	if status := state.LibraryStatus(roster, "maple", "movies"); status != "ejected" {
		t.Fatalf("status = %q", status)
	}
}

func TestPublishedLibraryCountOnlyCountsSource(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("maple")
	publishTestLibrary(t, state, roster, "cedar", "movies")
	publishTestLibrary(t, state, roster, "cedar", "tv")
	publishTestLibrary(t, state, roster, "maple", "movies")
	if count := state.PublishedLibraryCount("cedar"); count != 2 {
		t.Fatalf("cedar count = %d, want 2", count)
	}
}

// --- invitations -----------------------------------------------------------

// C-PO-3: any member may invite; only an owner or administrator may approve.
func TestAnyMemberCanInviteButOnlyAdminsApprove(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("maple")
	if err := state.CreateInvitation(roster, "maple", "invite-1", "hash-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("a member should be able to invite: %v", err)
	}
	if err := state.CreateInvitation(roster, "stranger", "invite-2", "hash-2", time.Now().Add(time.Hour)); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member inviting: error = %v, want ErrNotMember", err)
	}
	stageTestLibrary(t, state, roster, "birch", "movies")
	if err := state.RedeemInvitation(roster, "invite-1", "birch", "fingerprint-birch"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if err := state.ApproveInvitation(roster, "maple", "invite-1", "approval-1"); !errors.Is(err, ErrNotAdministrator) {
		t.Fatalf("an ordinary member approving: error = %v, want ErrNotAdministrator", err)
	}
	if err := state.ApproveInvitation(roster, "cedar", "invite-1", "approval-1"); err != nil {
		t.Fatalf("the owner should be able to approve: %v", err)
	}
}

// C-PO-3: an administrator, not only the owner, may approve.
func TestPromotedAdministratorCanApproveInvitations(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("walnut")
	roster.admins["walnut"] = true
	stageTestLibrary(t, state, roster, "birch", "movies")
	invitationID := redeemTestInvitation(t, state, roster, "birch")
	if err := state.ApproveInvitation(roster, "walnut", invitationID, "approval-1"); err != nil {
		t.Fatalf("an administrator should be able to approve: %v", err)
	}
}

// C-PO-3: an administrator may deny; an ordinary member may not.
func TestAdministratorCanDenyInvitation(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("walnut", "maple")
	roster.admins["walnut"] = true
	invitationID := redeemTestInvitation(t, state, roster, "birch")
	if err := state.DenyInvitation(roster, "maple", invitationID); !errors.Is(err, ErrNotAdministrator) {
		t.Fatalf("an ordinary member denying: error = %v, want ErrNotAdministrator", err)
	}
	if err := state.DenyInvitation(roster, "walnut", invitationID); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if invitation, _ := state.InvitationByID(invitationID); invitation.Status != InvitationDenied {
		t.Fatalf("status = %q, want denied", invitation.Status)
	}
	if err := state.ApproveInvitation(roster, "cedar", invitationID, "approval-1"); !errors.Is(err, ErrInvitationNotPending) {
		t.Fatalf("approving a denied invitation: error = %v, want ErrInvitationNotPending", err)
	}
}

func TestInvitationCannotBeReused(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	redeemTestInvitation(t, state, roster, "birch")
	if err := state.RedeemInvitation(roster, "invite-birch", "spruce", "fingerprint-spruce"); !errors.Is(err, ErrInvitationAlreadyUsed) {
		t.Fatalf("error = %v, want ErrInvitationAlreadyUsed", err)
	}
}

func TestExpiredInvitationCannotBeRedeemed(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	if err := state.CreateInvitation(roster, "cedar", "invite-1", "hash-1", time.Now().Add(-time.Minute)); !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("error = %v, want ErrInvitationExpired", err)
	}
}

func TestInvitationExpiryUsesTheInjectedClock(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	state.Now = func() time.Time { return base }
	if err := state.CreateInvitation(roster, "cedar", "invite-1", "hash-1", base.Add(time.Hour)); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	state.Now = func() time.Time { return base.Add(2 * time.Hour) }
	if err := state.RedeemInvitation(roster, "invite-1", "birch", "fingerprint-birch"); !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("error = %v, want ErrInvitationExpired", err)
	}
}

func TestAMemberCannotRedeemAnInvitation(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("maple")
	state.CreateInvitation(roster, "cedar", "invite-1", "hash-1", time.Now().Add(time.Hour))
	if err := state.RedeemInvitation(roster, "invite-1", "maple", "fingerprint-maple"); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("error = %v, want ErrAlreadyMember", err)
	}
}

// --- admission rule and reconciliation -------------------------------------

// C-PO-6: a joining node satisfies the admission rule with staged candidates,
// before it is a member, and approval requires it.
func TestAdmissionRuleIsSatisfiedByStagedCandidates(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	invitationID := redeemTestInvitation(t, state, roster, "birch")
	if err := state.ApproveInvitation(roster, "cedar", invitationID, "approval-1"); !errors.Is(err, ErrInsufficientPublications) {
		t.Fatalf("approval without a staged library: error = %v, want ErrInsufficientPublications", err)
	}
	stageTestLibrary(t, state, roster, "birch", "movies")
	if err := state.ValidateMemberAdmission("birch"); err != nil {
		t.Fatalf("a staged candidate should satisfy the rule: %v", err)
	}
	if err := state.ApproveInvitation(roster, "cedar", invitationID, "approval-1"); err != nil {
		t.Fatalf("approval with a staged library: %v", err)
	}
	if state.CanConsume(roster, "birch", "movies") {
		t.Fatal("a staged candidate must not be consumable before admission")
	}
}

// C-PO-6: a member publishes directly; staging is only for joining nodes.
func TestMemberCannotStageCandidates(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("maple")
	err := state.PublishCandidate(roster, Publication{GroupID: "group-1", SourceNodeID: "maple", Library: library("movies")})
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("error = %v, want ErrAlreadyMember", err)
	}
}

// C-PO-6: once the log admits a node, its staged candidates go live.
func TestReconcilePromotesAnAdmittedNodesCandidates(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster()
	stageTestLibrary(t, state, roster, "birch", "movies")
	roster.members["birch"] = true
	state.Reconcile(roster)
	if state.CandidateLibraryCount("birch") != 0 || !state.CanConsume(roster, "birch", "movies") {
		t.Fatal("reconciling after admission should promote the staged library")
	}
}

// C-PO-5: a removed member's publications are purged, and they are never
// consumable even before reconciliation runs.
func TestARemovedMembersPublicationsArePurged(t *testing.T) {
	state, roster := newTestState(t), newFakeRoster("maple")
	publishTestLibrary(t, state, roster, "maple", "movies")
	stageTestLibrary(t, state, roster, "birch", "tv")

	roster.eject("maple")
	if state.CanConsume(roster, "maple", "movies") {
		t.Fatal("an ejected member's library must not be consumable, even before reconciling")
	}
	state.Reconcile(roster)
	if state.PublishedLibraryCount("maple") != 0 {
		t.Fatal("reconciling should purge an ejected member's publications")
	}
	if state.CandidateLibraryCount("birch") != 1 {
		t.Fatal("a pending joiner's candidates must survive reconciliation")
	}
	roster.ejected["birch"] = true
	state.Reconcile(roster)
	if state.CandidateLibraryCount("birch") != 0 {
		t.Fatal("an ejected node's staged candidates should be discarded")
	}
}

// C-PO-5 end to end against the real group log: ejection purges, and rejoining
// needs a fresh invitation.
func TestTheGroupLogDrivesPublicationState(t *testing.T) {
	identities := map[string]*node.Identity{}
	for _, name := range []string{"cedar", "maple"} {
		directory := t.TempDir()
		identity, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
		if err != nil {
			t.Fatalf("identity: %v", err)
		}
		identities[name] = identity
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	log, err := grouplog.Create(identities["cedar"], "group-1", "cedar", "Cedar", "cedar.example.org", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sequence := func(kind grouplog.Kind, body any) error {
		proposal, err := grouplog.NewProposal(identities["cedar"], "group-1", "cedar", kind, body, now)
		if err != nil {
			return err
		}
		_, err = log.Sequence(identities["cedar"], proposal, now)
		return err
	}
	admission := grouplog.AdmissionBody{MemberID: "maple", MemberKey: identities["maple"].PublicKey(), InvitationID: "invite-maple", InviterID: "cedar"}

	state := newTestState(t)
	stageTestLibrary(t, state, log.State(), "maple", "movies")
	if err := sequence(grouplog.KindAdmission, admission); err != nil {
		t.Fatalf("admit: %v", err)
	}
	state.Reconcile(log.State())
	if !state.CanConsume(log.State(), "maple", "movies") {
		t.Fatal("an admitted member's staged library should be live")
	}

	if err := sequence(grouplog.KindEjection, grouplog.MemberBody{MemberID: "maple"}); err != nil {
		t.Fatalf("eject: %v", err)
	}
	state.Reconcile(log.State())
	if state.PublishedLibraryCount("maple") != 0 || state.LibraryStatus(log.State(), "maple", "movies") != "ejected" {
		t.Fatal("ejection should purge the member's publications")
	}
	if err := sequence(grouplog.KindAdmission, admission); !errors.Is(err, grouplog.ErrInvitationConsumed) {
		t.Fatalf("rejoining on the old invitation: error = %v, want ErrInvitationConsumed", err)
	}
	admission.InvitationID = "invite-maple-2"
	if err := sequence(grouplog.KindAdmission, admission); err != nil {
		t.Fatalf("rejoining on a fresh invitation: %v", err)
	}
}
