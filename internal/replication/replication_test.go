package replication

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/group"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/store"
	"jellymesh/internal/transport"
)

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// member is one node: its identity, its own database, and its copy of the log.
type member struct {
	name     string
	identity *node.Identity
	group    *membership.Group
	server   *Server
	address  string
}

type mesh struct {
	t       *testing.T
	members map[string]*member
	groupID string
}

func newIdentity(t *testing.T, name string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	identity, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
	if err != nil {
		t.Fatalf("identity %s: %v", name, err)
	}
	return identity
}

func openStores(t *testing.T) (*store.GroupLogRepository, *store.PeerRepository) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "state", "jellymesh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return store.NewGroupLogRepository(database), store.NewPeerRepository(database)
}

// newMesh founds group-1 owned by cedar, admits the other names, promotes
// walnut, and gives every member its own node with a copy of the log and a
// running replication server.
func newMesh(t *testing.T, names ...string) *mesh {
	t.Helper()
	m := &mesh{t: t, members: map[string]*member{}, groupID: "group-1"}
	identities := map[string]*node.Identity{"cedar": newIdentity(t, "cedar")}
	for _, name := range names {
		identities[name] = newIdentity(t, name)
	}

	logs, peers := openStores(t)
	owner, err := membership.Found(context.Background(), logs, peers, identities["cedar"], "group-1", "cedar", "Cedar", "cedar.example.org", now)
	if err != nil {
		t.Fatalf("found: %v", err)
	}
	m.members["cedar"] = &member{name: "cedar", identity: identities["cedar"], group: owner}
	for _, name := range names {
		m.sequence("cedar", grouplog.KindAdmission, grouplog.AdmissionBody{
			MemberID: name, MemberKey: identities[name].PublicKey(), InvitationID: "invite-" + name, InviterID: "cedar",
		})
	}
	m.sequence("cedar", grouplog.KindPromote, grouplog.MemberBody{MemberID: "walnut"})

	events := owner.EventsAfter(0)
	for _, name := range names {
		logs, peers := openStores(t)
		joined, err := membership.Join(context.Background(), logs, peers, events, events[0].Hash())
		if err != nil {
			t.Fatalf("join %s: %v", name, err)
		}
		m.members[name] = &member{name: name, identity: identities[name], group: joined}
	}
	for _, each := range m.members {
		m.serve(each)
	}
	return m
}

// serve starts a loopback replication server for a member.
func (m *mesh) serve(each *member) {
	m.t.Helper()
	each.server = NewServer(each.identity)
	each.server.Add(m.groupID, each.group)
	each.address = startTLS(m.t, each.identity, each.server, each.server.Handler())
}

func startTLS(t *testing.T, identity *node.Identity, trust transport.TrustStore, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(tls.NewListener(listener, transport.ServerTLSConfig(identity.TLSCertificate(), trust)))
	t.Cleanup(func() { server.Close() })
	return listener.Addr().String()
}

func (m *mesh) sequence(proposer string, kind grouplog.Kind, body any) grouplog.Event {
	m.t.Helper()
	owner := m.members["cedar"]
	proposal, err := grouplog.NewProposal(owner.identity, m.groupID, proposer, kind, body, now)
	if err != nil {
		m.t.Fatalf("proposal: %v", err)
	}
	event, err := owner.group.Sequence(context.Background(), owner.identity, proposal, now)
	if err != nil {
		m.t.Fatalf("sequence %s: %v", kind, err)
	}
	return event
}

func (m *mesh) peer(name string) Peer {
	return Peer{Address: m.members[name].address, Fingerprint: m.members[name].identity.Fingerprint()}
}

func (m *mesh) client(name string) *Client {
	return NewClient(m.members[name].identity, m.members[name].group)
}

func (m *mesh) sync(from string, to string, pageSize int) (Result, error) {
	return Sync(context.Background(), m.client(to), m.peer(from), m.members[to].group, m.groupID, pageSize)
}

// C-PO-21: a member that is behind fetches what it is missing, from the owner
// or from any other member.
func TestAMemberCatchesUpFromAnyMember(t *testing.T) {
	m := newMesh(t, "walnut", "maple", "birch")
	m.sequence("cedar", grouplog.KindEjection, grouplog.MemberBody{MemberID: "birch"})
	m.sequence("cedar", grouplog.KindPromote, grouplog.MemberBody{MemberID: "maple"})

	result, err := m.sync("cedar", "walnut", 0)
	if err != nil || result.Applied != 2 {
		t.Fatalf("walnut from the owner: %+v, %v", result, err)
	}
	// maple never talks to the owner.
	result, err = m.sync("walnut", "maple", 0)
	if err != nil || result.Applied != 2 {
		t.Fatalf("maple from walnut: %+v, %v", result, err)
	}
	if m.members["maple"].group.Head() != m.members["cedar"].group.Head() {
		t.Fatal("maple should converge on the owner's log")
	}
	if result, err := m.sync("walnut", "maple", 0); err != nil || result.Applied != 0 {
		t.Fatalf("a second sync should find nothing to do: %+v, %v", result, err)
	}
}

