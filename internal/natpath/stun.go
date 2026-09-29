// Package natpath finds how a node looks from outside its NAT, so that two
// members can open a direct UDP path between their homes (design-spec
// section 8, "Reachability and direct media paths"; assumption A-16).
//
// Discovery uses STUN binding requests (RFC 8489) sent from the same UDP
// socket the node's QUIC transport will use, because a NAT maps each socket
// separately: an address learned on any other socket would be the wrong one.
// Asking two servers also shows whether the NAT gives each destination its
// own outside port, which defeats hole punching.
package natpath

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

const (
	magicCookie         = 0x2112A442
	bindingRequest      = 0x0001
	bindingSuccess      = 0x0101
	attrMappedAddress   = 0x0001
	attrXORMappedAddr   = 0x0020
	headerLength        = 20
	maxResponseLength   = 1500
	defaultQueryTimeout = 3 * time.Second
)

var (
	ErrNoServers  = errors.New("no STUN servers are configured")
	ErrNoResponse = errors.New("no STUN server answered")
)

// Result is what discovery learned.
type Result struct {
	// Mapped is the outside address each answering server saw, by server.
	Mapped map[string]netip.AddrPort
	// Address is the outside address to offer peers: the one the first
	// answering server, in configuration order, saw.
	Address netip.AddrPort
	// VariesByDestination is true when two servers saw different outside
	// addresses for this one socket. Hole punching then cannot be relied on,
	// and media falls back to the advertised address (A-16).
	VariesByDestination bool
}

type transactionID [12]byte

// request builds a binding request.
func request(id transactionID) []byte {
	message := make([]byte, headerLength)
	binary.BigEndian.PutUint16(message[0:2], bindingRequest)
	binary.BigEndian.PutUint16(message[2:4], 0)
	binary.BigEndian.PutUint32(message[4:8], magicCookie)
	copy(message[8:20], id[:])
	return message
}

// parse returns the mapped address in a binding success response for id.
func parse(message []byte, id transactionID) (netip.AddrPort, error) {
	if len(message) < headerLength {
		return netip.AddrPort{}, errors.New("short STUN message")
	}
	if binary.BigEndian.Uint16(message[0:2]) != bindingSuccess {
		return netip.AddrPort{}, errors.New("not a binding success response")
	}
	if binary.BigEndian.Uint32(message[4:8]) != magicCookie {
		return netip.AddrPort{}, errors.New("not a STUN message")
	}
	if [12]byte(message[8:20]) != id {
		return netip.AddrPort{}, errors.New("transaction ID does not match")
	}
	length := int(binary.BigEndian.Uint16(message[2:4]))
	if headerLength+length > len(message) {
		return netip.AddrPort{}, errors.New("truncated STUN message")
	}
	var fallback netip.AddrPort
	for attributes := message[headerLength : headerLength+length]; len(attributes) >= 4; {
		kind := binary.BigEndian.Uint16(attributes[0:2])
		size := int(binary.BigEndian.Uint16(attributes[2:4]))
		if 4+size > len(attributes) {
			break
		}
		value := attributes[4 : 4+size]
		switch kind {
		case attrXORMappedAddr:
			if address, ok := decodeAddress(value, true, id); ok {
				return address, nil
			}
		case attrMappedAddress:
			if address, ok := decodeAddress(value, false, id); ok {
				fallback = address
			}
		}
		padded := (size + 3) &^ 3
		if 4+padded > len(attributes) {
			break
		}
		attributes = attributes[4+padded:]
	}
	if fallback.IsValid() {
		return fallback, nil
	}
	return netip.AddrPort{}, errors.New("no mapped address in response")
}

func decodeAddress(value []byte, xored bool, id transactionID) (netip.AddrPort, bool) {
	if len(value) < 8 {
		return netip.AddrPort{}, false
	}
	family := value[1]
	port := binary.BigEndian.Uint16(value[2:4])
	if xored {
		port ^= magicCookie >> 16
	}
	switch family {
	case 0x01:
		ip := [4]byte(value[4:8])
		if xored {
			var cookie [4]byte
			binary.BigEndian.PutUint32(cookie[:], magicCookie)
			for i := range ip {
				ip[i] ^= cookie[i]
			}
		}
		return netip.AddrPortFrom(netip.AddrFrom4(ip), port), true
	case 0x02:
		if len(value) < 20 {
			return netip.AddrPort{}, false
		}
		ip := [16]byte(value[4:20])
		if xored {
			var key [16]byte
			binary.BigEndian.PutUint32(key[0:4], magicCookie)
			copy(key[4:], id[:])
			for i := range ip {
				ip[i] ^= key[i]
			}
		}
		return netip.AddrPortFrom(netip.AddrFrom16(ip).Unmap(), port), true
	}
	return netip.AddrPort{}, false
}

