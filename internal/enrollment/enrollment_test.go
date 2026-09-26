package enrollment

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"jellymesh/internal/federation"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/replication"
	"jellymesh/internal/store"
	"jellymesh/internal/transport"
)

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

type testNode struct {
	name     string
	identity *node.Identity
	database *store.DB
	group    *membership.Group
	server   *replication.Server
	inviter  *Inviter
	address  string
}

func newIdentity(t *testing.T, name string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	identity, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return identity
}

func newTestNode(t *testing.T, name string) *testNode {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "state", "jellymesh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return &testNode{name: name, identity: newIdentity(t, name), database: database}
}

func (n *testNode) logs() *store.GroupLogRepository   { return store.NewGroupLogRepository(n.database) }
func (n *testNode) blocks() *store.PeerRepository     { return store.NewPeerRepository(n.database) }
func (n *testNode) policies() *store.PolicyRepository { return store.NewPolicyRepository(n.database) }

func (n *testNode) peer() replication.Peer {
	return replication.Peer{Address: n.address, Fingerprint: n.identity.Fingerprint()}
}

// listen starts the node's federation listener on loopback.
func (n *testNode) listen(t *testing.T) {
	t.Helper()
	n.server = replication.NewServer(n.identity)
	n.server.Add("group-1", n.group)
	services := []federation.Routes{n.server}
	if n.inviter != nil {
		services = append(services, n.inviter)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: federation.Handler(n.server, services...), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(tls.NewListener(listener, federation.TLSConfig(n.identity)))
	t.Cleanup(func() { server.Close() })
	n.address = listener.Addr().String()
}

// world is cedar, the owner and inviter; walnut, an administrator; maple, an
// ordinary member; and juniper, a node that wants to join.
type world struct {
	cedar, walnut, maple, juniper *testNode
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{cedar: newTestNode(t, "cedar"), walnut: newTestNode(t, "walnut"), maple: newTestNode(t, "maple"), juniper: newTestNode(t, "juniper")}
	ctx := context.Background()

	group, err := membership.Found(ctx, w.cedar.logs(), w.cedar.blocks(), w.cedar.identity, "group-1", "cedar", "Cedar", "cedar.example.org", now)
	if err != nil {
		t.Fatalf("found: %v", err)
	}
	w.cedar.group = group
	sequence := func(kind grouplog.Kind, body any) {
		proposal, err := grouplog.NewProposal(w.cedar.identity, "group-1", "cedar", kind, body, now)
		if err != nil {
			t.Fatalf("proposal: %v", err)
		}
		if _, err := group.Sequence(ctx, w.cedar.identity, proposal, now); err != nil {
			t.Fatalf("sequence: %v", err)
		}
	}
	for _, member := range []*testNode{w.walnut, w.maple} {
		sequence(grouplog.KindAdmission, grouplog.AdmissionBody{
			MemberID: member.name, MemberKey: member.identity.PublicKey(), InvitationID: "seed-" + member.name, InviterID: "cedar",
		})
	}
	sequence(grouplog.KindPromote, grouplog.MemberBody{MemberID: "walnut"})

	events := group.EventsAfter(0)
	for _, member := range []*testNode{w.walnut, w.maple} {
		joined, err := membership.Join(ctx, member.logs(), member.blocks(), events, events[0].Hash())
		if err != nil {
			t.Fatalf("join %s: %v", member.name, err)
		}
		member.group = joined
	}

	state, err := policy.NewState("group-1")
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	w.cedar.inviter = NewInviter("cedar", w.cedar.identity, "", w.cedar.group, "group-1", state, w.cedar.policies())
	w.cedar.listen(t)
	w.cedar.inviter.address = w.cedar.address
	w.walnut.listen(t)
	w.maple.listen(t)
	return w
}

func (w *world) invite(t *testing.T) Token {
	t.Helper()
	token, err := w.cedar.inviter.Invite(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	return token
}

func (w *world) redeem(t *testing.T, token Token) Redemption {
	t.Helper()
	redemption, err := Redeem(context.Background(), w.juniper.identity, token, "juniper", "Juniper", "juniper.example.org",
		[]policy.Library{{ID: "movies", Name: "Movies", CollectionType: "movies"}})
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return redemption
}

// approve is walnut, an administrator, fetching the request from the inviter,
// signing the admission, and submitting it to the owner.
func (w *world) approve(t *testing.T) grouplog.Event {
	t.Helper()
	ctx := context.Background()
	requests, err := Requests(ctx, w.walnut.identity, w.walnut.group, w.cedar.peer(), "group-1")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %v, %v", requests, err)
	}
	body, err := AdmissionFromRequest(requests[0])
	if err != nil {
		t.Fatalf("admission from request: %v", err)
	}
	proposal, err := grouplog.NewProposal(w.walnut.identity, "group-1", "walnut", grouplog.KindAdmission, body, now)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	event, err := replication.NewClient(w.walnut.identity, w.walnut.group).Submit(ctx, w.cedar.peer(), "group-1", proposal)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := w.cedar.inviter.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return event
}

// C-EN-1: a node joins end to end with a short code: it redeems against the
// inviter pinned by the code's fingerprint prefix, an administrator on
// another node approves, the owner sequences, and the new node downloads a
// log anchored to the genesis it learned over the pinned connection.
func TestANodeJoinsWithAShortCode(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	code := w.invite(t).ShortCode()

	token, err := ParseShortCode(w.cedar.address, code)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	redemption := w.redeem(t, token)
	if redemption.InviterFingerprint != w.cedar.identity.Fingerprint() || redemption.GroupID != "group-1" {
		t.Fatalf("redemption = %+v", redemption)
	}
	inviter := replication.Peer{Address: w.cedar.address, Fingerprint: redemption.InviterFingerprint}
	if status, err := Status(ctx, w.juniper.identity, inviter); err != nil || status.Status != string(policy.InvitationAwaitingApproval) {
		t.Fatalf("status before approval: %+v, %v", status, err)
	}

	w.approve(t)
	if status, err := Status(ctx, w.juniper.identity, inviter); err != nil || status.Status != string(policy.InvitationAdmitted) {
		t.Fatalf("status after approval: %+v, %v", status, err)
	}

	joined, err := Join(ctx, w.juniper.identity, inviter, "group-1", redemption.Genesis, w.juniper.logs(), w.juniper.blocks())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if joined.Head() != w.cedar.group.Head() {
		t.Fatal("the new member should hold the owner's log")
	}
	if !w.cedar.group.IsTrusted(w.juniper.identity.Fingerprint()) {
		t.Fatal("the owner's node should trust the new member")
	}
	if requests, _ := Requests(ctx, w.walnut.identity, w.walnut.group, w.cedar.peer(), "group-1"); len(requests) != 0 {
		t.Fatal("an admitted request should no longer be pending")
	}
}

// C-EN-1: the QR form carries the genesis hash, and a join through it works.
func TestANodeJoinsWithAQRCode(t *testing.T) {
	w := newWorld(t)
	token, err := ParseQR(w.invite(t).QR())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	redemption := w.redeem(t, token)
	w.approve(t)
	inviter := replication.Peer{Address: token.Address, Fingerprint: token.Fingerprint}
	if _, err := Join(context.Background(), w.juniper.identity, inviter, token.GroupID, token.Genesis, w.juniper.logs(), w.juniper.blocks()); err != nil {
		t.Fatalf("join: %v", err)
	}
	if redemption.Genesis != token.Genesis {
		t.Fatal("the inviter should report the invitation's genesis")
	}
}

// C-EN-3: an inviter that reports a different group than the QR invitation
// carries is refused.
func TestAQRInvitationForADifferentGroupIsRefused(t *testing.T) {
	w := newWorld(t)
	token := w.invite(t)
	token.Genesis[0] ^= 1
	_, err := Redeem(context.Background(), w.juniper.identity, token, "juniper", "", "", []policy.Library{{ID: "m", Name: "m", CollectionType: "movies"}})
	if !errors.Is(err, ErrGenesisDiffer) {
		t.Fatalf("error = %v, want ErrGenesisDiffer", err)
	}
}

// C-EN-3: a server that does not hold the inviter's key never sees the
// secret, even when it answers at the inviter's address.
func TestAnImpostorNeverReceivesTheSecret(t *testing.T) {
	w := newWorld(t)
	token, err := ParseShortCode(w.cedar.address, w.invite(t).ShortCode())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	impostor := newIdentity(t, "impostor")
	var hits atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(tls.NewListener(listener, federation.TLSConfig(impostor)))
	defer server.Close()

	token.Address = listener.Addr().String()
	if _, err := Redeem(context.Background(), w.juniper.identity, token, "juniper", "", "", []policy.Library{{ID: "m", Name: "m", CollectionType: "movies"}}); err == nil {
		t.Fatal("redeeming against an impostor must fail")
	}
	if hits.Load() != 0 {
		t.Fatal("the impostor must never receive a request, and so never the secret")
	}
}

// C-EN-4: wrong, used, and expired secrets look alike, and repeated failures
// shut redemption off for a while, even for a correct secret.
func TestFailedRedemptionsAreIndistinguishableAndRateLimited(t *testing.T) {
	w := newWorld(t)
	good := w.invite(t)
	bad := good
	bad.Secret = []byte("wrongsec")
	libraries := []policy.Library{{ID: "m", Name: "m", CollectionType: "movies"}}
	clock := time.Now()
	w.cedar.inviter.now = func() time.Time { return clock }

	for attempt := 0; attempt < failureLimit; attempt++ {
		if _, err := Redeem(context.Background(), w.juniper.identity, bad, "juniper", "", "", libraries); !errors.Is(err, ErrInvitationUnavailable) {
			t.Fatalf("attempt %d: error = %v, want ErrInvitationUnavailable", attempt, err)
		}
	}
	if _, err := Redeem(context.Background(), w.juniper.identity, good, "juniper", "", "", libraries); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after the limit: error = %v, want ErrRateLimited", err)
	}
	clock = clock.Add(failureWindow + time.Second)
	if _, err := Redeem(context.Background(), w.juniper.identity, good, "juniper", "", "", libraries); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if _, err := Redeem(context.Background(), w.juniper.identity, good, "juniper", "", "", libraries); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("a used secret: error = %v, want ErrInvitationUnavailable", err)
	}
}

