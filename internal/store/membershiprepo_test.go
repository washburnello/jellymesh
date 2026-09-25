package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"jellymesh/internal/group"
	"jellymesh/internal/policy"
)

// admitMember stages a candidate library, then walks a fresh invitation
// through creation, redemption, and approval before admitting nodeID, so
// that the resulting state satisfies the same admission rule production code
// enforces (C-PO-6) rather than reaching into internals to fake membership.
func admitMember(t *testing.T, state *policy.State, nodeID string) {
	t.Helper()
	if err := state.PublishCandidate(policy.Publication{
		GroupID:      "group-1",
		SourceNodeID: nodeID,
		Library:      policy.Library{ID: "movies", Name: "Movies", CollectionType: "movies"},
	}); err != nil {
		t.Fatalf("stage candidate for %s: %v", nodeID, err)
	}
	invitationID := "invite-" + nodeID
	if err := state.CreateInvitation("cedar", invitationID, "hash-"+nodeID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invitation for %s: %v", nodeID, err)
	}
	if err := state.RedeemInvitation(invitationID, nodeID, "fingerprint-"+nodeID); err != nil {
		t.Fatalf("redeem invitation for %s: %v", nodeID, err)
	}
	approvalID := "approval-" + nodeID
	if err := state.ApproveInvitation("cedar", invitationID, approvalID); err != nil {
		t.Fatalf("approve invitation for %s: %v", nodeID, err)
	}
	if err := state.ApplyVerifiedAdmission(policy.Admission{
		GroupID:      "group-1",
		MemberID:     nodeID,
		InvitationID: invitationID,
		ApprovalID:   approvalID,
		Sequence:     state.MembershipSequence + 1,
		IssuedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("admit %s: %v", nodeID, err)
	}
}

// buildFullyPopulatedState assembles a policy.State that exercises every
// field C-PO-8 requires to survive a restart: an owner, several members, two
// administrators promoted at different times, an ejected member, a blocked
// peer, a live publication alongside a staged candidate, an opt-out, and
// invitations spanning every status.
func buildFullyPopulatedState(t *testing.T, database *DB) *policy.State {
	t.Helper()
	ctx := context.Background()

	state, err := policy.NewState("group-1", "cedar")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}

	for _, nodeID := range []string{"maple", "walnut", "birch", "spruce"} {
		admitMember(t, state, nodeID)
	}

	maplePromotedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	walnutPromotedAt := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	if err := state.Roles.PromoteAdminAt("cedar", "maple", maplePromotedAt); err != nil {
		t.Fatalf("promote maple: %v", err)
	}
	if err := state.Roles.PromoteAdminAt("cedar", "walnut", walnutPromotedAt); err != nil {
		t.Fatalf("promote walnut: %v", err)
	}

	if err := state.EjectMember("cedar", "spruce"); err != nil {
		t.Fatalf("eject spruce: %v", err)
	}

	if err := state.Publish(policy.Publication{
		GroupID:      "group-1",
		SourceNodeID: "birch",
		Library:      policy.Library{ID: "tv", Name: "TV Shows", CollectionType: "tvshows"},
		PublishedAt:  time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("publish birch/tv: %v", err)
	}
	if err := state.PublishCandidate(policy.Publication{
		GroupID:      "group-1",
		SourceNodeID: "juniper",
		Library:      policy.Library{ID: "docs", Name: "Documentaries", CollectionType: "movies"},
		PublishedAt:  time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("stage juniper/docs: %v", err)
	}

	if err := state.SetOptOut("walnut", "movies", true); err != nil {
		t.Fatalf("opt out of walnut/movies: %v", err)
	}

	// A block is a pairwise decision about a peer this node has actually
	// seen, so a real peers row has to exist before it can be persisted.
	if err := NewPeerRepository(database).Upsert(ctx, Peer{
		NodeID:      "walnut",
		Fingerprint: "fingerprint-walnut",
		Trusted:     true,
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed walnut peer record: %v", err)
	}
	if err := state.BlockPeer("walnut"); err != nil {
		t.Fatalf("block walnut: %v", err)
	}

	successionStart := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := state.Roles.MarkOwnerUnavailable(successionStart); err != nil {
		t.Fatalf("mark owner unavailable: %v", err)
	}

	// Invitations spanning every status.
	if err := state.CreateInvitation("cedar", "invite-created", "hash-created", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite-created: %v", err)
	}
	if err := state.CreateInvitation("cedar", "invite-hazel", "hash-hazel", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite-hazel: %v", err)
	}
	if err := state.RedeemInvitation("invite-hazel", "hazel", "fingerprint-hazel"); err != nil {
		t.Fatalf("redeem invite-hazel: %v", err)
	}
	if err := state.CreateInvitation("cedar", "invite-denied", "hash-denied", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite-denied: %v", err)
	}
	if err := state.RedeemInvitation("invite-denied", "denied-node", "fingerprint-denied"); err != nil {
		t.Fatalf("redeem invite-denied: %v", err)
	}
	if err := state.DenyInvitation("cedar", "invite-denied"); err != nil {
		t.Fatalf("deny invite-denied: %v", err)
	}
	// invite-maple (etc.) from admitMember are already left in status
	// "approved", which covers that status without any extra setup.

	// A genuinely expired invitation: create it against a fixed clock, then
	// advance that clock past its expiry before redeeming it, which is what
	// flips its stored status to "expired".
	expiryBase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state.Now = func() time.Time { return expiryBase }
	if err := state.CreateInvitation("cedar", "invite-expired", "hash-expired", expiryBase.Add(time.Minute)); err != nil {
		t.Fatalf("create invite-expired: %v", err)
	}
	state.Now = func() time.Time { return expiryBase.Add(time.Hour) }
	if err := state.RedeemInvitation("invite-expired", "late-node", "fingerprint-late"); !errors.Is(err, policy.ErrInvitationExpired) {
		t.Fatalf("redeem invite-expired: error = %v, want ErrInvitationExpired", err)
	}
	state.Now = nil

	return state
}

func TestMembershipRepositorySaveAndLoadRoundTrip(t *testing.T) {
	database := openTestDB(t)
	repo := NewMembershipRepository(database)
	ctx := context.Background()

	state := buildFullyPopulatedState(t, database)
	expectedSequence := state.MembershipSequence

	if err := repo.Save(ctx, state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, found, err := repo.Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !found {
		t.Fatal("expected the saved group to be found")
	}

	// --- group_state fields ---
	if loaded.Roles.OwnerID != "cedar" {
		t.Fatalf("owner = %q, want cedar", loaded.Roles.OwnerID)
	}
	if loaded.Roles.Status != group.StatusSuccessionPending {
		t.Fatalf("status = %q, want %q", loaded.Roles.Status, group.StatusSuccessionPending)
	}
	if loaded.MembershipSequence != expectedSequence {
		t.Fatalf("membership sequence = %d, want %d", loaded.MembershipSequence, expectedSequence)
	}
	if !loaded.Roles.OwnerUnavailableSince.Equal(state.Roles.OwnerUnavailableSince) {
		t.Fatalf("owner_unavailable_since = %v, want %v", loaded.Roles.OwnerUnavailableSince, state.Roles.OwnerUnavailableSince)
	}
	if !loaded.Roles.SuccessionDeadline.Equal(state.Roles.SuccessionDeadline) {
		t.Fatalf("succession_deadline = %v, want %v", loaded.Roles.SuccessionDeadline, state.Roles.SuccessionDeadline)
	}
	if !loaded.Roles.DissolutionDeadline.Equal(state.Roles.DissolutionDeadline) {
		t.Fatalf("dissolution_deadline = %v, want %v", loaded.Roles.DissolutionDeadline, state.Roles.DissolutionDeadline)
	}
	if !loaded.Roles.LastNotificationAt.Equal(state.Roles.LastNotificationAt) {
		t.Fatalf("last_notification_at = %v, want %v", loaded.Roles.LastNotificationAt, state.Roles.LastNotificationAt)
	}

	// --- membership, roles, ejection: data ---
	for _, nodeID := range []string{"cedar", "maple", "walnut", "birch"} {
		if !loaded.IsMember(nodeID) {
			t.Fatalf("expected %s to be a member after reload", nodeID)
		}
	}
	if loaded.IsMember("spruce") {
		t.Fatal("ejected member must not be a member after reload")
	}
	if !loaded.IsEjected("spruce") {
		t.Fatal("expected spruce to be ejected after reload")
	}
	admin, ok := loaded.Roles.Admins["maple"]
	if !ok || !admin.PromotedAt.Equal(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("maple admin record = %+v, ok=%v", admin, ok)
	}
	admin, ok = loaded.Roles.Admins["walnut"]
	if !ok || !admin.PromotedAt.Equal(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("walnut admin record = %+v, ok=%v", admin, ok)
	}

	// --- publications: live vs staged candidate are distinct states ---
	if !loaded.CanConsume("birch", "tv") {
		t.Fatal("birch/tv should be a live, consumable publication after reload")
	}
	if loaded.PublishedLibraryCount("juniper") != 0 {
		t.Fatal("juniper's library must still be a candidate, not published")
	}
	if loaded.CandidateLibraryCount("juniper") != 1 {
		t.Fatal("juniper's staged candidate did not survive reload")
	}

	// --- opt-outs ---
	if !loaded.IsOptedOut("walnut", "movies") {
		t.Fatal("expected walnut/movies to still be opted out after reload")
	}
	if loaded.CanConsume("walnut", "movies") {
		t.Fatal("an opted-out (and blocked) library must not be consumable")
	}

	// --- blocked peers ---
	if !loaded.IsPeerBlocked("walnut") {
		t.Fatal("expected walnut to still be blocked after reload")
	}

	// --- invitations: every field, every status ---
	wantStatuses := map[string]policy.InvitationStatus{
		"invite-created": policy.InvitationCreated,
		"invite-hazel":   policy.InvitationAwaitingApproval,
		"invite-denied":  policy.InvitationDenied,
		"invite-expired": policy.InvitationExpired,
		"invite-maple":   policy.InvitationApproved,
	}
	for invitationID, wantStatus := range wantStatuses {
		want, ok := state.Invitations[invitationID]
		if !ok {
			t.Fatalf("test setup bug: %s missing from original state", invitationID)
		}
		got, ok := loaded.Invitations[invitationID]
		if !ok {
			t.Fatalf("%s missing after reload", invitationID)
		}
		if got.Status != wantStatus {
			t.Fatalf("%s status = %q, want %q", invitationID, got.Status, wantStatus)
		}
		if got.GroupID != want.GroupID || got.InviterID != want.InviterID || got.InviteeID != want.InviteeID ||
			got.CodeHash != want.CodeHash || got.Fingerprint != want.Fingerprint || got.ApprovalID != want.ApprovalID {
			t.Fatalf("%s fields = %+v, want %+v", invitationID, got, want)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("%s created_at = %v, want %v", invitationID, got.CreatedAt, want.CreatedAt)
		}
		if !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Fatalf("%s expires_at = %v, want %v", invitationID, got.ExpiresAt, want.ExpiresAt)
		}
	}

	// --- behaviour, not just data ---
	for _, nodeID := range []string{"cedar", "maple", "walnut", "birch", "spruce", "juniper", "nobody"} {
		if loaded.IsOwner(nodeID) != state.IsOwner(nodeID) {
			t.Fatalf("IsOwner(%s) disagrees after reload", nodeID)
		}
		if loaded.IsAdministrator(nodeID) != state.IsAdministrator(nodeID) {
			t.Fatalf("IsAdministrator(%s) disagrees after reload", nodeID)
		}
		if loaded.IsMember(nodeID) != state.IsMember(nodeID) {
			t.Fatalf("IsMember(%s) disagrees after reload", nodeID)
		}
		if loaded.IsEjected(nodeID) != state.IsEjected(nodeID) {
			t.Fatalf("IsEjected(%s) disagrees after reload", nodeID)
		}
		if loaded.IsPeerBlocked(nodeID) != state.IsPeerBlocked(nodeID) {
			t.Fatalf("IsPeerBlocked(%s) disagrees after reload", nodeID)
		}
	}
	for _, publication := range []struct{ source, library string }{{"birch", "tv"}, {"walnut", "movies"}} {
		if loaded.CanConsume(publication.source, publication.library) != state.CanConsume(publication.source, publication.library) {
			t.Fatalf("CanConsume(%s,%s) disagrees after reload", publication.source, publication.library)
		}
		if loaded.IsOptedOut(publication.source, publication.library) != state.IsOptedOut(publication.source, publication.library) {
			t.Fatalf("IsOptedOut(%s,%s) disagrees after reload", publication.source, publication.library)
		}
	}

	// Succession must select the same eligible successor after a restart:
	// maple was promoted before walnut, so maple remains the oldest
	// administrator and therefore the eligible successor.
	pastDeadline := successionDeadlineFor(state)
	originalDecision := state.Advance(pastDeadline)
	loadedDecision := loaded.Advance(pastDeadline)
	if originalDecision.SuccessionEligible != loadedDecision.SuccessionEligible {
		t.Fatalf("succession disagrees after reload: original=%q loaded=%q",
			originalDecision.SuccessionEligible, loadedDecision.SuccessionEligible)
	}
	if loadedDecision.SuccessionEligible != "maple" {
		t.Fatalf("eligible successor = %q, want maple", loadedDecision.SuccessionEligible)
	}

	// A joining node whose only publication is a staged candidate must still
	// satisfy the admission rule after a restart.
	if err := loaded.ValidateMemberAdmission("juniper"); err != nil {
		t.Fatalf("juniper should satisfy admission via its staged candidate: %v", err)
	}

	// The membership sequence must be preserved so a replayed, stale
	// revocation is still rejected after a restart.
	err = loaded.ApplyVerifiedRevocation(policy.Revocation{
		GroupID:  "group-1",
		MemberID: "birch",
		Sequence: loaded.MembershipSequence,
		IssuedAt: time.Now(),
	})
	if !errors.Is(err, policy.ErrStaleRevocation) {
		t.Fatalf("replayed revocation after reload: error = %v, want ErrStaleRevocation", err)
	}
}

// successionDeadlineFor returns a time safely past the succession deadline
// state.Roles carries, for exercising Advance without waiting real days.
func successionDeadlineFor(state *policy.State) time.Time {
	return state.Roles.SuccessionDeadline.Add(24 * time.Hour)
}

func TestMembershipRepositorySaveUpdatesRatherThanDuplicating(t *testing.T) {
	database := openTestDB(t)
	repo := NewMembershipRepository(database)
	ctx := context.Background()

	state, err := policy.NewState("group-1", "cedar")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	admitMember(t, state, "maple")
	if err := repo.Save(ctx, state); err != nil {
		t.Fatalf("first save: %v", err)
	}

	admitMember(t, state, "walnut")
	if err := state.Roles.PromoteAdminAt("cedar", "maple", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("promote maple: %v", err)
	}
	if err := repo.Save(ctx, state); err != nil {
		t.Fatalf("second save: %v", err)
	}

	var groupCount, memberCount int
	if err := database.SQL().QueryRow(`SELECT count(*) FROM group_state WHERE group_id = 'group-1'`).Scan(&groupCount); err != nil {
		t.Fatalf("count group_state: %v", err)
	}
	if groupCount != 1 {
		t.Fatalf("expected exactly one group_state row, got %d", groupCount)
	}
	if err := database.SQL().QueryRow(`SELECT count(*) FROM members WHERE group_id = 'group-1'`).Scan(&memberCount); err != nil {
		t.Fatalf("count members: %v", err)
	}
	// cedar, maple, walnut.
	if memberCount != 3 {
		t.Fatalf("expected 3 member rows, got %d", memberCount)
	}

	loaded, found, err := repo.Load(ctx, "group-1")
	if err != nil || !found {
		t.Fatalf("load after second save: found=%v err=%v", found, err)
	}
	if !loaded.IsMember("walnut") {
		t.Fatal("second save's new member did not survive")
	}
	if !loaded.IsAdministrator("maple") {
		t.Fatal("second save's new admin did not survive")
	}
}

func TestMembershipRepositoryLoadUnknownGroupReturnsNotFound(t *testing.T) {
	database := openTestDB(t)
	repo := NewMembershipRepository(database)

	state, found, err := repo.Load(context.Background(), "nonexistent-group")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if found {
		t.Fatal("expected an unknown group id not to be found")
	}
	if state != nil {
		t.Fatal("expected a nil state for an unknown group id")
	}
}

func TestMembershipRepositoryDelete(t *testing.T) {
	database := openTestDB(t)
	repo := NewMembershipRepository(database)
	ctx := context.Background()

	state, err := policy.NewState("group-1", "cedar")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	admitMember(t, state, "maple")
	if err := repo.Save(ctx, state); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := repo.Delete(ctx, "group-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, found, err := repo.Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if found {
		t.Fatal("expected the group to be gone after delete")
	}

	for _, table := range []string{"members", "publications", "opt_outs", "invitations"} {
		var count int
		if err := database.SQL().QueryRow(`SELECT count(*) FROM ` + table + ` WHERE group_id = 'group-1'`).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected %s to be empty after delete, got %d rows", table, count)
		}
	}

	// Deleting an already-absent group is not an error.
	if err := repo.Delete(ctx, "group-1"); err != nil {
		t.Fatalf("delete missing group: %v", err)
	}
}

func TestMembershipRepositoryListGroupIDs(t *testing.T) {
	database := openTestDB(t)
	repo := NewMembershipRepository(database)
	ctx := context.Background()

	for _, groupID := range []string{"group-b", "group-a", "group-c"} {
		state, err := policy.NewState(groupID, "cedar")
		if err != nil {
			t.Fatalf("new state %s: %v", groupID, err)
		}
		if err := repo.Save(ctx, state); err != nil {
			t.Fatalf("save %s: %v", groupID, err)
		}
	}

	groupIDs, err := repo.ListGroupIDs(ctx)
	if err != nil {
		t.Fatalf("list group ids: %v", err)
	}
	want := []string{"group-a", "group-b", "group-c"}
	if len(groupIDs) != len(want) {
		t.Fatalf("group ids = %v, want %v", groupIDs, want)
	}
	for i := range want {
		if groupIDs[i] != want[i] {
			t.Fatalf("group ids = %v, want %v", groupIDs, want)
		}
	}
}

func TestMembershipRepositorySaveRequiresStateAndGroupID(t *testing.T) {
	database := openTestDB(t)
	repo := NewMembershipRepository(database)
	ctx := context.Background()

	if err := repo.Save(ctx, nil); !errors.Is(err, ErrMembershipStateRequired) {
		t.Fatalf("save nil state: error = %v, want ErrMembershipStateRequired", err)
	}
	if err := repo.Save(ctx, &policy.State{}); !errors.Is(err, ErrMembershipRolesRequired) {
		t.Fatalf("save state without roles: error = %v, want ErrMembershipRolesRequired", err)
	}

	state, err := policy.NewState("group-1", "cedar")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	state.GroupID = ""
	if err := repo.Save(ctx, state); !errors.Is(err, ErrMembershipGroupIDRequired) {
		t.Fatalf("save state without group id: error = %v, want ErrMembershipGroupIDRequired", err)
	}

	if _, _, err := repo.Load(ctx, ""); !errors.Is(err, ErrMembershipGroupIDRequired) {
		t.Fatalf("load with empty group id: error = %v, want ErrMembershipGroupIDRequired", err)
	}
	if err := repo.Delete(ctx, ""); !errors.Is(err, ErrMembershipGroupIDRequired) {
		t.Fatalf("delete with empty group id: error = %v, want ErrMembershipGroupIDRequired", err)
	}
}
