package natpath

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

// ALPN names Jellymesh's direct-path protocol inside QUIC.
const ALPN = "jellymesh-direct/1"

// Punching is bounded: each candidate gets a small datagram every
// punchInterval for at most punchWindow, so an offer can never turn this
// node into a source of much traffic toward anyone.
const (
	punchInterval = 100 * time.Millisecond
	punchWindow   = 5 * time.Second
	punchPayload  = "jellymesh-punch"
)

var (
	ErrNotExpected = errors.New("no direct connection from this peer is expected")
	ErrNoPath      = errors.New("no direct path could be opened")
)

func quicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       60 * time.Second,
		// Keep-alives hold both NATs' mappings open between requests.
		KeepAlivePeriod: 15 * time.Second,
	}
}

// Endpoint is a node's one UDP socket for direct paths. QUIC and STUN share
// it, because a NAT maps each socket separately: the address STUN learns is
// only good for the socket that asked (C-NT-2).
type Endpoint struct {
	conn      *net.UDPConn
	transport *quic.Transport
	listener  *quic.Listener
	cert      tls.Certificate

	mutex     sync.Mutex
	expecting map[node.Fingerprint]chan *quic.Conn
	stun      chan datagram

	closed chan struct{}
}

type datagram struct {
	data []byte
	from net.Addr
}

// Listen opens the endpoint on address (such as ":0" or "0.0.0.0:41641")
// with the node's certificate.
func Listen(address string, cert tls.Certificate) (*Endpoint, error) {
	local, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", local)
	if err != nil {
		return nil, err
	}
	endpoint := &Endpoint{
		conn: conn, transport: &quic.Transport{Conn: conn}, cert: cert,
		expecting: map[node.Fingerprint]chan *quic.Conn{},
		stun:      make(chan datagram, 64),
		closed:    make(chan struct{}),
	}
	server := &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{cert},
		ClientAuth:            tls.RequireAnyClientCert,
		NextProtos:            []string{ALPN},
		VerifyPeerCertificate: endpoint.verifyExpected,
	}
	if endpoint.listener, err = endpoint.transport.Listen(server, quicConfig()); err != nil {
		conn.Close()
		return nil, err
	}
	// quic-go keeps non-QUIC datagrams only once ReadNonQUICPacket has been
	// called, and drops them before that. Call it once now, with a context
	// that is already done, so that STUN answers arriving before the reader
	// goroutine runs are queued rather than lost.
	started, cancel := context.WithCancel(context.Background())
	cancel()
	endpoint.transport.ReadNonQUICPacket(started, nil)
	go endpoint.accept()
	go endpoint.readNonQUIC()
	return endpoint, nil
}

// LocalAddr is the socket's local address.
func (endpoint *Endpoint) LocalAddr() netip.AddrPort {
	address, _ := addrPortOf(endpoint.conn.LocalAddr())
	return address
}

// Close closes the listener, every connection, and the socket.
func (endpoint *Endpoint) Close() error {
	select {
	case <-endpoint.closed:
		return nil
	default:
		close(endpoint.closed)
	}
	endpoint.listener.Close()
	endpoint.transport.Close()
	return endpoint.conn.Close()
}

// verifyExpected admits a connecting peer only if a connection from its key
// is expected: one whose offer this node has just accepted.
func (endpoint *Endpoint) verifyExpected(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return transport.ErrNoPeerCertificate
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return err
	}
	fingerprint, err := transport.FingerprintOfCertificate(leaf)
	if err != nil {
		return err
	}
	endpoint.mutex.Lock()
	_, ok := endpoint.expecting[fingerprint]
	endpoint.mutex.Unlock()
	if !ok {
		return ErrNotExpected
	}
	return nil
}

func (endpoint *Endpoint) accept() {
	for {
		conn, err := endpoint.listener.Accept(context.Background())
		if err != nil {
			return
		}
		state := conn.ConnectionState().TLS
		fingerprint, err := transport.PeerFingerprint(state)
		endpoint.mutex.Lock()
		waiter, ok := endpoint.expecting[fingerprint]
		endpoint.mutex.Unlock()
		if err != nil || !ok {
			conn.CloseWithError(1, "not expected")
			continue
		}
		select {
		case waiter <- conn:
		default:
			conn.CloseWithError(1, "already connected")
		}
	}
}