func TestSyncPagesThroughALongLog(t *testing.T) {
	m := newMesh(t, "walnut", "maple")
	for index := 0; index < 5; index++ {
		m.sequence("cedar", grouplog.KindPromote, grouplog.MemberBody{MemberID: "maple"})
		m.sequence("cedar", grouplog.KindDemote, grouplog.MemberBody{MemberID: "maple"})
	}
	result, err := m.sync("cedar", "maple", 3)
	if err != nil || result.Applied != 10 {
		t.Fatalf("paged sync: %+v, %v", result, err)
	}
	if m.members["maple"].group.Head() != m.members["cedar"].group.Head() {
		t.Fatal("paged sync should converge")
	}
}

// C-PO-15 over the wire: a member that applied the former owner's events
// converges on the succession by syncing from any member of the new epoch.
func TestAMinorityConvergesOnASuccessionBySync(t *testing.T) {
	m := newMesh(t, "walnut", "maple", "birch")
	var base *grouplog.State
	m.members["birch"].group.View(func(state *grouplog.State) { base = state })
	attestation, err := grouplog.Attest(m.members["birch"].identity, "birch", base, now, now.Add(group.OwnerSuccessionTimeout+time.Hour))
	if err != nil {
		t.Fatalf("attest: %v", err)
	}

	// Partitioned, the old owner sequences once more and only maple sees it.
	stale := m.sequence("cedar", grouplog.KindEjection, grouplog.MemberBody{MemberID: "birch"})
	if _, err := m.members["maple"].group.Receive(context.Background(), stale); err != nil {
		t.Fatalf("maple applies the stale event: %v", err)
	}

	walnut := m.members["walnut"]
	if _, err := walnut.group.Claim(context.Background(), walnut.identity, "walnut", []grouplog.Attestation{attestation}, now); err != nil {
		t.Fatalf("claim: %v", err)
	}

	result, err := m.sync("walnut", "maple", 2)
	if err != nil || !result.Superseded {
		t.Fatalf("maple syncs from the new owner: %+v, %v", result, err)
	}
	if m.members["maple"].group.Head() != walnut.group.Head() {
		t.Fatal("maple should converge on the new epoch")
	}
	// The former owner, now fenced, cannot pull maple back.
	if result, err := m.sync("cedar", "maple", 0); err != nil || result.Applied != 0 {
		t.Fatalf("sync from the fenced owner: %+v, %v", result, err)
	}
}

// C-PO-19 over the wire: a peer whose log differs at the same height exposes
// equivocation, and the syncing node halts.
func TestSyncDetectsEquivocation(t *testing.T) {
	m := newMesh(t, "walnut", "maple")
	owner := m.members["cedar"]
	rival, err := grouplog.Replay(owner.group.EventsAfter(0))
	if err != nil {
		t.Fatalf("rival: %v", err)
	}
	first := m.sequence("cedar", grouplog.KindDemote, grouplog.MemberBody{MemberID: "walnut"})
	proposal, _ := grouplog.NewProposal(owner.identity, "group-1", "cedar", grouplog.KindPromote, grouplog.MemberBody{MemberID: "maple"}, now)
	second, err := rival.Sequence(owner.identity, proposal, now)
	if err != nil {
		t.Fatalf("rival sequence: %v", err)
	}
	if _, err := m.members["walnut"].group.Receive(context.Background(), first); err != nil {
		t.Fatalf("walnut: %v", err)
	}
	if _, err := m.members["maple"].group.Receive(context.Background(), second); err != nil {
		t.Fatalf("maple: %v", err)
	}

	if _, err := m.sync("maple", "walnut", 0); !errors.Is(err, grouplog.ErrEquivocation) {
		t.Fatalf("error = %v, want ErrEquivocation", err)
	}
	if m.members["walnut"].group.IsTrusted(m.members["maple"].identity.Fingerprint()) {
		t.Fatal("a node that detected equivocation must stop trusting")
	}
}

