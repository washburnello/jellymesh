package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
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
		Trusted:        true,
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
// place. Doing so would carry the old key's trust over to the new key.
func TestUpsertRefusesToChangeAKnownNodesFingerprint(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-one")
	peer.Trusted = true
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	peer.Fingerprint = "fingerprint-two"
	if err := repo.Upsert(ctx, peer); !errors.Is(err, ErrFingerprintChanged) {
		t.Fatalf("re-keying upsert: error = %v, want ErrFingerprintChanged", err)
	}
	if repo.IsTrusted("fingerprint-two") {
		t.Fatal("a refused re-key must not trust the new key")
	}
	if !repo.IsTrusted("fingerprint-one") {
		t.Fatal("a refused re-key must leave the existing record untouched")
	}

	// Re-enrollment is explicit: remove the old record, then enroll the new
	// key as an untrusted peer awaiting a fresh decision.
	if err := repo.Remove(ctx, "cedar"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	peer.Trusted = false
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("enroll new key after removal: %v", err)
	}
	if repo.IsTrusted("fingerprint-two") {
		t.Fatal("a re-enrolled key starts untrusted")
	}
}

// C-BL-5: updating a peer's descriptive fields changes neither its trust
// decision nor its block.
func TestUpsertOfAKnownPeerDoesNotChangeTrustOrBlock(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-cedar")
	peer.Trusted = false
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("create: %v", err)
	}
	peer.Trusted = true
	peer.FriendlyName = "Renamed"
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, err := repo.ByNodeID(ctx, "cedar")
	if err != nil {
		t.Fatalf("by node id: %v", err)
	}
	if got.Trusted {
		t.Fatal("an upsert must not re-trust a peer; that is SetTrusted's job")
	}
	if got.FriendlyName != "Renamed" {
		t.Fatalf("friendly name = %q, want Renamed", got.FriendlyName)
	}

	if err := repo.SetTrusted(ctx, "cedar", true); err != nil {
		t.Fatalf("set trusted: %v", err)
	}
	if err := repo.SetBlocked(ctx, "cedar", true); err != nil {
		t.Fatalf("set blocked: %v", err)
	}
	peer.Blocked = false
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("an upsert must not lift a block")
	}
}

// C-PO-12: a block is durable for a node that has never connected, and it
// applies when that node later appears.
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
	blocked, err := repo.IsBlocked(ctx, "stranger")
	if err != nil || !blocked {
		t.Fatalf("block did not survive restart: blocked=%v err=%v", blocked, err)
	}

	peer := samplePeer("stranger", "fingerprint-stranger")
	peer.Trusted = true
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("a block recorded before first contact must apply once the peer appears")
	}
	if err := repo.Remove(ctx, "stranger"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if blocked, _ := repo.IsBlocked(ctx, "stranger"); !blocked {
		t.Fatal("removing a peer record must not lift its block")
	}
}

func TestBlockedPeerIsNotTrustedEvenWhenTrustedFlagIsTrue(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-cedar")
	peer.Trusted = true
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.SetBlocked(ctx, "cedar", true); err != nil {
		t.Fatalf("set blocked: %v", err)
	}

	if repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("a blocked peer must never be trusted, even with trusted=true")
	}
}

func TestTrustedUnblockedPeerIsTrusted(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-cedar")
	peer.Trusted = true
	peer.Blocked = false
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("a trusted, unblocked peer should be trusted")
	}
}

func TestEmptyFingerprintIsNeverTrusted(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	if repo.IsTrusted("") {
		t.Fatal("an empty fingerprint must never be trusted")
	}
}

func TestUnknownFingerprintIsNotTrusted(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	if repo.IsTrusted("never-seen") {
		t.Fatal("an unrecorded fingerprint must not be trusted")
	}
}

func TestIsTrustedFailsClosedWhenDatabaseIsClosed(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-cedar")
	peer.Trusted = true
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("expected the peer to be trusted before closing the database")
	}

	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Must fail closed rather than panic once the database is unusable.
	if repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("IsTrusted must return false, not true, once the database is closed")
	}
}