// C-EN-2: the admission binds the key the invitee presented in TLS. A request
// body cannot substitute another key.
func TestTheRedeemingKeyIsTheOneAdmitted(t *testing.T) {
	w := newWorld(t)
	token := w.invite(t)
	config := transport.ClientTLSConfigMatching(w.juniper.identity.TLSCertificate(), token.Matches)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	body := `{"secret":"` + encodeSecret(token.Secret) + `","node_id":"juniper","libraries":[{"ID":"m","Name":"m","CollectionType":"movies"}],"public_key":"AAAA"}`
	response, err := client.Post("https://"+w.cedar.address+RedeemPath, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a body naming a key: status %d, want 400", response.StatusCode)
	}

	w.redeem(t, token)
	requests, err := Requests(context.Background(), w.walnut.identity, w.walnut.group, w.cedar.peer(), "group-1")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %v, %v", requests, err)
	}
	if requests[0].Fingerprint != w.juniper.identity.Fingerprint() {
		t.Fatal("the pending request must carry the key the invitee connected with")
	}
	tampered := requests[0]
	tampered.PublicKey = newIdentity(t, "other").PublicKey()
	if _, err := AdmissionFromRequest(tampered); err == nil {
		t.Fatal("an approver must refuse a request whose key does not match its fingerprint")
	}
}

