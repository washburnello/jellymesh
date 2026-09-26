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
// If Go ever changes this, the test fails and the doc comment on
// ClientTLSConfig should be revisited. It pins library behaviour only: no dial
// path exists yet, so nothing here proves that Jellymesh honours the caution.
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

	// The server's rejection is what makes this case interesting; confirm it
	// happened before asking what the client saw.
	clientHandshakeErr := client.Handshake()
	if got := <-serverErr; !errors.Is(got, ErrUntrustedPeer) {
		t.Fatalf("server should have rejected with ErrUntrustedPeer, got %v", got)
	}

	// The pinned behaviour: the client's handshake reports success even though
	// the server refused it. If this starts failing, crypto/tls has changed
	// and the ClientTLSConfig doc comment must be revisited.
	if clientHandshakeErr != nil {
		t.Fatalf("client Handshake returned %v; crypto/tls now surfaces the rejection during the handshake, so revisit the ClientTLSConfig caution", clientHandshakeErr)
	}

	// And the rejection must surface on first use, never be silently lost.
	if _, readErr := client.Read(make([]byte, 1)); readErr == nil {
		t.Fatal("the server's rejection never surfaced to the client")
	} else if readErr == io.EOF {
		t.Log("server closed without a TLS alert")
	}
}
