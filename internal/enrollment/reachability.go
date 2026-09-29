package enrollment

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

// A member must be reachable at the address it advertises, by a router port
// forward or by Tailscale Funnel in raw-TCP mode (assumption A-16): other
// members connect to it there for its catalog and media. An approver checks
// that before signing an admission, so an unreachable node is caught when it
// joins rather than when a stream first fails (C-NT-1).
var (
	ErrNoAddress          = errors.New("the joining node advertises no address")
	ErrUnreachable        = errors.New("the joining node cannot be reached at its advertised address")
	ErrAddressHasOtherKey = errors.New("a different node answers at the joining node's advertised address")
)

// reachabilityTimeout bounds one check; an address that takes longer than
// this to complete a handshake is not usable by other members either.
const reachabilityTimeout = 10 * time.Second

// CheckAdvertisedAddress completes a TLS handshake with address and requires
// the key presented there to be want. Only the server's identity matters:
// the joining node's listener accepts any Ed25519 client certificate at the
// handshake, because it authorizes each request separately.
func CheckAdvertisedAddress(ctx context.Context, identity *node.Identity, address string, want node.Fingerprint) error {
	if strings.TrimSpace(address) == "" {
		return ErrNoAddress
	}
	ctx, cancel := context.WithTimeout(ctx, reachabilityTimeout)
	defer cancel()
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{},
		Config:    transport.ClientTLSConfig(identity.TLSCertificate(), want, transport.NewMemoryTrustStore(want)),
	}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		if errors.Is(err, transport.ErrPeerMismatch) {
			return fmt.Errorf("%w (%s)", ErrAddressHasOtherKey, address)
		}
		return fmt.Errorf("%w (%s): %v", ErrUnreachable, address, err)
	}
	return conn.Close()
}