// readNonQUIC drains datagrams that are not QUIC: STUN answers go to
// discovery, and punch datagrams, which only exist to open the NAT, are
// dropped.
func (endpoint *Endpoint) readNonQUIC() {
	for {
		buffer := make([]byte, maxResponseLength)
		n, from, err := endpoint.transport.ReadNonQUICPacket(context.Background(), buffer)
		if err != nil {
			return
		}
		if n >= headerLength && buffer[4] == 0x21 && buffer[5] == 0x12 && buffer[6] == 0xA4 && buffer[7] == 0x42 {
			select {
			case endpoint.stun <- datagram{buffer[:n], from}:
			default: // nobody is asking; drop it
			}
		}
	}
}

// Discover learns this endpoint's outside address from STUN servers, on the
// endpoint's own socket, while QUIC keeps running on it.
func (endpoint *Endpoint) Discover(ctx context.Context, servers []string) (Result, error) {
	send := func(b []byte, to net.Addr) error { _, err := endpoint.transport.WriteTo(b, to); return err }
	receive := func(ctx context.Context, b []byte) (int, net.Addr, error) {
		select {
		case packet := <-endpoint.stun:
			return copy(b, packet.data), packet.from, nil
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	return discover(ctx, send, receive, servers)
}

// Expect admits one direct connection from peer, and returns a function
// that waits for it. The expectation lapses when ctx ends.
func (endpoint *Endpoint) Expect(ctx context.Context, peer node.Fingerprint) func() (*quic.Conn, error) {
	waiter := make(chan *quic.Conn, 1)
	endpoint.mutex.Lock()
	endpoint.expecting[peer] = waiter
	endpoint.mutex.Unlock()
	return func() (*quic.Conn, error) {
		defer func() {
			endpoint.mutex.Lock()
			if endpoint.expecting[peer] == waiter {
				delete(endpoint.expecting, peer)
			}
			endpoint.mutex.Unlock()
		}()
		select {
		case conn := <-waiter:
			return conn, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrNoPath, ctx.Err())
		}
	}
}

// Punch sends small datagrams to each candidate until ctx ends or the punch
// window passes, so that this side's NAT admits the peer's packets. It
// returns how many it sent.
func (endpoint *Endpoint) Punch(ctx context.Context, candidates []netip.AddrPort) int {
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	ctx, cancel := context.WithTimeout(ctx, punchWindow)
	defer cancel()
	ticker := time.NewTicker(punchInterval)
	defer ticker.Stop()
	sent := 0
	for {
		for _, candidate := range candidates {
			if _, err := endpoint.transport.WriteTo([]byte(punchPayload), net.UDPAddrFromAddrPort(candidate)); err == nil {
				sent++
			}
		}
		select {
		case <-ctx.Done():
			return sent
		case <-ticker.C:
		}
	}
}

// Dial opens a direct QUIC connection to peer, punching toward every
// candidate meanwhile and racing a handshake to each; the first to complete
// wins. The peer is held to its pinned key exactly as on TCP.
func (endpoint *Endpoint) Dial(ctx context.Context, peer node.Fingerprint, candidates []netip.AddrPort) (*quic.Conn, error) {
	if len(candidates) == 0 {
		return nil, ErrNoPath
	}
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	ctx, cancel := context.WithTimeout(ctx, punchWindow+2*time.Second)
	defer cancel()
	go endpoint.Punch(ctx, candidates)
	client := transport.ClientTLSConfig(endpoint.cert, peer, transport.NewMemoryTrustStore(peer))
	client.NextProtos = []string{ALPN}
	type outcome struct {
		conn *quic.Conn
		err  error
	}
	results := make(chan outcome, len(candidates))
	for _, candidate := range candidates {
		go func() {
			conn, err := endpoint.transport.Dial(ctx, net.UDPAddrFromAddrPort(candidate), client, quicConfig())
			results <- outcome{conn, err}
		}()
	}
	var winner *quic.Conn
	var last error
	for range candidates {
		result := <-results
		switch {
		case result.err != nil:
			last = result.err
		case winner == nil:
			winner = result.conn
			cancel() // stop the other handshakes and the punching
		default:
			result.conn.CloseWithError(0, "another path won")
		}
	}
	if winner == nil {
		return nil, fmt.Errorf("%w: %v", ErrNoPath, last)
	}
	return winner, nil
}
