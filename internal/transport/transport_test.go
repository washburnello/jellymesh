package transport

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
	"testing"
	"time"
)

// newCertificateForKey builds a throwaway self-signed Ed25519 certificate
// over the given key pair. Passing different serial numbers over the same
// key simulates reissuing a certificate without changing node identity.
func newCertificateForKey(t *testing.T, public ed25519.PublicKey, private ed25519.PrivateKey, serialNumber int64) tls.Certificate {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serialNumber),
		Subject:               pkix.Name{CommonName: "jellymesh-test-node"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatalf("create self-signed certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}

// newSelfSignedCertificate generates a fresh Ed25519 key pair and a
// self-signed certificate over it.
func newSelfSignedCertificate(t *testing.T, serialNumber int64) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return newCertificateForKey(t, public, private, serialNumber)
}

// fingerprintOf parses the leaf out of a tls.Certificate and computes its
// Fingerprint, failing the test on any error.
func fingerprintOf(t *testing.T, certificate tls.Certificate) Fingerprint {
	t.Helper()
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	fingerprint, err := FingerprintOfCertificate(leaf)
	if err != nil {
		t.Fatalf("fingerprint certificate: %v", err)
	}
	return fingerprint
}

type handshakeResult struct {
	conn *tls.Conn
	err  error
}

// performHandshake runs a real TLS handshake between serverConfig and
// clientConfig over a loopback-only listener and returns both sides'
// resulting connection and error. It binds only to 127.0.0.1:0.
func performHandshake(t *testing.T, serverConfig, clientConfig *tls.Config) (serverConn, clientConn *tls.Conn, serverErr, clientErr error) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	results := make(chan handshakeResult, 1)
	go func() {
		rawConn, err := listener.Accept()
		if err != nil {
			results <- handshakeResult{nil, err}
			return
		}
		serverSide := tls.Server(rawConn, serverConfig)
		err = serverSide.HandshakeContext(context.Background())
		results <- handshakeResult{serverSide, err}
	}()

	rawConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	clientConn = tls.Client(rawConn, clientConfig)
	clientErr = clientConn.HandshakeContext(context.Background())

	select {
	case result := <-results:
		serverConn, serverErr = result.conn, result.err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server-side handshake")
	}

	t.Cleanup(func() {
		clientConn.Close()
		if serverConn != nil {
			serverConn.Close()
		}
	})
	return serverConn, clientConn, serverErr, clientErr
}

func TestHandshakeSucceedsWhenMutuallyTrusted(t *testing.T) {
	serverCertificate := newSelfSignedCertificate(t, 1)
	clientCertificate := newSelfSignedCertificate(t, 2)
	serverFingerprint := fingerprintOf(t, serverCertificate)
	clientFingerprint := fingerprintOf(t, clientCertificate)

	serverTrust := NewMemoryTrustStore(clientFingerprint)
	clientTrust := NewMemoryTrustStore(serverFingerprint)

	serverConfig := ServerTLSConfig(serverCertificate, serverTrust)
	clientConfig := ClientTLSConfig(clientCertificate, serverFingerprint, clientTrust)

	serverConn, clientConn, serverErr, clientErr := performHandshake(t, serverConfig, clientConfig)
	if serverErr != nil {
		t.Fatalf("server handshake: %v", serverErr)
	}
	if clientErr != nil {
		t.Fatalf("client handshake: %v", clientErr)
	}

	gotServerPeer, err := PeerFingerprint(serverConn.ConnectionState())
	if err != nil {
		t.Fatalf("server PeerFingerprint: %v", err)
	}
	if gotServerPeer != clientFingerprint {
		t.Fatalf("server saw peer %q, want client fingerprint %q", gotServerPeer, clientFingerprint)
	}

	gotClientPeer, err := PeerFingerprint(clientConn.ConnectionState())
	if err != nil {
		t.Fatalf("client PeerFingerprint: %v", err)
	}
	if gotClientPeer != serverFingerprint {
		t.Fatalf("client saw peer %q, want server fingerprint %q", gotClientPeer, serverFingerprint)
	}
}

func TestServerRejectsUntrustedClient(t *testing.T) {
	serverCertificate := newSelfSignedCertificate(t, 1)
	clientCertificate := newSelfSignedCertificate(t, 2)
	serverFingerprint := fingerprintOf(t, serverCertificate)

	serverTrust := NewMemoryTrustStore() // client fingerprint deliberately absent
	clientTrust := NewMemoryTrustStore(serverFingerprint)

	serverConfig := ServerTLSConfig(serverCertificate, serverTrust)
	clientConfig := ClientTLSConfig(clientCertificate, serverFingerprint, clientTrust)

	_, clientConn, serverErr, _ := performHandshake(t, serverConfig, clientConfig)
	if !errors.Is(serverErr, ErrUntrustedPeer) {
		t.Fatalf("server error = %v, want ErrUntrustedPeer", serverErr)
	}
	// In TLS 1.3 the client considers its handshake complete as soon as it
	// sends its own Finished message, before the server has processed the
	// client certificate. The rejection only surfaces as the server's alert
	// arriving on (or closing) the connection, so confirm the connection is
	// actually unusable rather than asserting on the client handshake error.
	assertConnectionIsDead(t, clientConn)
}

