package enrollment

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"

	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

// listen serves TLS as identity the way a federation listener does, and
// returns its address.
func listen(t *testing.T, name string) (string, node.Fingerprint) {
	t.Helper()
	identity := newIdentity(t, name)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", transport.ServerTLSConfigAuthorizingPerRequest(identity.TLSCertificate()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				conn.(*tls.Conn).Handshake()
				conn.Close()
			}()
		}
	}()
	return listener.Addr().String(), identity.Fingerprint()
}

// C-NT-1: an approver accepts a joining node only when the key answering at
// its advertised address is the key it joined with.
func TestAdvertisedAddressMustAnswerWithTheJoinersKey(t *testing.T) {
	approver := newIdentity(t, "approver")
	address, fingerprint := listen(t, "joiner")
	ctx := context.Background()

	if err := CheckAdvertisedAddress(ctx, approver, address, fingerprint); err != nil {
		t.Fatalf("a reachable joiner was refused: %v", err)
	}

	otherAddress, _ := listen(t, "someone-else")
	if err := CheckAdvertisedAddress(ctx, approver, otherAddress, fingerprint); !errors.Is(err, ErrAddressHasOtherKey) {
		t.Fatalf("another node's address must be refused as such, got %v", err)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unused := closed.Addr().String()
	closed.Close()
	if err := CheckAdvertisedAddress(ctx, approver, unused, fingerprint); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("an unreachable address must be refused, got %v", err)
	}

	if err := CheckAdvertisedAddress(ctx, approver, "  ", fingerprint); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("an empty address must be refused, got %v", err)
	}
}
