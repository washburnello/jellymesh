package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/transport"
)

func samplePeer(nodeID string, fingerprint transport.Fingerprint) Peer {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := created.Add(time.Hour)
	return Peer{
		NodeID:         nodeID,
		Fingerprint:    fingerprint,
		FriendlyName:   "Cedar's Server",
		PublicHostname: "cedar.example.com",
		Blocked:        false,
		CreatedAt:      created,
		UpdatedAt:      updated,
	}
}

func TestPeerUpsertRoundTripsByNodeIDAndFingerprint(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-cedar")
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	byNode, found, err := repo.ByNodeID(ctx, "cedar")
	if err != nil {
		t.Fatalf("by node id: %v", err)
	}
	if !found {
		t.Fatal("expected peer to be found by node id")
	}
	if byNode != peer {
		t.Fatalf("round trip by node id changed the value: got %+v, want %+v", byNode, peer)
	}

	byFingerprint, found, err := repo.ByFingerprint(ctx, "fingerprint-cedar")
	if err != nil {
		t.Fatalf("by fingerprint: %v", err)
	}
	if !found {
		t.Fatal("expected peer to be found by fingerprint")
	}
	if byFingerprint != peer {
		t.Fatalf("round trip by fingerprint changed the value: got %+v, want %+v", byFingerprint, peer)
	}
}

func TestPeerByNodeIDNotFound(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	_, found, err := repo.ByNodeID(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("by node id: %v", err)
	}
	if found {
		t.Fatal("expected no peer to be found")
	}
}

func TestUpsertRequiresNodeIDAndFingerprint(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	if err := repo.Upsert(ctx, Peer{Fingerprint: "fp"}); !errors.Is(err, ErrNodeIDRequired) {
		t.Fatalf("error = %v, want ErrNodeIDRequired", err)
	}
	if err := repo.Upsert(ctx, Peer{NodeID: "cedar"}); !errors.Is(err, ErrFingerprintRequired) {
		t.Fatalf("error = %v, want ErrFingerprintRequired", err)
	}
}

func TestFingerprintCollisionBetweenTwoNodesIsRejected(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	if err := repo.Upsert(ctx, samplePeer("cedar", "shared-fingerprint")); err != nil {
		t.Fatalf("upsert cedar: %v", err)
	}

	err := repo.Upsert(ctx, samplePeer("walnut", "shared-fingerprint"))
	if !errors.Is(err, ErrFingerprintInUse) {
		t.Fatalf("error = %v, want ErrFingerprintInUse", err)
	}

	// The rejected write must not have gone through: walnut should not exist,
	// and cedar must still own the fingerprint.
	if _, found, _ := repo.ByNodeID(ctx, "walnut"); found {
		t.Fatal("a rejected upsert must not create the conflicting node")
	}
	owner, found, err := repo.ByFingerprint(ctx, "shared-fingerprint")
	if err != nil || !found {
		t.Fatalf("by fingerprint: found=%v err=%v", found, err)
	}
	if owner.NodeID != "cedar" {
		t.Fatalf("fingerprint owner = %q, want cedar", owner.NodeID)
	}
}

// C-TR-7: there is no key rotation, so a known node cannot be re-keyed in
// place in the directory either.
func TestUpsertRefusesToChangeAKnownNodesFingerprint(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-one")
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	peer.Fingerprint = "fingerprint-two"
	if err := repo.Upsert(ctx, peer); !errors.Is(err, ErrFingerprintChanged) {
		t.Fatalf("re-keying upsert: error = %v, want ErrFingerprintChanged", err)
	}
	got, _, err := repo.ByNodeID(ctx, "cedar")
	if err != nil || got.Fingerprint != "fingerprint-one" {
		t.Fatalf("a refused re-key must leave the record untouched: %+v, %v", got, err)
	}

	// Re-enrollment is explicit: remove the old record, then record the new key.
	if err := repo.Remove(ctx, "cedar"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("record new key after removal: %v", err)
	}
}

// C-BL-5: updating a peer's descriptive fields never lifts its block.
func TestUpsertOfAKnownPeerDoesNotLiftABlock(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-cedar")
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetBlocked(ctx, "cedar", true); err != nil {
		t.Fatalf("set blocked: %v", err)
	}
	peer.FriendlyName = "Renamed"
	peer.Blocked = false
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, err := repo.ByNodeID(ctx, "cedar")
	if err != nil {
		t.Fatalf("by node id: %v", err)
	}
	if got.FriendlyName != "Renamed" {
		t.Fatalf("friendly name = %q, want Renamed", got.FriendlyName)
	}
	if !got.Blocked {
		t.Fatal("an upsert must not lift a block")
	}
}

// C-PO-12: a block is durable for a node that has never connected, applies
// once it appears, and outlives its directory entry.
func TestBlockOfAnUnseenPeerIsDurableAndApplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "jellymesh.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := NewPeerRepository(database).SetBlocked(ctx, "stranger", true); err != nil {
		t.Fatalf("block unseen peer: %v", err)
	}
	database.Close()

	database, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer database.Close()
	repo := NewPeerRepository(database)
	if blocked, err := repo.IsBlocked(ctx, "stranger"); err != nil || !blocked {
		t.Fatalf("block did not survive restart: blocked=%v err=%v", blocked, err)
	}
	if err := repo.Upsert(ctx, samplePeer("stranger", "fingerprint-stranger")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got, _, _ := repo.ByNodeID(ctx, "stranger"); !got.Blocked {
		t.Fatal("a block recorded before first contact must apply once the peer appears")
	}
	if err := repo.Remove(ctx, "stranger"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if blocked, _ := repo.IsBlocked(ctx, "stranger"); !blocked {
		t.Fatal("removing a peer record must not lift its block")
	}
}

func TestSetBlockedAndRemove(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	if err := repo.Upsert(ctx, samplePeer("cedar", "fingerprint-cedar")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.SetBlocked(ctx, "cedar", true); err != nil {
		t.Fatalf("set blocked: %v", err)
	}
	if blocked, _ := repo.IsBlocked(ctx, "cedar"); !blocked {
		t.Fatal("expected blocked")
	}
	if err := repo.SetBlocked(ctx, "cedar", false); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if blocked, _ := repo.IsBlocked(ctx, "cedar"); blocked {
		t.Fatal("expected unblocked")
	}
	if err := repo.SetBlocked(ctx, "  ", true); !errors.Is(err, ErrNodeIDRequired) {
		t.Fatalf("blank node: error = %v, want ErrNodeIDRequired", err)
	}

	if err := repo.Remove(ctx, "cedar"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, found, _ := repo.ByNodeID(ctx, "cedar"); found {
		t.Fatal("expected peer to be gone after remove")
	}
	if err := repo.Remove(ctx, "cedar"); !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("remove missing node: error = %v, want ErrPeerNotFound", err)
	}
}

func TestPeerListReturnsAllPeers(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	if err := repo.Upsert(ctx, samplePeer("cedar", "fp-cedar")); err != nil {
		t.Fatalf("upsert cedar: %v", err)
	}
	if err := repo.Upsert(ctx, samplePeer("walnut", "fp-walnut")); err != nil {
		t.Fatalf("upsert walnut: %v", err)
	}

	peers, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}
	if peers[0].NodeID != "cedar" || peers[1].NodeID != "walnut" {
		t.Fatalf("unexpected peers: %+v", peers)
	}
}