// C-TR-8: a peer that refuses this node is reported as a refusal, distinct from
// a peer that cannot be reached, even though under TLS 1.3 the client's own
// handshake reports success.
func TestRefusalIsDistinguishedFromUnreachability(t *testing.T) {
	m := newMesh(t, "walnut")
	outsider := newIdentity(t, "outsider")
	cedar := m.members["cedar"]
	client := NewClient(outsider, transport.NewMemoryTrustStore(cedar.identity.Fingerprint()))

	if _, err := client.Head(context.Background(), m.peer("cedar"), "group-1"); !errors.Is(err, ErrRefusedByPeer) {
		t.Fatalf("refused: error = %v, want ErrRefusedByPeer", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closed := listener.Addr().String()
	listener.Close()
	if _, err := client.Head(context.Background(), Peer{Address: closed, Fingerprint: cedar.identity.Fingerprint()}, "group-1"); !errors.Is(err, ErrPeerUnreachable) {
		t.Fatalf("unreachable: error = %v, want ErrPeerUnreachable", err)
	}
}

// A member of one group cannot read another group's log from a node that
// serves both.
func TestAMemberOfOneGroupCannotReadAnother(t *testing.T) {
	m := newMesh(t, "walnut")
	cedar := m.members["cedar"]
	spruce := newIdentity(t, "spruce")
	logs, peers := openStores(t)
	other, err := membership.Found(context.Background(), logs, peers, cedar.identity, "group-2", "cedar", "Cedar", "cedar.example.org", now)
	if err != nil {
		t.Fatalf("found group-2: %v", err)
	}
	proposal, _ := grouplog.NewProposal(cedar.identity, "group-2", "cedar", grouplog.KindAdmission,
		grouplog.AdmissionBody{MemberID: "spruce", MemberKey: spruce.PublicKey(), InvitationID: "invite-spruce", InviterID: "cedar"}, now)
	if _, err := other.Sequence(context.Background(), cedar.identity, proposal, now); err != nil {
		t.Fatalf("admit spruce: %v", err)
	}
	cedar.server.Add("group-2", other)

	walnutClient := m.client("walnut")
	if _, err := walnutClient.Head(context.Background(), m.peer("cedar"), "group-2"); !errors.Is(err, ErrNotServed) {
		t.Fatalf("walnut reading group-2: error = %v, want ErrNotServed", err)
	}
	spruceClient := NewClient(spruce, other)
	if _, err := spruceClient.Head(context.Background(), m.peer("cedar"), "group-2"); err != nil {
		t.Fatalf("spruce reading its own group: %v", err)
	}
	if _, err := spruceClient.Head(context.Background(), m.peer("cedar"), "group-1"); !errors.Is(err, ErrNotServed) {
		t.Fatalf("spruce reading group-1: error = %v, want ErrNotServed", err)
	}
	intrusion, _ := grouplog.NewProposal(spruce, "group-1", "spruce", grouplog.KindLeave, grouplog.MemberBody{MemberID: "spruce"}, now)
	if _, err := spruceClient.Submit(context.Background(), m.peer("cedar"), "group-1", intrusion); !errors.Is(err, ErrNotServed) {
		t.Fatalf("spruce submitting to group-1: error = %v, want ErrNotServed", err)
	}
}

// A member that serves forged or out-of-order events cannot change the log of
// the node syncing from it.
func TestAPeerServingForgedEventsCannotCorruptTheLog(t *testing.T) {
	m := newMesh(t, "walnut", "maple")
	genuine := m.sequence("cedar", grouplog.KindPromote, grouplog.MemberBody{MemberID: "maple"})
	walnut := m.members["walnut"]
	before := m.members["maple"].group.Head()

	forged := genuine
	forged.Payload = append([]byte(nil), genuine.Payload...)
	forged.Payload[len(forged.Payload)-2] ^= 1
	skipped := genuine
	skipped.Sequence++

	for name, served := range map[string]grouplog.Event{"tampered": forged, "out of order": skipped} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET "+headPath, func(response http.ResponseWriter, _ *http.Request) {
				writeJSON(response, served.Head())
			})
			mux.HandleFunc("GET "+eventsPath, func(response http.ResponseWriter, _ *http.Request) {
				writeJSON(response, []grouplog.Event{served})
			})
			// The hostile server holds walnut's genuine key, so it is a
			// trusted member in every respect except the events it serves.
			address := startTLS(t, walnut.identity, m.members["maple"].group, mux)
			peer := Peer{Address: address, Fingerprint: walnut.identity.Fingerprint()}
			if _, err := Sync(context.Background(), m.client("maple"), peer, m.members["maple"].group, "group-1", 0); err == nil {
				t.Fatal("syncing forged events must fail")
			}
			if m.members["maple"].group.Head() != before {
				t.Fatal("forged events must not change the log")
			}
		})
	}
	var decoded grouplog.Head
	if err := json.Unmarshal([]byte(`{"epoch":1,"sequence":1,"hash":"00"}`), &decoded); err == nil {
		t.Fatal("a malformed hash must not decode")
	}
}

