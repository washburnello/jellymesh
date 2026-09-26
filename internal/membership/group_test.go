package membership

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/audit"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/node"
	"jellymesh/internal/store"
	"jellymesh/internal/transport"
)

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newIdentity(t *testing.T, name string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	identity, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return identity
}

type harness struct {
	identities map[string]*node.Identity
	path       string
	database   *store.DB
	peers      *store.PeerRepository
	group      *Group
}

// newHarness founds group-1 owned by cedar and admits walnut and maple,
// storing everything in a real SQLite database.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{identities: map[string]*node.Identity{}, path: filepath.Join(t.TempDir(), "state", "jellymesh.db")}
	for _, name := range []string{"cedar", "walnut", "maple", "outsider"} {
		h.identities[name] = newIdentity(t, name)
	}
	h.open(t)
	group, err := Found(context.Background(), store.NewGroupLogRepository(h.database), h.peers,
		h.identities["cedar"], "group-1", "cedar", "Cedar", "cedar.example.org", now)
	if err != nil {
		t.Fatalf("found: %v", err)
	}
	h.group = group
	h.admit(t, "walnut")
	h.admit(t, "maple")
	return h
}

func (h *harness) open(t *testing.T) {
	t.Helper()
	database, err := store.Open(h.path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	h.database = database
	h.peers = store.NewPeerRepository(database)
}

func (h *harness) sequence(t *testing.T, kind grouplog.Kind, body any) grouplog.Event {
	t.Helper()
	proposal, err := grouplog.NewProposal(h.identities["cedar"], "group-1", "cedar", kind, body, now)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	event, err := h.group.Sequence(context.Background(), h.identities["cedar"], proposal, now)
	if err != nil {
		t.Fatalf("sequence %s: %v", kind, err)
	}
	return event
}

func (h *harness) admit(t *testing.T, name string) grouplog.Event {
	t.Helper()
	return h.sequence(t, grouplog.KindAdmission, grouplog.AdmissionBody{
		MemberID: name, MemberKey: h.identities[name].PublicKey(), InvitationID: "invite-" + name, InviterID: "cedar",
	})
}

// handshake runs a real mutual-TLS handshake with server authorizing by
// trust, and reports whether the server accepted client.
func handshake(t *testing.T, server *node.Identity, trust transport.TrustStore, client *node.Identity) bool {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer raw.Close()
		conn := tlsServer(raw, server, trust)
		accepted <- conn.Handshake()
	}()

	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	conn := tlsClient(raw, client, server.Fingerprint())
	// Under TLS 1.3 the client's handshake can succeed even when the server
	// refuses it (C-TR-5), so the server's result is the one that counts.
	_ = conn.Handshake()

	select {
	case err := <-accepted:
		return err == nil
	case <-time.After(5 * time.Second):
		t.Fatal("handshake timed out")
		return false
	}
}

// C-TR-9: a member's key is trusted because the roster says so, with no
// separate trust record written anywhere.
func TestMembersAreTrustedAndOutsidersAreNot(t *testing.T) {
	h := newHarness(t)
	server := h.identities["cedar"]
	if !handshake(t, server, h.group, h.identities["walnut"]) {
		t.Fatal("an admitted member should complete a handshake")
	}
	if handshake(t, server, h.group, h.identities["outsider"]) {
		t.Fatal("a key outside the roster must be refused")
	}
	if peers, _ := h.peers.List(context.Background()); len(peers) != 0 {
		t.Fatal("trust must not depend on, or create, peer directory records")
	}
}

// C-TR-9 and C-OP-3: ejection revokes transport trust on every node that
// applies it, including a node that only receives the event.
func TestEjectionRevokesTrustOnEveryNode(t *testing.T) {
	h := newHarness(t)

	followerDatabase, err := store.Open(filepath.Join(t.TempDir(), "follower.db"))
	if err != nil {
		t.Fatalf("open follower: %v", err)
	}
	defer followerDatabase.Close()
	follower, err := Join(context.Background(), store.NewGroupLogRepository(followerDatabase), store.NewPeerRepository(followerDatabase),
		h.group.EventsAfter(0), h.group.EventsAfter(0)[0].Hash())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	walnutServer := h.identities["walnut"]
	if !handshake(t, walnutServer, follower, h.identities["maple"]) {
		t.Fatal("maple should be trusted before ejection")
	}

	ejection := h.sequence(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "maple"})
	if handshake(t, h.identities["cedar"], h.group, h.identities["maple"]) {
		t.Fatal("the owner's node must refuse maple after ejecting it")
	}
	if _, err := follower.Receive(context.Background(), ejection); err != nil {
		t.Fatalf("follower receives ejection: %v", err)
	}
	if handshake(t, walnutServer, follower, h.identities["maple"]) {
		t.Fatal("a node that received the ejection must refuse maple")
	}
}