func TestClientRejectsFingerprintMismatch(t *testing.T) {
	serverCertificate := newSelfSignedCertificate(t, 1)
	clientCertificate := newSelfSignedCertificate(t, 2)
	serverFingerprint := fingerprintOf(t, serverCertificate)
	clientFingerprint := fingerprintOf(t, clientCertificate)

	serverTrust := NewMemoryTrustStore(clientFingerprint)
	// The client's trust store trusts the server's real fingerprint, so a
	// bare trust check would pass. Pinning to the wrong expected fingerprint
	// must still fail the handshake.
	clientTrust := NewMemoryTrustStore(serverFingerprint)
	wrongExpectedFingerprint := clientFingerprint

	serverConfig := ServerTLSConfig(serverCertificate, serverTrust)
	clientConfig := ClientTLSConfig(clientCertificate, wrongExpectedFingerprint, clientTrust)

	_, _, _, clientErr := performHandshake(t, serverConfig, clientConfig)
	if !errors.Is(clientErr, ErrPeerMismatch) {
		t.Fatalf("client error = %v, want ErrPeerMismatch", clientErr)
	}
}

func TestRevokedFingerprintFailsSubsequentHandshake(t *testing.T) {
	serverCertificate := newSelfSignedCertificate(t, 1)
	clientCertificate := newSelfSignedCertificate(t, 2)
	serverFingerprint := fingerprintOf(t, serverCertificate)
	clientFingerprint := fingerprintOf(t, clientCertificate)

	serverTrust := NewMemoryTrustStore(clientFingerprint)
	clientTrust := NewMemoryTrustStore(serverFingerprint)

	serverConfig := ServerTLSConfig(serverCertificate, serverTrust)
	clientConfig := ClientTLSConfig(clientCertificate, serverFingerprint, clientTrust)

	_, _, serverErr, clientErr := performHandshake(t, serverConfig, clientConfig)
	if serverErr != nil || clientErr != nil {
		t.Fatalf("initial handshake should succeed: serverErr=%v clientErr=%v", serverErr, clientErr)
	}

	serverTrust.Revoke(clientFingerprint)

	var clientConn *tls.Conn
	_, clientConn, serverErr, _ = performHandshake(t, serverConfig, clientConfig)
	if !errors.Is(serverErr, ErrUntrustedPeer) {
		t.Fatalf("server error after revocation = %v, want ErrUntrustedPeer", serverErr)
	}
	assertConnectionIsDead(t, clientConn)
}

// assertConnectionIsDead confirms a connection is unusable, which is how a
// TLS 1.3 client observes the server's post-handshake rejection: the
// client's own Handshake call can return successfully before the server
// finishes verifying the client certificate, since the client does not wait
// for a response after sending its Finished message.
func assertConnectionIsDead(t *testing.T, conn *tls.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, 1)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("expected the connection to be closed or alerted, but Read succeeded")
	}
}

func TestFingerprintOfCertificateRequiresACertificate(t *testing.T) {
	if _, err := FingerprintOfCertificate(nil); !errors.Is(err, ErrCertificateRequired) {
		t.Fatalf("error = %v, want ErrCertificateRequired", err)
	}
}

func TestFingerprintIsStableAcrossReissuance(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	firstCertificate := newCertificateForKey(t, public, private, 1)
	secondCertificate := newCertificateForKey(t, public, private, 2)

	firstFingerprint := fingerprintOf(t, firstCertificate)
	secondFingerprint := fingerprintOf(t, secondCertificate)
	if firstFingerprint != secondFingerprint {
		t.Fatalf("fingerprints over the same key differ: %q vs %q", firstFingerprint, secondFingerprint)
	}
}

func TestFingerprintDiffersForDifferentKeys(t *testing.T) {
	firstCertificate := newSelfSignedCertificate(t, 1)
	secondCertificate := newSelfSignedCertificate(t, 2)

	firstFingerprint := fingerprintOf(t, firstCertificate)
	secondFingerprint := fingerprintOf(t, secondCertificate)
	if firstFingerprint == secondFingerprint {
		t.Fatal("fingerprints for different keys should differ")
	}
}
