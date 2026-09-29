package natpath

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"jellymesh/internal/node"
)

func identity(t *testing.T, name string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	id, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func endpoint(t *testing.T, id *node.Identity) *Endpoint {
	t.Helper()
	e, err := Listen("127.0.0.1:0", id.TLSCertificate())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func within(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// C-NT-4: two endpoints open a QUIC connection authenticated by the same
// pinned node keys as the TCP transport, and carry data both ways.
func TestADirectConnectionUsesThePinnedKeys(t *testing.T) {
	cedarID, walnutID := identity(t, "cedar"), identity(t, "walnut")
	cedar, walnut := endpoint(t, cedarID), endpoint(t, walnutID)
	ctx := within(t, 10*time.Second)

	wait := cedar.Expect(ctx, walnutID.Fingerprint(), []netip.AddrPort{walnut.LocalAddr()})
	go cedar.Punch(ctx, []netip.AddrPort{walnut.LocalAddr()})
	dialed, err := walnut.Dial(ctx, cedarID.Fingerprint(), []netip.AddrPort{cedar.LocalAddr()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	accepted, err := wait()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got, _ := node.FingerprintOfCertificate(dialed.ConnectionState().TLS.PeerCertificates[0]); got != cedarID.Fingerprint() {
		t.Fatal("walnut should see cedar's key")
	}
	if got, _ := node.FingerprintOfCertificate(accepted.ConnectionState().TLS.PeerCertificates[0]); got != walnutID.Fingerprint() {
		t.Fatal("cedar should see walnut's key")
	}

	stream, err := dialed.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		incoming, err := accepted.AcceptStream(ctx)
		if err != nil {
			return
		}
		io.Copy(incoming, incoming) // echo
		incoming.Close()
	}()
	stream.Write([]byte("range bytes=0-99"))
	stream.Close()
	echoed, _ := io.ReadAll(stream)
	if string(echoed) != "range bytes=0-99" {
		t.Fatalf("echo %q", echoed)
	}
}

// C-NT-4: a peer presenting another key, or a node no offer was accepted
// from, is refused at the handshake.
func TestADirectConnectionRefusesTheWrongKey(t *testing.T) {
	cedarID, walnutID, strangerID := identity(t, "cedar"), identity(t, "walnut"), identity(t, "stranger")
	cedar, walnut, stranger := endpoint(t, cedarID), endpoint(t, walnutID), endpoint(t, strangerID)
	ctx := within(t, 20*time.Second)

	// walnut expects cedar but reaches the stranger's address.
	strangerWait := stranger.Expect(ctx, walnutID.Fingerprint(), []netip.AddrPort{walnut.LocalAddr()})
	if _, err := walnut.Dial(ctx, cedarID.Fingerprint(), []netip.AddrPort{stranger.LocalAddr()}); err == nil {
		t.Fatal("a peer with the wrong key must be refused")
	}
	_ = strangerWait

	// cedar expects walnut, but from the stranger's address, as if an
	// attacker held walnut's address: the key must still be refused, in the
	// TLS handshake itself (a crypto error), not merely by closing the
	// connection after it was accepted.
	cedar.Expect(ctx, walnutID.Fingerprint(), []netip.AddrPort{stranger.LocalAddr()})
	conn, err := stranger.Dial(ctx, cedarID.Fingerprint(), []netip.AddrPort{cedar.LocalAddr()})
	if err == nil {
		// Under TLS 1.3 the client can finish before the server's refusal
		// arrives; the refusal then ends the connection at once.
		stream, openErr := conn.OpenStreamSync(ctx)
		if openErr == nil {
			stream.Write([]byte("x"))
			_, openErr = stream.Read(make([]byte, 1))
		}
		err = openErr
	}
	var transportError *quic.TransportError
	if err == nil || !errors.As(err, &transportError) || !transportError.ErrorCode.IsCryptoError() {
		t.Fatalf("a node cedar does not expect must be refused at the handshake, got %v", err)
	}
}

// C-NT-2: STUN discovery runs on the endpoint's own socket while QUIC is
// listening on it, and QUIC still works afterwards.
func TestDiscoveryAndQUICShareOneSocket(t *testing.T) {
	cedarID, walnutID := identity(t, "cedar"), identity(t, "walnut")
	cedar, walnut := endpoint(t, cedarID), endpoint(t, walnutID)
	server := startServer(t, nil, true)
	result, err := cedar.Discover(within(t, 2*time.Second), []string{server.address()})
	if err != nil {
		t.Fatalf("discover on the QUIC socket: %v", err)
	}
	if result.Address != cedar.LocalAddr() {
		t.Fatalf("learned %v, want the endpoint's own socket %v", result.Address, cedar.LocalAddr())
	}
	ctx := within(t, 10*time.Second)
	wait := cedar.Expect(ctx, walnutID.Fingerprint(), []netip.AddrPort{walnut.LocalAddr()})
	if _, err := walnut.Dial(ctx, cedarID.Fingerprint(), []netip.AddrPort{cedar.LocalAddr()}); err != nil {
		t.Fatalf("QUIC after discovery: %v", err)
	}
	if _, err := wait(); err != nil {
		t.Fatal(err)
	}
}

// C-NT-4: punching sends a bounded number of small datagrams and stops at
// its window, so an offer cannot turn a node into a flood.
func TestPunchingIsBounded(t *testing.T) {
	cedar := endpoint(t, identity(t, "cedar"))
	sink, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	var received atomic.Int64
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, _, err := sink.ReadFrom(buffer)
			if err != nil {
				return
			}
			if n > 64 {
				t.Errorf("a punch datagram of %d bytes", n)
			}
			received.Add(1)
		}
	}()
	target, _ := addrPortOf(sink.LocalAddr())
	many := make([]netip.AddrPort, maxCandidates+4)
	for i := range many {
		many[i] = target
	}
	began := time.Now()
	sent := cedar.Punch(context.Background(), many)
	elapsed := time.Since(began)
	if elapsed > punchWindow+time.Second {
		t.Fatalf("punching ran %v, past its window", elapsed)
	}
	limit := maxCandidates * int(punchWindow/punchInterval+1)
	if sent == 0 || sent > limit {
		t.Fatalf("sent %d datagrams, want between 1 and %d", sent, limit)
	}
	time.Sleep(100 * time.Millisecond)
	if received.Load() == 0 {
		t.Fatal("punch datagrams never arrived")
	}
}

// C-NT-4 (#55): a connection attempt from an address no accepted offer
// named is refused before any cryptography, even by the right key.
func TestAConnectionFromAnUnofferedAddressIsRefusedEarly(t *testing.T) {
	cedarID, walnutID := identity(t, "cedar"), identity(t, "walnut")
	cedar, walnut := endpoint(t, cedarID), endpoint(t, walnutID)
	ctx := within(t, 10*time.Second)
	elsewhere := netip.MustParseAddrPort("127.0.0.1:9") // not walnut's socket
	cedar.Expect(ctx, walnutID.Fingerprint(), []netip.AddrPort{elsewhere})
	_, err := walnut.Dial(ctx, cedarID.Fingerprint(), []netip.AddrPort{cedar.LocalAddr()})
	var transportError *quic.TransportError
	if err == nil || (errors.As(err, &transportError) && transportError.ErrorCode.IsCryptoError()) {
		t.Fatalf("an unoffered address must be refused before the handshake, got %v", err)
	}
}