// C-TR-6: authorization is durable, because the roster is rebuilt from the
// stored log on restart.
func TestTrustSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.database.Close()
	h.open(t)
	reopened, err := Open(context.Background(), store.NewGroupLogRepository(h.database), h.peers, "group-1")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !handshake(t, h.identities["cedar"], reopened, h.identities["walnut"]) {
		t.Fatal("a member should still be trusted after a restart")
	}
	if handshake(t, h.identities["cedar"], reopened, h.identities["outsider"]) {
		t.Fatal("an outsider should still be refused after a restart")
	}
}

// C-BL-1: a blocked member is refused even though it remains in the roster.
func TestABlockedMemberIsRefused(t *testing.T) {
	h := newHarness(t)
	if err := h.peers.SetBlocked(context.Background(), "walnut", true); err != nil {
		t.Fatalf("block: %v", err)
	}
	if handshake(t, h.identities["cedar"], h.group, h.identities["walnut"]) {
		t.Fatal("a blocked member must be refused")
	}
	var stillMember bool
	h.group.View(func(state *grouplog.State) { stillMember = state.IsMember("walnut") })
	if !stillMember {
		t.Fatal("a block is a media cut and leaves the member in the roster")
	}
	if err := h.peers.SetBlocked(context.Background(), "walnut", false); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if !h.group.IsTrusted(h.identities["walnut"].Fingerprint()) {
		t.Fatal("unblocking restores trust")
	}
}

// C-BL-2 and C-BL-3: trust fails closed.
func TestTrustFailsClosed(t *testing.T) {
	h := newHarness(t)
	if h.group.IsTrusted("") {
		t.Fatal("an empty fingerprint must never be trusted")
	}
	var nilGroup *Group
	if nilGroup.IsTrusted(h.identities["walnut"].Fingerprint()) {
		t.Fatal("a nil group trusts nobody")
	}
	// A block lookup that fails must refuse, not admit.
	h.database.Close()
	if h.group.IsTrusted(h.identities["walnut"].Fingerprint()) {
		t.Fatal("trust must fail closed when the block list cannot be read")
	}
}

// An equivocating owner leaves the node unsure which roster is true, so it
// trusts nobody.
func TestAHaltedGroupTrustsNobody(t *testing.T) {
	h := newHarness(t)
	events := h.group.EventsAfter(0)
	rival, err := grouplog.Replay(events)
	if err != nil {
		t.Fatalf("rival: %v", err)
	}
	h.sequence(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "maple"})
	proposal, _ := grouplog.NewProposal(h.identities["cedar"], "group-1", "cedar", grouplog.KindEjection, grouplog.MemberBody{MemberID: "walnut"}, now)
	second, err := rival.Sequence(h.identities["cedar"], proposal, now)
	if err != nil {
		t.Fatalf("rival sequence: %v", err)
	}
	if _, err := h.group.Receive(context.Background(), second); !errors.Is(err, grouplog.ErrEquivocation) {
		t.Fatalf("error = %v, want ErrEquivocation", err)
	}
	if h.group.IsTrusted(h.identities["walnut"].Fingerprint()) {
		t.Fatal("a halted group must trust nobody")
	}
	// And that survives a restart.
	reopened, err := Open(context.Background(), store.NewGroupLogRepository(h.database), h.peers, "group-1")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.IsTrusted(h.identities["walnut"].Fingerprint()) {
		t.Fatal("a halted group must still trust nobody after a restart")
	}
}

// failingStore stores until told to fail.
type failingStore struct {
	inner LogStore
	fail  bool
}

