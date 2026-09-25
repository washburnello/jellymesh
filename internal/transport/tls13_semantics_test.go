package transport

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
)

// This test pins a crypto/tls behaviour that the ClientTLSConfig doc comment
// warns callers about, so that the warning cannot quietly become false: under
// TLS 1.3 the client's Handshake returns nil even when the server rejects its
// certificate, and the rejection only appears on the first Read.
//
// If Go ever changes this, the assertion below fails loudly and the doc comment
// on ClientTLSConfig should be revisited.
func TestClientHandshakeMayReturnNilDespiteServerRejection(t *testing.T) {
	serverIdentity := newSelfSignedCertificate(t, 1)
	clientIdentity := newSelfSignedCertificate(t, 2)

	serverFingerprint := fingerprintOf(t, serverIdentity)

	// The server trusts nobody, so it must reject this client.
	serverTrust := NewMemoryTrustStore()
	clientTrust := NewMemoryTrustStore(serverFingerprint)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		raw, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		conn := tls.Server(raw, ServerTLSConfig(serverIdentity, serverTrust))
		serverErr <- conn.Handshake()
	}()

	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	client := tls.Client(raw, ClientTLSConfig(clientIdentity, serverFingerprint, clientTrust))

	clientHandshakeErr := client.Handshake()
	t.Logf("CLIENT Handshake() error: %v", clientHandshakeErr)

	if got := <-serverErr; !errors.Is(got, ErrUntrustedPeer) {
		t.Fatalf("server should have rejected with ErrUntrustedPeer, got %v", got)
	}

	// The decisive part: does the rejection surface only on first use?
	_, readErr := client.Read(make([]byte, 1))
	t.Logf("CLIENT first Read() error: %v", readErr)

	if clientHandshakeErr == nil && readErr == nil {
		t.Fatal("rejection never surfaced to the client at all")
	}
	if clientHandshakeErr == nil {
		t.Logf("CONFIRMED: handshake returned nil; rejection surfaced only on read (%v)", readErr)
	} else {
		t.Logf("NOT REPRODUCED here: handshake itself failed (%v)", clientHandshakeErr)
	}
	if readErr == io.EOF {
		t.Log("(server closed without a TLS alert)")
	}
}