func TestSetTrustedSetBlockedAndRemove(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	peer := samplePeer("cedar", "fingerprint-cedar")
	peer.Trusted = false
	peer.Blocked = false
	if err := repo.Upsert(ctx, peer); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := repo.SetTrusted(ctx, "cedar", true); err != nil {
		t.Fatalf("set trusted: %v", err)
	}
	got, _, _ := repo.ByNodeID(ctx, "cedar")
	if !got.Trusted {
		t.Fatal("expected trusted to be set")
	}

	if err := repo.SetBlocked(ctx, "cedar", true); err != nil {
		t.Fatalf("set blocked: %v", err)
	}
	got, _, _ = repo.ByNodeID(ctx, "cedar")
	if !got.Blocked {
		t.Fatal("expected blocked to be set")
	}
	if repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("blocking a trusted peer must revoke transport trust immediately")
	}

	if err := repo.SetTrusted(ctx, "missing", true); !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("set trusted on missing node: error = %v, want ErrPeerNotFound", err)
	}
	if err := repo.SetBlocked(ctx, "cedar", false); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if !repo.IsTrusted(peer.Fingerprint) {
		t.Fatal("unblocking should restore the existing trust decision")
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

// --- Mutual TLS integration: PeerRepository as transport.TrustStore ---

// generateTestCertificate mints a throwaway self-signed Ed25519 certificate,
// mirroring internal/transport's own test helper, so this test proves the
// two packages fit together using only the public API each exposes.
func generateTestCertificate(t *testing.T, commonName string) (tls.Certificate, transport.Fingerprint) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	fingerprint, err := transport.FingerprintOfCertificate(leaf)
	if err != nil {
		t.Fatalf("fingerprint certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}, fingerprint
}

// TestPeerRepositoryAsTrustStorePerformsRealHandshake proves the durable
// PeerRepository actually satisfies transport.TrustStore end to end: a real
// mutual-TLS handshake over a loopback listener, authorized purely from a
// row this test wrote into SQLite.
func TestPeerRepositoryAsTrustStorePerformsRealHandshake(t *testing.T) {
	database := openTestDB(t)
	repo := NewPeerRepository(database)
	ctx := context.Background()

	serverCertificate, serverFingerprint := generateTestCertificate(t, "server-node")
	clientCertificate, clientFingerprint := generateTestCertificate(t, "client-node")

	// The server's trust store is the durable repository: only a peer with a
	// row that is trusted and not blocked may complete the handshake.
	if err := repo.Upsert(ctx, Peer{
		NodeID:      "client-node",
		Fingerprint: clientFingerprint,
		Trusted:     true,
	}); err != nil {
		t.Fatalf("upsert client peer: %v", err)
	}

	clientTrust := transport.NewMemoryTrustStore(serverFingerprint)
	serverConfig := transport.ServerTLSConfig(serverCertificate, repo)
	clientConfig := transport.ClientTLSConfig(clientCertificate, serverFingerprint, clientTrust)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	type result struct {
		conn *tls.Conn
		err  error
	}
	serverResults := make(chan result, 1)
	go func() {
		rawConn, err := listener.Accept()
		if err != nil {
			serverResults <- result{nil, err}
			return
		}
		serverConn := tls.Server(rawConn, serverConfig)
		err = serverConn.HandshakeContext(context.Background())
		serverResults <- result{serverConn, err}
	}()

	rawConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	clientConn := tls.Client(rawConn, clientConfig)
	defer clientConn.Close()
	if err := clientConn.HandshakeContext(context.Background()); err != nil {
		t.Fatalf("client handshake: %v", err)
	}

	select {
	case serverResult := <-serverResults:
		if serverResult.conn != nil {
			defer serverResult.conn.Close()
		}
		if serverResult.err != nil {
			t.Fatalf("server handshake against a durable trust store: %v", serverResult.err)
		}
		peer, err := transport.PeerFingerprint(serverResult.conn.ConnectionState())
		if err != nil {
			t.Fatalf("peer fingerprint: %v", err)
		}
		if peer != clientFingerprint {
			t.Fatalf("server saw peer %q, want %q", peer, clientFingerprint)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server handshake")
	}

	// Now block the peer and confirm a fresh handshake is refused, proving
	// IsTrusted's fail-closed block semantics reach all the way through TLS.
	if err := repo.SetBlocked(ctx, "client-node", true); err != nil {
		t.Fatalf("set blocked: %v", err)
	}

	listener2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener2.Close()

	blockedResults := make(chan result, 1)
	go func() {
		rawConn, err := listener2.Accept()
		if err != nil {
			blockedResults <- result{nil, err}
			return
		}
		serverConn := tls.Server(rawConn, serverConfig)
		err = serverConn.HandshakeContext(context.Background())
		blockedResults <- result{serverConn, err}
	}()

	rawConn2, err := net.Dial("tcp", listener2.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	blockedClientConn := tls.Client(rawConn2, clientConfig)
	defer blockedClientConn.Close()
	_ = blockedClientConn.HandshakeContext(context.Background())

	select {
	case blockedResult := <-blockedResults:
		if blockedResult.conn != nil {
			defer blockedResult.conn.Close()
		}
		if !errors.Is(blockedResult.err, transport.ErrUntrustedPeer) {
			t.Fatalf("server error after block = %v, want ErrUntrustedPeer", blockedResult.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server handshake")
	}
}
