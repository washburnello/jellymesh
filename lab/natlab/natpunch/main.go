// Command natpunch runs one side of the NAT lab (#54, C-NT-4) using the
// real internal/natpath code, or the lab's STUN server.
//
//	natpunch stun  -listen :3478
//	natpunch node  -name a -peer b -role client|server -stun host:3478 -shared /shared
//
// A node creates an identity, opens a direct-path endpoint, learns its
// outside address by STUN, publishes {fingerprint, candidates} to the shared
// directory (standing in for the signalling connection), waits for its
// peer's, then punches. The client dials and sends 32 MiB, which the server
// checks. Each node writes its outcome to <shared>/<name>.result.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"jellymesh/internal/enrollment"
	"jellymesh/internal/natpath"
	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

type published struct {
	Fingerprint node.Fingerprint `json:"fingerprint"`
	Candidates  []netip.AddrPort `json:"candidates"`
	Varies      bool             `json:"varies"`
}

type outcome struct {
	OK      bool    `json:"ok"`
	Error   string  `json:"error,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	MBps    float64 `json:"mbps,omitempty"`
	Varies  bool    `json:"varies"`
	Mapped  string  `json:"mapped,omitempty"`
}

const payloadSize = 32 << 20

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: natpunch stun|node ...")
	}
	switch os.Args[1] {
	case "stun":
		stun(os.Args[2:])
	case "node":
		runNode(os.Args[2:])
	case "stunprobe":
		stunProbe(os.Args[2:])
	case "reach":
		reach(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

// stun answers binding requests with the sender's address, as seen here.
func stun(args []string) {
	flags := flag.NewFlagSet("stun", flag.ExitOnError)
	listen := flags.String("listen", ":3478", "listen address")
	flags.Parse(args)
	conn, err := net.ListenPacket("udp4", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("stun on %s", conn.LocalAddr())
	buffer := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buffer)
		if err != nil {
			log.Fatal(err)
		}
		if n < 20 || binary.BigEndian.Uint16(buffer[0:2]) != 0x0001 {
			continue
		}
		sender := from.(*net.UDPAddr).AddrPort()
		ip := sender.Addr().Unmap().As4()
		response := make([]byte, 32)
		binary.BigEndian.PutUint16(response[0:2], 0x0101)
		binary.BigEndian.PutUint16(response[2:4], 12)
		binary.BigEndian.PutUint32(response[4:8], 0x2112A442)
		copy(response[8:20], buffer[8:20])
		binary.BigEndian.PutUint16(response[20:22], 0x0020)
		binary.BigEndian.PutUint16(response[22:24], 8)
		response[25] = 0x01
		binary.BigEndian.PutUint16(response[26:28], sender.Port()^0x2112)
		cookie := []byte{0x21, 0x12, 0xA4, 0x42}
		for i := range ip {
			response[28+i] = ip[i] ^ cookie[i]
		}
		conn.WriteTo(response, from)
	}
}

// reach performs the check an approver makes before admitting a node
// (C-NT-1): a TLS handshake to address that must present the key with
// fingerprint. Use it to confirm an advertised address works from outside,
// through a port forward or Tailscale Funnel.
func reach(args []string) {
	flags := flag.NewFlagSet("reach", flag.ExitOnError)
	address := flags.String("address", "", "host:port to reach")
	fingerprint := flags.String("fingerprint", "", "the node's key fingerprint")
	connect := flags.String("connect", "", "connect to this IP instead of resolving the address, still sending its name (to test a public path from a host whose DNS gives a private one)")
	flags.Parse(args)
	dir, _ := os.MkdirTemp("", "reach")
	id, err := node.LoadOrCreate(filepath.Join(dir, "node.key"), filepath.Join(dir, "node.crt"), "reach")
	if err != nil {
		log.Fatal(err)
	}
	began := time.Now()
	if *connect != "" {
		host, port, _ := net.SplitHostPort(*address)
		want := node.Fingerprint(*fingerprint)
		config := transport.ClientTLSConfig(id.TLSCertificate(), want, transport.NewMemoryTrustStore(want))
		config.ServerName = host
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
		conn, dialErr := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort(*connect, port))
		if dialErr == nil {
			conn.Close()
		}
		fmt.Printf("%s via %s: %v (%v)\n", *address, *connect, map[bool]string{true: "the pinned key answers", false: "FAILED: " + fmt.Sprint(dialErr)}[dialErr == nil], time.Since(began).Round(time.Millisecond))
		return
	}
	err = enrollment.CheckAdvertisedAddress(context.Background(), id, *address, node.Fingerprint(*fingerprint))
	fmt.Printf("%s: %v (%v)\n", *address, map[bool]string{true: "the pinned key answers", false: "FAILED: " + fmt.Sprint(err)}[err == nil], time.Since(began).Round(time.Millisecond))
}

// stunProbe runs one STUN discovery from a UDP socket and prints the
// result, to diagnose a node whose discovery fails.
func stunProbe(args []string) {
	flags := flag.NewFlagSet("stunprobe", flag.ExitOnError)
	listen := flags.String("listen", "0.0.0.0:0", "local UDP address")
	servers := flags.String("stun", "stun.l.google.com:19302,stun.cloudflare.com:3478", "STUN servers")
	useEndpoint := flags.Bool("endpoint", false, "use a natpath.Endpoint (QUIC and STUN on one socket), as the daemon does")
	flags.Parse(args)
	if *useEndpoint {
		dir, _ := os.MkdirTemp("", "probe")
		id, err := node.LoadOrCreate(filepath.Join(dir, "node.key"), filepath.Join(dir, "node.crt"), "probe")
		if err != nil {
			log.Fatal(err)
		}
		endpoint, err := natpath.Listen(*listen, id.TLSCertificate())
		if err != nil {
			log.Fatal(err)
		}
		defer endpoint.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := endpoint.Discover(ctx, splitList(*servers))
		fmt.Printf("endpoint %s -> %+v, error %v\n", endpoint.LocalAddr(), result, err)
		return
	}
	conn, err := net.ListenPacket("udp4", *listen)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := natpath.Discover(ctx, conn, splitList(*servers))
	fmt.Printf("local %s -> %+v, error %v\n", conn.LocalAddr(), result, err)
}

func runNode(args []string) {
	flags := flag.NewFlagSet("node", flag.ExitOnError)
	name := flags.String("name", "", "this node's name")
	peer := flags.String("peer", "", "the peer's name")
	role := flags.String("role", "client", "client dials; server accepts")
	stunServers := flags.String("stun", "", "comma-separated STUN servers")
	shared := flags.String("shared", "/shared", "directory shared with the peer")
	signal := flags.String("signal", "", "the server's forwarded signalling address, host:port (client)")
	flags.Parse(args)
	result := outcome{}
	defer func() {
		encoded, _ := json.Marshal(result)
		os.WriteFile(filepath.Join(*shared, *name+".result"), encoded, 0o644)
		log.Printf("result %s", encoded)
	}()

	dir, _ := os.MkdirTemp("", "natpunch")
	identity, err := node.LoadOrCreate(filepath.Join(dir, "node.key"), filepath.Join(dir, "node.crt"), *name+".lab")
	if err != nil {
		result.Error = err.Error()
		return
	}
	endpoint, err := natpath.Listen("0.0.0.0:41641", identity.TLSCertificate())
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer endpoint.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	discovered, err := endpoint.Discover(ctx, splitList(*stunServers))
	cancel()
	if err != nil {
		result.Error = "discover: " + err.Error()
		return
	}
	result.Varies, result.Mapped = discovered.VariesByDestination, discovered.Address.String()
	mine := published{Fingerprint: identity.Fingerprint(), Candidates: []netip.AddrPort{discovered.Address}, Varies: discovered.VariesByDestination}
	encoded, _ := json.Marshal(mine)
	os.WriteFile(filepath.Join(*shared, *name+".json.tmp"), encoded, 0o644)
	os.Rename(filepath.Join(*shared, *name+".json.tmp"), filepath.Join(*shared, *name+".json"))

	// The shared directory stands in for the roster: it gives each node the
	// other's key fingerprint. Offers travel over HTTP to the server's
	// forwarded port, as they would to its advertised address.
	var theirs published
	for deadline := time.Now().Add(60 * time.Second); ; {
		if raw, err := os.ReadFile(filepath.Join(*shared, *peer+".json")); err == nil && json.Unmarshal(raw, &theirs) == nil {
			break
		}
		if time.Now().After(deadline) {
			result.Error = "the peer never published"
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	began := time.Now()
	if *role == "server" {
		accepted := make(chan struct {
			offer natpath.Offer
			start time.Time
		}, 1)
		server := &natpath.Server{
			GroupID:  "lab",
			Acceptor: natpath.NewAcceptor(*name),
			Caller: func(request *http.Request) (string, bool) {
				return request.Header.Get("X-Lab-Node"), request.Header.Get("X-Lab-Node") == *peer
			},
			Answer: func(context.Context, string) ([]netip.AddrPort, error) { return mine.Candidates, nil },
			Accepted: func(offer natpath.Offer, start time.Time) {
				accepted <- struct {
					offer natpath.Offer
					start time.Time
				}{offer, start}
			},
		}
		members := http.NewServeMux()
		server.Register(nil, members)
		go http.ListenAndServe(":8443", members)
		var got struct {
			offer natpath.Offer
			start time.Time
		}
		select {
		case got = <-accepted:
		case <-ctx.Done():
			result.Error = "no offer arrived"
			return
		}
		wait := endpoint.Expect(ctx, theirs.Fingerprint, got.offer.Candidates)
		time.Sleep(time.Until(got.start))
		go endpoint.Punch(ctx, got.offer.Candidates)
		conn, err := wait()
		if err != nil {
			result.Error = "accept: " + err.Error()
			return
		}
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			result.Error = "stream: " + err.Error()
			return
		}
		sum := sha256.New()
		n, err := io.Copy(sum, stream)
		if err != nil || n != payloadSize {
			result.Error = fmt.Sprintf("received %d bytes: %v", n, err)
			return
		}
		stream.Write([]byte(hex.EncodeToString(sum.Sum(nil))))
		stream.Close()
		time.Sleep(time.Second) // let the confirmation leave before closing
	} else {
		offer, err := natpath.NewOffer(*name, *peer, mine.Candidates, time.Now())
		if err != nil {
			result.Error = err.Error()
			return
		}
		client := &http.Client{Transport: labHeader{*name}, Timeout: 10 * time.Second}
		answer, start, err := natpath.Exchange(ctx, client, "http://"+*signal, "lab", offer, natpath.NewAcceptor(*name))
		if err != nil {
			result.Error = "exchange: " + err.Error()
			return
		}
		time.Sleep(time.Until(start))
		conn, err := endpoint.Dial(ctx, theirs.Fingerprint, answer.Candidates)
		if err != nil {
			result.Error = "dial: " + err.Error()
			return
		}
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			result.Error = "stream: " + err.Error()
			return
		}
		payload := make([]byte, payloadSize)
		rand.New(rand.NewSource(1)).Read(payload)
		want := sha256.Sum256(payload)
		sent := time.Now()
		stream.Write(payload)
		stream.Close()
		confirmation, _ := io.ReadAll(stream)
		if string(confirmation) != hex.EncodeToString(want[:]) {
			result.Error = "the server's checksum did not match"
			return
		}
		result.MBps = float64(payloadSize) / 1e6 / time.Since(sent).Seconds()
	}
	result.OK, result.Seconds = true, time.Since(began).Seconds()
}

// labHeader names the calling node, standing in for its TLS identity.
type labHeader struct{ name string }

func (h labHeader) RoundTrip(request *http.Request) (*http.Response, error) {
	request.Header.Set("X-Lab-Node", h.name)
	return http.DefaultTransport.RoundTrip(request)
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