// C-EN-5: only an owner or administrator can see pending requests or deny
// one, and a denied invitee learns it.
func TestOnlyAdministratorsSeeAndDenyRequests(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	redemption := w.redeem(t, w.invite(t))

	if _, err := Requests(ctx, w.maple.identity, w.maple.group, w.cedar.peer(), "group-1"); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("an ordinary member listing requests: error = %v, want a 404", err)
	}
	if err := Deny(ctx, w.maple.identity, w.maple.group, w.cedar.peer(), "group-1", redemption.InvitationID); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("an ordinary member denying: error = %v, want a 404", err)
	}
	if err := Deny(ctx, w.walnut.identity, w.walnut.group, w.cedar.peer(), "group-1", redemption.InvitationID); err != nil {
		t.Fatalf("an administrator denying: %v", err)
	}
	inviter := replication.Peer{Address: w.cedar.address, Fingerprint: redemption.InviterFingerprint}
	if status, err := Status(ctx, w.juniper.identity, inviter); err != nil || status.Status != string(policy.InvitationDenied) {
		t.Fatalf("status after denial: %+v, %v", status, err)
	}
	if requests, _ := Requests(ctx, w.walnut.identity, w.walnut.group, w.cedar.peer(), "group-1"); len(requests) != 0 {
		t.Fatal("a denied request should no longer be pending")
	}
	if _, err := Join(ctx, w.juniper.identity, inviter, "group-1", redemption.Genesis, w.juniper.logs(), w.juniper.blocks()); err == nil {
		t.Fatal("a denied node must not be able to download the log")
	}
}

