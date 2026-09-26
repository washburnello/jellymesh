package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"jellymesh/internal/group"
	"jellymesh/internal/policy"
)

// staticRoster is a fixed membership for exercising policy after a reload.
type staticRoster map[string]bool

func (roster staticRoster) IsMember(nodeID string) bool        { return roster[nodeID] }
func (roster staticRoster) IsAdministrator(nodeID string) bool { return nodeID == "cedar" }
func (roster staticRoster) IsEjected(nodeID string) bool       { return false }

var policyTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// buildPolicyState populates every kind of record the repository stores: a
// live publication, a staged candidate, an opt-out, a block, and invitations
// in every status.
func buildPolicyState(t *testing.T) (*policy.State, staticRoster) {
	t.Helper()
	roster := staticRoster{"cedar": true, "maple": true}
	state, err := policy.NewState("group-1")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	state.Now = func() time.Time { return policyTime }
	publish := func(node, id string) {
		if err := state.Publish(roster, policy.Publication{GroupID: "group-1", SourceNodeID: node, Library: policy.Library{ID: id, Name: id, CollectionType: "movies"}}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	publish("cedar", "movies")
	publish("maple", "movies")
	publish("maple", "films")
	if err := state.PublishCandidate(roster, policy.Publication{GroupID: "group-1", SourceNodeID: "juniper", Library: policy.Library{ID: "tv", Name: "tv", CollectionType: "tvshows"}}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := state.SetOptOut("maple", "films", true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	if err := state.BlockPeer("walnut"); err != nil {
		t.Fatalf("block: %v", err)
	}

	expires := policyTime.Add(time.Hour)
	for _, id := range []string{"created", "pending", "approved", "denied"} {
		if err := state.CreateInvitation(roster, "cedar", "invite-"+id, "hash-"+id, expires); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, id := range []string{"pending", "approved", "denied"} {
		if err := state.RedeemInvitation(roster, "invite-"+id, "juniper-"+id, "fingerprint-"+id); err != nil {
			t.Fatalf("redeem %s: %v", id, err)
		}
	}
	if err := state.PublishCandidate(roster, policy.Publication{GroupID: "group-1", SourceNodeID: "juniper-approved", Library: policy.Library{ID: "music", Name: "music", CollectionType: "music"}}); err != nil {
		t.Fatalf("stage for approval: %v", err)
	}
	if err := state.ApproveInvitation(roster, "cedar", "invite-approved", "approval-1"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := state.DenyInvitation(roster, "cedar", "invite-denied"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	return state, roster
}

// C-PO-8: publications, candidates, opt-outs, blocks, and invitations survive
// a restart, and behaviour after reloading matches behaviour before it.
func TestPolicyRepositorySaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "jellymesh.db")
	ctx := context.Background()
	state, roster := buildPolicyState(t)

	database := reopen(t, path)
	if err := NewPolicyRepository(database).Save(ctx, state); err != nil {
		t.Fatalf("save: %v", err)
	}
	database.Close()

	loaded, err := NewPolicyRepository(reopen(t, path)).Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	loaded.Now = state.Now
	if !reflect.DeepEqual(loaded.Published, state.Published) || !reflect.DeepEqual(loaded.Candidates, state.Candidates) {
		t.Fatal("publications differ after reload")
	}
	if !reflect.DeepEqual(loaded.OptOuts, state.OptOuts) || !reflect.DeepEqual(loaded.BlockedPeers, state.BlockedPeers) {
		t.Fatal("opt-outs or blocks differ after reload")
	}
	if !reflect.DeepEqual(loaded.Invitations, state.Invitations) {
		t.Fatal("invitations differ after reload")
	}

	for _, source := range []string{"cedar", "maple", "walnut", "juniper"} {
		for _, library := range []string{"movies", "films", "tv"} {
			if loaded.CanConsume(roster, source, library) != state.CanConsume(roster, source, library) {
				t.Fatalf("CanConsume(%s, %s) differs after reload", source, library)
			}
		}
	}
	if err := loaded.ValidateMemberAdmission("juniper"); err != nil {
		t.Fatalf("a staged candidate should still satisfy admission after reload: %v", err)
	}
	if pending := loaded.PendingApprovals(); len(pending) != 1 || pending[0].InvitationID != "invite-pending" {
		t.Fatalf("pending approvals after reload: %+v", pending)
	}
}

// C-PO-8: saving again replaces rather than duplicates.
func TestPolicyRepositorySaveReplacesRatherThanDuplicating(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	repo := NewPolicyRepository(database)
	state, _ := buildPolicyState(t)
	if err := repo.Save(ctx, state); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := state.Unpublish("maple", "movies"); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if err := repo.Save(ctx, state); err != nil {
		t.Fatalf("second save: %v", err)
	}
	loaded, err := repo.Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.PublishedLibraryCount("maple") != 1 {
		t.Fatalf("maple has %d publications, want 1", loaded.PublishedLibraryCount("maple"))
	}
}

func TestPolicyRepositoryLoadOfAnUnknownGroupIsEmpty(t *testing.T) {
	loaded, err := NewPolicyRepository(openTestDB(t)).Load(context.Background(), "group-2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Published) != 0 || len(loaded.Invitations) != 0 {
		t.Fatal("an unknown group should load empty")
	}
}

func TestPolicyRepositoryDeleteKeepsBlocks(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	repo := NewPolicyRepository(database)
	state, _ := buildPolicyState(t)
	if err := repo.Save(ctx, state); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.Delete(ctx, "group-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	loaded, err := repo.Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Published) != 0 || len(loaded.Invitations) != 0 {
		t.Fatal("delete should remove the group's policy")
	}
	if blocked, _ := NewPeerRepository(database).IsBlocked(ctx, "walnut"); !blocked {
		t.Fatal("a block is about a node, and survives deleting one group's policy")
	}
}

// C-BL-5: saving policy never lifts a block, including one recorded through
// the peer repository rather than through policy state.
func TestPolicySaveDoesNotLiftABlock(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	peers := NewPeerRepository(database)
	if err := peers.SetBlocked(ctx, "maple", true); err != nil {
		t.Fatalf("set blocked: %v", err)
	}
	state, err := policy.NewState("group-1")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := NewPolicyRepository(database).Save(ctx, state); err != nil {
		t.Fatalf("save: %v", err)
	}
	if blocked, _ := peers.IsBlocked(ctx, "maple"); !blocked {
		t.Fatal("an unrelated policy save lifted a block")
	}
	loaded, err := NewPolicyRepository(database).Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.IsPeerBlocked("maple") {
		t.Fatal("the reloaded state should report the block")
	}
}

// C-PO-8: an owner-absence window survives a restart rather than starting
// again from zero.
func TestOwnerWatchSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "jellymesh.db")
	ctx := context.Background()
	watch, err := group.NewWatch("group-1", "cedar")
	if err != nil {
		t.Fatalf("new watch: %v", err)
	}
	if err := watch.OwnerUnavailable(policyTime, true); err != nil {
		t.Fatalf("owner unavailable: %v", err)
	}
	watch.Advance(policyTime, true)

	database := reopen(t, path)
	if err := NewOwnerWatchRepository(database).Save(ctx, watch); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := NewOwnerWatchRepository(database).Save(ctx, watch); err != nil {
		t.Fatalf("save again: %v", err)
	}
	database.Close()

	loaded, found, err := NewOwnerWatchRepository(reopen(t, path)).Load(ctx, "group-1")
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(loaded, watch) {
		t.Fatalf("watch after reload = %+v, want %+v", loaded, watch)
	}
	if decision := loaded.Advance(policyTime.Add(group.OwnerSuccessionTimeout), true); !decision.ClaimEligible {
		t.Fatal("the reloaded window should still reach its deadline")
	}
	if _, found, _ := NewOwnerWatchRepository(reopen(t, path)).Load(ctx, "group-2"); found {
		t.Fatal("an unknown group has no watch")
	}
}

func TestGroupLogRepositoryListGroupIDs(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	f := newLogFixture(t)
	if err := NewGroupLogRepository(database).Save(ctx, f.log); err != nil {
		t.Fatalf("save: %v", err)
	}
	ids, err := NewGroupLogRepository(database).ListGroupIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "group-1" {
		t.Fatalf("ids = %v, err %v", ids, err)
	}
}
