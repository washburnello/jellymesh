package natpath

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

// fakeServer answers binding requests on loopback. It reports the sender's
// real address, or, to emulate a NAT, the address report returns for it.
type fakeServer struct {
	conn   net.PacketConn
	report func(sender netip.AddrPort) netip.AddrPort
	xor    bool
}

func startServer(t *testing.T, report func(netip.AddrPort) netip.AddrPort, xor bool) *fakeServer {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	server := &fakeServer{conn: conn, report: report, xor: xor}
	go server.serve()
	return server
}

func (s *fakeServer) address() string { return s.conn.LocalAddr().String() }

func (s *fakeServer) serve() {
	buffer := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		if n < headerLength || binary.BigEndian.Uint16(buffer[0:2]) != bindingRequest {
			continue
		}
		var id transactionID
		copy(id[:], buffer[8:20])
		sender, _ := addrPortOf(from)
		if s.report != nil {
			sender = s.report(sender)
		}
		s.conn.WriteTo(response(id, sender, s.xor), from)
	}
}

// response builds a binding success carrying mapped.
func response(id transactionID, mapped netip.AddrPort, xor bool) []byte {
	value := make([]byte, 8)
	value[1] = 0x01
	port := mapped.Port()
	ip := mapped.Addr().As4()
	kind := uint16(attrMappedAddress)
	if xor {
		kind = attrXORMappedAddr
		port ^= magicCookie >> 16
		var cookie [4]byte
		binary.BigEndian.PutUint32(cookie[:], magicCookie)
		for i := range ip {
			ip[i] ^= cookie[i]
		}
	}
	binary.BigEndian.PutUint16(value[2:4], port)
	copy(value[4:8], ip[:])
	message := make([]byte, headerLength, headerLength+12)
	binary.BigEndian.PutUint16(message[0:2], bindingSuccess)
	binary.BigEndian.PutUint16(message[2:4], 12)
	binary.BigEndian.PutUint32(message[4:8], magicCookie)
	copy(message[8:20], id[:])
	attribute := make([]byte, 4)
	binary.BigEndian.PutUint16(attribute[0:2], kind)
	binary.BigEndian.PutUint16(attribute[2:4], 8)
	return append(append(message, attribute...), value...)
}

func socket(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func context2s(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// C-NT-2: the outside address is learned for the very socket that asks, and
// both the XOR-mapped and the legacy mapped attributes are understood.
func TestDiscoveryLearnsTheAskingSocketsOutsideAddress(t *testing.T) {
	conn := socket(t)
	a := startServer(t, nil, true)
	b := startServer(t, nil, false)
	result, err := Discover(context2s(t), conn, []string{a.address(), b.address()})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := addrPortOf(conn.LocalAddr())
	if result.Address != local || len(result.Mapped) != 2 || result.VariesByDestination {
		t.Fatalf("want %v from both servers, same mapping: %+v", local, result)
	}
}

// C-NT-2: a NAT that gives each destination its own outside port is
// recognised, and one that keeps the mapping is not.
func TestDiscoveryTellsWhetherTheMappingVariesByDestination(t *testing.T) {
	outside := netip.MustParseAddr("198.51.100.7")
	easy := func(sender netip.AddrPort) netip.AddrPort { return netip.AddrPortFrom(outside, 40000) }
	a := startServer(t, easy, true)
	b := startServer(t, easy, true)
	result, err := Discover(context2s(t), socket(t), []string{a.address(), b.address()})
	if err != nil || result.VariesByDestination || result.Address != netip.AddrPortFrom(outside, 40000) {
		t.Fatalf("an endpoint-independent mapping: %+v, %v", result, err)
	}

	port := uint16(40000)
	hard := func(sender netip.AddrPort) netip.AddrPort { port++; return netip.AddrPortFrom(outside, port) }
	c := startServer(t, hard, true)
	d := startServer(t, hard, true)
	result, err = Discover(context2s(t), socket(t), []string{c.address(), d.address()})
	if err != nil || !result.VariesByDestination {
		t.Fatalf("a destination-dependent mapping must be reported: %+v, %v", result, err)
	}
}

// C-NT-2: only a server that was asked, answering the transaction it was
// sent, is believed; stray and forged packets on the socket are ignored.
func TestDiscoveryIgnoresPacketsItDidNotAskFor(t *testing.T) {
	conn := socket(t)
	liar, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer liar.Close()
	silent := socket(t) // asked, never answers itself
	honest := startServer(t, nil, true)
	target := conn.LocalAddr()
	forged := netip.MustParseAddrPort("203.0.113.9:9")
	go func() {
		// Someone who saw the request to the silent server answers it with
		// the right transaction ID, but from another address.
		buffer := make([]byte, 1500)
		n, _, err := silent.ReadFrom(buffer)
		if err != nil || n < headerLength {
			return
		}
		var seen transactionID
		copy(seen[:], buffer[8:20])
		for i := 0; i < 20; i++ {
			liar.WriteTo(response(seen, forged, true), target)
			liar.WriteTo(response(transactionID{}, forged, true), target)
			liar.WriteTo([]byte("not stun at all"), target)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	result, err := Discover(context2s(t), conn, []string{silent.LocalAddr().String(), honest.address()})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := addrPortOf(conn.LocalAddr())
	if result.Address != local || len(result.Mapped) != 1 {
		t.Fatalf("a forged or unasked answer was believed: %+v", result)
	}
	if _, err := parse(response(transactionID{1}, local, true), transactionID{2}); err == nil {
		t.Fatal("an answer to another transaction must be refused")
	}
}

// C-NT-2: when no server answers, discovery says so rather than inventing an
// address, and it gives up at the deadline.
func TestDiscoveryFailsCleanlyWhenNoServerAnswers(t *testing.T) {
	silent := socket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := Discover(ctx, socket(t), []string{silent.LocalAddr().String()}); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", err)
	}
	if time.Since(began) > time.Second {
		t.Fatal("discovery ignored its deadline")
	}
	if _, err := Discover(ctx, socket(t), nil); !errors.Is(err, ErrNoServers) {
		t.Fatalf("want ErrNoServers, got %v", err)
	}
}
