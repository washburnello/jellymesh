package membership

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/events"
	"jellymesh/internal/group"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/store"
	"jellymesh/internal/transport"
)

// fakeDirectory stands in for the peer repository so the authorization path can
// be exercised without a database.
type fakeDirectory struct {
	peers map[string]store.Peer
	err   error
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{peers: map[string]store.Peer{}}
}

func (directory *fakeDirectory) ByNodeID(_ context.Context, nodeID string) (store.Peer, bool, error) {
	if directory.err != nil {
		return store.Peer{}, false, directory.err
	}
	peer, found := directory.peers[nodeID]
	return peer, found, nil
}

func (directory *fakeDirectory) SetTrusted(_ context.Context, nodeID string, trusted bool) error {
	if directory.err != nil {
		return directory.err
	}
	peer, found := directory.peers[nodeID]
	if !found {
		return errors.New("no such peer")
	}
	peer.Trusted = trusted
	directory.peers[nodeID] = peer
	return nil
}

func newIdentity(t *testing.T, hostname string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	identity, err := node.LoadOrCreate(
		filepath.Join(directory, "node.key"),
		filepath.Join(directory, "node.crt"),
		hostname,
	)
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	return identity
}

func publicKeyOf(t *testing.T, identity *node.Identity) ed25519.PublicKey {
	t.Helper()
	return identity.PublicKey()
}

// fixture builds a group where cedar owns, walnut is an administrator, and
// maple is an ordinary member, with peer rows and identities for each.
func fixture(t *testing.T) (*Applier, *fakeDirectory, map[string]*node.Identity) {
	t.Helper()
	state, err := policy.NewState("group-1", "cedar")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	identities := map[string]*node.Identity{}
	directory := newFakeDirectory()
	for _, name := range []string{"cedar", "walnut", "maple"} {
		identity := newIdentity(t, name+".example.org")
		identities[name] = identity
		directory.peers[name] = store.Peer{
			NodeID:      name,
			Fingerprint: transport.Fingerprint(identity.Fingerprint()),
			Trusted:     true,
		}
	}
	for _, name := range []string{"walnut", "maple"} {
		if err := state.PublishCandidate(policy.Publication{
			GroupID: "group-1", SourceNodeID: name,
			Library: policy.Library{ID: "movies", Name: "Movies", CollectionType: "movies"},
		}); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
		state.Roles.Members[name] = true
	}
	if err := state.Roles.PromoteAdmin("cedar", "walnut"); err != nil {
		t.Fatalf("promote walnut: %v", err)
	}
	state.MembershipSequence = 5
	return NewApplier(state, directory), directory, identities
}

func revocationEnvelope(t *testing.T, issuer *node.Identity, issuerID string, target string, sequence uint64) events.Envelope {
	t.Helper()
	envelope, err := events.Sign(issuer, events.KindRevocation, "group-1", issuerID, sequence, time.Now().UTC(),
		policy.Revocation{GroupID: "group-1", MemberID: target, Sequence: sequence, IssuedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	return envelope
}

// C-PO-10: a correctly signed event from an ordinary member must be refused.
// The signature is valid; only the role check can reject it.
func TestCorrectlySignedEventFromOrdinaryMemberIsRefused(t *testing.T) {
	applier, _, identities := fixture(t)
	envelope := revocationEnvelope(t, identities["maple"], "maple", "walnut", 6)

	err := applier.Apply(context.Background(), envelope, publicKeyOf(t, identities["maple"]))
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("an ordinary member must not issue membership events, got %v", err)
	}
	if !applier.State().IsMember("walnut") {
		t.Fatal("the refused event must not have been applied")
	}
}

func TestAdministratorMayIssueMembershipEvents(t *testing.T) {
	applier, directory, identities := fixture(t)
	envelope := revocationEnvelope(t, identities["walnut"], "walnut", "maple", 6)

	if err := applier.Apply(context.Background(), envelope, publicKeyOf(t, identities["walnut"])); err != nil {
		t.Fatalf("an administrator should be able to revoke an ordinary member: %v", err)
	}
	if applier.State().IsMember("maple") {
		t.Fatal("the revoked member is still in the roster")
	}
	// C-OP-3: the revocation must also have cut transport trust.
	if directory.peers["maple"].Trusted {
		t.Fatal("a revoked member must not remain trusted at the transport layer")
	}
}

// C-PO-10 second half: an envelope naming an administrator as issuer while
// signed by somebody else must be refused even though the signature verifies
// against the key the caller supplied.
func TestIssuerNameCannotBeBorrowedFromAnAdministrator(t *testing.T) {
	applier, _, identities := fixture(t)
	// maple signs, but claims to be walnut.
	envelope, err := events.Sign(identities["maple"], events.KindRevocation, "group-1", "walnut", 6,
		time.Now().UTC(), policy.Revocation{GroupID: "group-1", MemberID: "cedar", Sequence: 6})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	err = applier.Apply(context.Background(), envelope, publicKeyOf(t, identities["maple"]))
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a borrowed issuer name must be refused, got %v", err)
	}
}