// C-EN-6: on the federation listener a non-member reaches enrollment and
// nothing else.
func TestANonMemberReachesOnlyEnrollment(t *testing.T) {
	w := newWorld(t)
	outsider := newIdentity(t, "outsider")
	client := replication.NewClient(outsider, transport.NewMemoryTrustStore(w.cedar.identity.Fingerprint()))
	if _, err := client.Head(context.Background(), w.cedar.peer(), "group-1"); !errors.Is(err, replication.ErrNotServed) {
		t.Fatalf("a non-member reading the log: error = %v, want ErrNotServed", err)
	}
	if _, err := Status(context.Background(), outsider, w.cedar.peer()); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("a non-member with no request asking status: error = %v, want a 404", err)
	}
	// And the enrollment route is reachable: a correct redemption works.
	w.redeem(t, w.invite(t))
}

// Join refuses a log that does not admit the joining node's key.
func TestJoinBeforeAdmissionFails(t *testing.T) {
	w := newWorld(t)
	redemption := w.redeem(t, w.invite(t))
	inviter := replication.Peer{Address: w.cedar.address, Fingerprint: redemption.InviterFingerprint}
	if _, err := Join(context.Background(), w.juniper.identity, inviter, "group-1", redemption.Genesis, w.juniper.logs(), w.juniper.blocks()); err == nil {
		t.Fatal("joining before admission must fail")
	}
}

// encodeSecret encodes a secret as encoding/json encodes a byte slice.
func encodeSecret(secret []byte) string {
	return base64.StdEncoding.EncodeToString(secret)
}

// C-EN-1: Join refuses a genuine log that does not admit this node, as an
// inviter serving a stale or selective log might send.
func TestJoinRefusesALogThatDoesNotAdmitThisNode(t *testing.T) {
	w := newWorld(t)
	events := w.cedar.group.EventsAfter(0)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jellymesh/v1/groups/group-1/log/events", func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("after") == "0" {
			writeJSON(response, events)
			return
		}
		writeJSON(response, []grouplog.Event{})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(tls.NewListener(listener, federation.TLSConfig(w.cedar.identity)))
	defer server.Close()

	inviter := replication.Peer{Address: listener.Addr().String(), Fingerprint: w.cedar.identity.Fingerprint()}
	if _, err := Join(context.Background(), w.juniper.identity, inviter, "group-1", events[0].Hash(), w.juniper.logs(), w.juniper.blocks()); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("error = %v, want ErrNotAdmitted", err)
	}
	if _, found, _ := w.juniper.logs().Load(context.Background(), "group-1"); found {
		t.Fatal("a log that does not admit this node must not be stored")
	}
}