func (m *mesh) signed(proposer string, kind grouplog.Kind, body any) grouplog.Proposal {
	m.t.Helper()
	proposal, err := grouplog.NewProposal(m.members[proposer].identity, m.groupID, proposer, kind, body, now)
	if err != nil {
		m.t.Fatalf("proposal: %v", err)
	}
	return proposal
}

// C-PO-23: an administrator's decision reaches the log by submitting its
// signed proposal to the owner's node, which sequences it; any member may
// relay it, and it then replicates like any other event.
func TestAnAdministratorsProposalIsSequencedByTheOwner(t *testing.T) {
	m := newMesh(t, "walnut", "maple", "birch")
	proposal := m.signed("walnut", grouplog.KindEjection, grouplog.MemberBody{MemberID: "birch"})

	event, err := m.client("walnut").Submit(context.Background(), m.peer("cedar"), "group-1", proposal)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if event.Sequence != m.members["cedar"].group.Head().Sequence {
		t.Fatal("the returned event should be the owner's new head")
	}
	if m.members["cedar"].group.IsTrusted(m.members["birch"].identity.Fingerprint()) {
		t.Fatal("the owner should have applied the ejection")
	}
	if result, err := m.sync("cedar", "maple", 0); err != nil || result.Applied != 1 {
		t.Fatalf("the sequenced proposal should replicate: %+v, %v", result, err)
	}

	// A member may relay an administrator's proposal; its signature is what
	// authorizes it.
	relayed := m.signed("walnut", grouplog.KindDemote, grouplog.MemberBody{MemberID: "walnut"})
	if _, err := m.client("maple").Submit(context.Background(), m.peer("cedar"), "group-1", relayed); !errors.Is(err, ErrProposalRefused) {
		t.Fatalf("an administrator may not demote itself; error = %v, want ErrProposalRefused", err)
	}
	promote := m.signed("cedar", grouplog.KindPromote, grouplog.MemberBody{MemberID: "maple"})
	if _, err := m.client("maple").Submit(context.Background(), m.peer("cedar"), "group-1", promote); err != nil {
		t.Fatalf("a relayed owner proposal: %v", err)
	}
}

// C-PO-23: the rules apply to submitted proposals exactly as to any event.
func TestTheOwnerRefusesAProposalTheRulesForbid(t *testing.T) {
	m := newMesh(t, "walnut", "maple")
	before := m.members["cedar"].group.Head()
	forbidden := m.signed("maple", grouplog.KindEjection, grouplog.MemberBody{MemberID: "walnut"})
	if _, err := m.client("maple").Submit(context.Background(), m.peer("cedar"), "group-1", forbidden); !errors.Is(err, ErrProposalRefused) {
		t.Fatalf("error = %v, want ErrProposalRefused", err)
	}
	if m.members["cedar"].group.Head() != before {
		t.Fatal("a refused proposal must not change the owner's log")
	}
}

// C-PO-23: only the owner's node sequences; others say so rather than
// accepting a proposal they cannot place in the log.
func TestANodeThatIsNotTheOwnerRefusesToSequence(t *testing.T) {
	m := newMesh(t, "walnut", "maple")
	proposal := m.signed("walnut", grouplog.KindEjection, grouplog.MemberBody{MemberID: "maple"})
	if _, err := m.client("walnut").Submit(context.Background(), m.peer("maple"), "group-1", proposal); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("error = %v, want ErrNotOwner", err)
	}
}

func TestANonMemberCannotSubmit(t *testing.T) {
	m := newMesh(t, "walnut")
	outsider := newIdentity(t, "outsider")
	client := NewClient(outsider, transport.NewMemoryTrustStore(m.members["cedar"].identity.Fingerprint()))
	proposal, _ := grouplog.NewProposal(outsider, "group-1", "outsider", grouplog.KindLeave, grouplog.MemberBody{MemberID: "outsider"}, now)
	if _, err := client.Submit(context.Background(), m.peer("cedar"), "group-1", proposal); !errors.Is(err, ErrRefusedByPeer) {
		t.Fatalf("error = %v, want ErrRefusedByPeer", err)
	}
}