// C-OP-3: ejection and transport revocation must happen together.
func TestEjectionRevokesTransportTrust(t *testing.T) {
	applier, directory, _ := fixture(t)
	if !directory.peers["maple"].Trusted {
		t.Fatal("precondition: maple should start trusted")
	}
	if err := applier.Eject(context.Background(), "cedar", "maple"); err != nil {
		t.Fatalf("eject: %v", err)
	}
	if !applier.State().IsEjected("maple") {
		t.Fatal("maple should be ejected from the roster")
	}
	if directory.peers["maple"].Trusted {
		t.Fatal("an ejected member must not remain trusted at the transport layer")
	}
}

func TestEjectionByANonAdministratorChangesNothing(t *testing.T) {
	applier, directory, _ := fixture(t)
	if err := applier.Eject(context.Background(), "maple", "walnut"); err == nil {
		t.Fatal("an ordinary member must not eject anyone")
	}
	if !directory.peers["walnut"].Trusted {
		t.Fatal("a refused ejection must not touch transport trust")
	}
}

// C-PO-11: a restarted node must not accept a sequence it already superseded.
func TestReplayGuardIsSeededFromDurableSequence(t *testing.T) {
	applier, _, identities := fixture(t)
	// The fixture persisted MembershipSequence 5, so 4 is already superseded.
	stale := revocationEnvelope(t, identities["walnut"], "walnut", "maple", 4)
	err := applier.Apply(context.Background(), stale, publicKeyOf(t, identities["walnut"]))
	if !errors.Is(err, events.ErrStaleSequence) {
		t.Fatalf("a superseded sequence must be refused after a restart, got %v", err)
	}
	if applier.State().IsEjected("maple") {
		t.Fatal("the stale event must not have been applied")
	}
}

func TestSeedNeverLowersTheHighWaterMark(t *testing.T) {
	guard := events.NewSequenceGuard()
	guard.Seed("group-1", 10)
	guard.Seed("group-1", 3)
	if err := guard.Admit("group-1", 5); !errors.Is(err, events.ErrStaleSequence) {
		t.Fatalf("seeding lower must not weaken the guard, got %v", err)
	}
	if err := guard.Admit("group-1", 11); err != nil {
		t.Fatalf("a monotonic increase should still be admitted: %v", err)
	}
}

func TestEventForAnotherGroupIsRefused(t *testing.T) {
	applier, _, identities := fixture(t)
	envelope, err := events.Sign(identities["walnut"], events.KindRevocation, "group-2", "walnut", 6,
		time.Now().UTC(), policy.Revocation{GroupID: "group-2", MemberID: "maple", Sequence: 6})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := applier.Apply(context.Background(), envelope, publicKeyOf(t, identities["walnut"])); !errors.Is(err, ErrGroupMismatch) {
		t.Fatalf("an event for another group must be refused, got %v", err)
	}
}

func TestUnknownIssuerIsRefused(t *testing.T) {
	applier, directory, identities := fixture(t)
	delete(directory.peers, "walnut")
	envelope := revocationEnvelope(t, identities["walnut"], "walnut", "maple", 6)
	if err := applier.Apply(context.Background(), envelope, publicKeyOf(t, identities["walnut"])); !errors.Is(err, ErrIssuerUnknown) {
		t.Fatalf("an issuer with no peer record must be refused, got %v", err)
	}
}

func TestTamperedEventIsRefusedBeforeAnyRoleCheck(t *testing.T) {
	applier, _, identities := fixture(t)
	envelope := revocationEnvelope(t, identities["walnut"], "walnut", "maple", 6)
	envelope.Sequence = 7
	if err := applier.Apply(context.Background(), envelope, publicKeyOf(t, identities["walnut"])); err == nil {
		t.Fatal("a tampered envelope must be refused")
	}
	if applier.State().IsEjected("maple") {
		t.Fatal("a tampered event must not have been applied")
	}
}

// C-PO-4 through the full event path: a correctly signed revocation of one
// administrator by another is refused, and the target keeps transport trust.
func TestSignedRevocationOfAnAdministratorByAnAdministratorIsRefused(t *testing.T) {
	applier, directory, identities := fixture(t)
	if err := applier.State().Roles.PromoteAdmin("cedar", "maple"); err != nil {
		t.Fatalf("promote maple: %v", err)
	}
	envelope := revocationEnvelope(t, identities["walnut"], "walnut", "maple", 6)

	err := applier.Apply(context.Background(), envelope, publicKeyOf(t, identities["walnut"]))
	if !errors.Is(err, group.ErrAdminProtected) {
		t.Fatalf("error = %v, want ErrAdminProtected", err)
	}
	if !applier.State().IsAdministrator("maple") {
		t.Fatal("the target administrator must remain in place")
	}
	if !directory.peers["maple"].Trusted {
		t.Fatal("a refused revocation must not revoke transport trust")
	}
}