// Discover asks each server, from conn, how conn looks from outside. Answers
// are accepted only from the address each server resolved to and only for
// the transaction sent to it; anything else arriving on conn is ignored. It
// returns once every server has answered or ctx expires, and fails only if
// none answered.
//
// Discover reads from conn directly, so it must run before another user of
// the socket (such as a QUIC transport) starts reading, or be handed that
// user's non-QUIC packets instead.
func Discover(ctx context.Context, conn net.PacketConn, servers []string) (Result, error) {
	send := func(b []byte, to net.Addr) error { _, err := conn.WriteTo(b, to); return err }
	receive := func(ctx context.Context, b []byte) (int, net.Addr, error) {
		deadline, _ := ctx.Deadline()
		if err := conn.SetReadDeadline(deadline); err != nil {
			return 0, nil, err
		}
		return conn.ReadFrom(b)
	}
	defer conn.SetReadDeadline(time.Time{})
	return discover(ctx, send, receive, servers)
}

// discover is Discover over any way of sending datagrams from, and
// receiving non-QUIC datagrams on, one socket.
func discover(ctx context.Context, send func([]byte, net.Addr) error, receive func(context.Context, []byte) (int, net.Addr, error), servers []string) (Result, error) {
	if len(servers) == 0 {
		return Result{}, ErrNoServers
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultQueryTimeout)
		defer cancel()
	}
	type query struct {
		server  string
		address netip.AddrPort
		id      transactionID
	}
	var queries []query
	for _, server := range servers {
		// IPv4 first: the outside addresses offered to peers are IPv4, and a
		// host with no IPv6 route (common, including Docker's default
		// networks) cannot send to a server's IPv6 address at all.
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", hostOf(server))
		if err != nil || len(resolved) == 0 {
			if resolved, err = net.DefaultResolver.LookupNetIP(ctx, "ip", hostOf(server)); err != nil || len(resolved) == 0 {
				continue
			}
		}
		port, err := portOf(server)
		if err != nil {
			continue
		}
		var id transactionID
		if _, err := rand.Read(id[:]); err != nil {
			return Result{}, err
		}
		address := netip.AddrPortFrom(resolved[0].Unmap(), port)
		if err := send(request(id), net.UDPAddrFromAddrPort(address)); err != nil {
			continue
		}
		queries = append(queries, query{server, address, id})
	}
	result := Result{Mapped: map[string]netip.AddrPort{}}
	buffer := make([]byte, maxResponseLength)
	for len(result.Mapped) < len(queries) {
		n, from, err := receive(ctx, buffer)
		if err != nil {
			break // the deadline, or the socket closed
		}
		sender, ok := addrPortOf(from)
		if !ok {
			continue
		}
		for _, q := range queries {
			if q.address != sender {
				continue
			}
			if mapped, err := parse(buffer[:n], q.id); err == nil {
				result.Mapped[q.server] = mapped
			}
		}
	}
	if len(result.Mapped) == 0 {
		return Result{}, ErrNoResponse
	}
	for _, q := range queries {
		mapped, ok := result.Mapped[q.server]
		if !ok {
			continue
		}
		if !result.Address.IsValid() {
			result.Address = mapped
		} else if mapped != result.Address {
			result.VariesByDestination = true
		}
	}
	return result, nil
}

func hostOf(server string) string {
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return server
	}
	return host
}

func portOf(server string) (uint16, error) {
	_, port, err := net.SplitHostPort(server)
	if err != nil {
		return 3478, nil // the STUN default
	}
	var value uint16
	if _, err := fmt.Sscan(port, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func addrPortOf(address net.Addr) (netip.AddrPort, bool) {
	udp, ok := address.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	parsed := udp.AddrPort()
	return netip.AddrPortFrom(parsed.Addr().Unmap(), parsed.Port()), true
}