func (s *failingStore) Load(ctx context.Context, groupID string) (*grouplog.Log, bool, error) {
	return s.inner.Load(ctx, groupID)
}

func (s *failingStore) Save(ctx context.Context, log *grouplog.Log) error {
	if s.fail {
		return errors.New("disk full")
	}
	return s.inner.Save(ctx, log)
}

// An owner never keeps an event it could not store, so it can never later
// sequence a different event for the same slot.
func TestAnEventThatCannotBeStoredIsDiscarded(t *testing.T) {
	h := newHarness(t)
	flaky := &failingStore{inner: store.NewGroupLogRepository(h.database)}
	group, err := Open(context.Background(), flaky, h.peers, "group-1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	before := group.Head()

	flaky.fail = true
	proposal, _ := grouplog.NewProposal(h.identities["cedar"], "group-1", "cedar", grouplog.KindEjection, grouplog.MemberBody{MemberID: "maple"}, now)
	if _, err := group.Sequence(context.Background(), h.identities["cedar"], proposal, now); err == nil {
		t.Fatal("sequencing must fail when the event cannot be stored")
	}
	if group.Head() != before {
		t.Fatal("an event that could not be stored must not remain in memory")
	}
	if !group.IsTrusted(h.identities["maple"].Fingerprint()) {
		t.Fatal("the discarded ejection must have no effect")
	}

	flaky.fail = false
	if _, err := group.Sequence(context.Background(), h.identities["cedar"], proposal, now); err != nil {
		t.Fatalf("retry after the store recovers: %v", err)
	}
}

func tlsServer(raw net.Conn, identity *node.Identity, trust transport.TrustStore) *tls.Conn {
	return tls.Server(raw, transport.ServerTLSConfig(identity.TLSCertificate(), trust))
}

// tlsClient dials as identity. The client trusts the server it expects, so
// that only the server's decision is under test.
func tlsClient(raw net.Conn, identity *node.Identity, expected node.Fingerprint) *tls.Conn {
	return tls.Client(raw, transport.ClientTLSConfig(identity.TLSCertificate(), expected, transport.NewMemoryTrustStore(expected)))
}

// C-PO-11: replay protection is structural and durable. After a restart, an
// old event presented again changes nothing.
func TestAnOldEventReplayedAfterRestartChangesNothing(t *testing.T) {
	h := newHarness(t)
	admission := h.group.EventsAfter(0)[1]
	h.sequence(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "walnut"})
	h.database.Close()
	h.open(t)

	reopened, err := Open(context.Background(), store.NewGroupLogRepository(h.database), h.peers, "group-1")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	head := reopened.Head()
	outcome, err := reopened.Receive(context.Background(), admission)
	if err != nil || outcome != grouplog.Duplicate {
		t.Fatalf("replayed admission: %v, %v; want Duplicate", outcome, err)
	}
	if reopened.Head() != head || reopened.IsTrusted(h.identities["walnut"].Fingerprint()) {
		t.Fatal("a replayed admission must not readmit an ejected member")
	}
}

// C-OP-1: changes to the group log are audited, including equivocation.
func TestGroupChangesAreAudited(t *testing.T) {
	h := newHarness(t)
	h.group.SetAudit(&audit.Log{Sink: store.NewAuditRepository(h.database)})
	rival, err := grouplog.Replay(h.group.EventsAfter(0))
	if err != nil {
		t.Fatalf("rival: %v", err)
	}
	h.sequence(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "maple"})
	proposal, _ := grouplog.NewProposal(h.identities["cedar"], "group-1", "cedar", grouplog.KindEjection, grouplog.MemberBody{MemberID: "walnut"}, now)
	second, _ := rival.Sequence(h.identities["cedar"], proposal, now)
	h.group.Receive(context.Background(), second)

	events, err := store.NewAuditRepository(h.database).List(context.Background(), 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events: %+v, %v", events, err)
	}
	if events[1].Action != "group.event_sequenced" || events[1].Detail["kind"] != "ejection" {
		t.Fatalf("first event = %+v", events[1])
	}
	if events[0].Action != "group.equivocation" {
		t.Fatalf("second event = %+v", events[0])
	}
}
