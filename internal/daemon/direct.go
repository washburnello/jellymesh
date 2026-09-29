package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/natpath"
	"jellymesh/internal/node"
)

// Direct paths (assumption A-16, design-spec section 8). A member is reached
// over TCP at its advertised address; media prefers a direct QUIC path,
// arranged over that connection, and falls back to it. Playback never waits
// for a path: until one exists, and whenever it fails, media uses TCP.

const (
	// discoveryInterval refreshes the outside address, which a home
	// router may change.
	discoveryInterval = 15 * time.Minute
	// retryAfterFailure spaces attempts at a direct path to one peer. It
	// is longer than conntrack's 30 s for unanswered UDP, so a failed
	// punch's traces have cleared before the next.
	retryAfterFailure = 2 * time.Minute
	// attemptTimeout bounds one attempt, exchange and punch together.
	attemptTimeout = 20 * time.Second
)

var errNoOutsideAddress = errors.New("this node has no outside address to offer")

type directPaths struct {
	node     *Node
	endpoint *natpath.Endpoint
	acceptor *natpath.Acceptor

	mutex        sync.Mutex
	discovered   natpath.Result
	discoverErr  error
	discoveredAt time.Time
	peers        map[string]*peerPath

	// served counts media requests this node answered over direct paths.
	served atomic.Int64

	// offerInterval overrides natpath.MinOfferInterval, for tests.
	offerInterval time.Duration
}

type peerPath struct {
	client      *http.Client
	conn        *quic.Conn
	established time.Time
	attempting  bool
	failedUntil time.Time
	lastError   string
}

// openDirect starts the direct-path endpoint, or returns nil when direct
// paths are off or the socket cannot be opened, in which case media uses
// TCP only. A socket that cannot be opened (for example, a port another
// program holds) is reported by jellymesh status, not only logged.
func openDirect(n *Node) *directPaths {
	if n.cfg.DirectListenAddress == "" {
		return nil
	}
	endpoint, err := natpath.Listen(n.cfg.DirectListenAddress, n.identity.TLSCertificate())
	if err != nil {
		n.logger.Printf("direct paths off: %v", err)
		n.directError = err.Error()
		return nil
	}
	direct := &directPaths{node: n, endpoint: endpoint, acceptor: natpath.NewAcceptor(n.nodeID), peers: map[string]*peerPath{}}
	if n.cfg.DirectOfferLocal {
		direct.acceptor.AllowLocal()
	}
	return direct
}

func (direct *directPaths) close() {
	if direct != nil {
		direct.endpoint.Close()
	}
}

// discover refreshes this node's outside address, and reports whether it
// learned one.
func (direct *directPaths) discover(ctx context.Context) bool {
	if direct.node.cfg.DirectOfferLocal {
		return true // local candidates only; no STUN
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := direct.endpoint.Discover(ctx, direct.node.cfg.STUNServers)
	direct.mutex.Lock()
	direct.discovered, direct.discoverErr, direct.discoveredAt = result, err, time.Now()
	direct.mutex.Unlock()
	if err != nil {
		direct.node.logger.Printf("direct paths: STUN: %v", err)
	}
	return err == nil
}

// candidates are the addresses this node offers: its outside address, when
// its NAT keeps one mapping for every destination, and any configured.
func (direct *directPaths) candidates() ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	if direct.node.cfg.DirectOfferLocal {
		out = append(out, direct.endpoint.LocalAddr())
	}
	direct.mutex.Lock()
	if direct.discovered.Address.IsValid() && !direct.discovered.VariesByDestination {
		out = append(out, direct.discovered.Address)
	}
	direct.mutex.Unlock()
	for _, candidate := range direct.node.cfg.DirectCandidates {
		if parsed, err := netip.ParseAddrPort(candidate); err == nil {
			out = append(out, parsed)
		}
	}
	if len(out) == 0 {
		return nil, errNoOutsideAddress
	}
	return out, nil
}

// memberByKey names the member behind a request's TLS key, unless this node
// has blocked it. The federation listener already turns away a blocked
// member, because group trust consults the block list, so this is a second
// layer, for the day the route is reached another way.
func (direct *directPaths) memberByKey(runtime *groupRuntime, request *http.Request) (string, bool) {
	if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
		return "", false
	}
	fingerprint, err := node.FingerprintOfCertificate(request.TLS.PeerCertificates[0])
	if err != nil {
		return "", false
	}
	var memberID string
	_ = runtime.group.View(func(state *grouplog.State) {
		if member, ok := state.MemberByFingerprint(fingerprint); ok {
			memberID = member.NodeID
		}
	})
	if memberID == "" || memberID == direct.node.nodeID {
		return "", false
	}
	if blocked, err := direct.node.peers.IsBlocked(request.Context(), memberID); err != nil || blocked {
		return "", false
	}
	return memberID, true
}

// offerRoutes answers offers on the federation listener's member routes.
func (direct *directPaths) offerRoutes(runtime *groupRuntime) *natpath.Server {
	return &natpath.Server{
		GroupID:  runtime.id,
		Acceptor: direct.acceptor,
		Caller:   func(request *http.Request) (string, bool) { return direct.memberByKey(runtime, request) },
		Answer:   func(context.Context, string) ([]netip.AddrPort, error) { return direct.candidates() },
		Accepted: func(offer natpath.Offer, start time.Time) { go direct.answer(runtime, offer, start) },
		Refused: func(caller string, err error) {
			direct.node.audit.Record(context.Background(), caller, "direct_path_refused", caller, map[string]string{"node_id": caller, "reason": err.Error()})
		},
		Interval: direct.offerInterval,
	}
}

// answer punches toward a peer whose offer was accepted, at the agreed
// start, and serves the federation routes over the connection it makes.
func (direct *directPaths) answer(runtime *groupRuntime, offer natpath.Offer, start time.Time) {
	peer, err := direct.node.peer(runtime, offer.From)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
	defer cancel()
	wait := direct.endpoint.Expect(ctx, peer.Fingerprint, offer.Candidates)
	time.Sleep(time.Until(start))
	go direct.endpoint.Punch(ctx, offer.Candidates)
	conn, err := wait()
	if err != nil {
		direct.record("direct_path_failed", offer.From, "answered", err)
		return
	}
	direct.record("direct_path_opened", offer.From, "answered", nil)
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		direct.served.Add(1)
		direct.node.FederationHandler().ServeHTTP(response, request)
	})
	go natpath.Serve(conn, handler)
}

// client returns an HTTP client over a direct path to peerID if one is
// open. If none is, it starts an attempt in the background, unless one is
// running or the last failed recently, and reports false: the caller uses
// TCP this time.
func (direct *directPaths) client(peerID string) (*http.Client, bool) {
	if direct == nil {
		return nil, false
	}
	direct.mutex.Lock()
	defer direct.mutex.Unlock()
	path := direct.peers[peerID]
	if path == nil {
		path = &peerPath{}
		direct.peers[peerID] = path
	}
	if path.client != nil {
		return path.client, true
	}
	if !path.attempting && time.Now().After(path.failedUntil) {
		path.attempting = true
		go direct.attempt(peerID)
	}
	return nil, false
}

// failed drops a peer's direct path after a failure on it.
func (direct *directPaths) failed(peerID string, err error) {
	if direct == nil {
		return
	}
	direct.mutex.Lock()
	defer direct.mutex.Unlock()
	if path := direct.peers[peerID]; path != nil && path.client != nil {
		path.conn.CloseWithError(0, "failed")
		path.client, path.conn = nil, nil
		path.failedUntil = time.Now().Add(retryAfterFailure)
		path.lastError = err.Error()
	}
}

// record audits a direct path's outcome, with no address: identifiers and
// outcomes only, as C-OP-1 requires.
func (direct *directPaths) record(action string, peerID string, outcome string, err error) {
	detail := map[string]string{"node_id": peerID, "outcome": outcome}
	if err != nil {
		detail["reason"] = err.Error()
	}
	direct.node.audit.Record(context.Background(), "local", action, peerID, detail)
}

func (direct *directPaths) attempt(peerID string) {
	err := direct.open(peerID)
	if err != nil {
		direct.record("direct_path_failed", peerID, "dialed", err)
	} else {
		direct.record("direct_path_opened", peerID, "dialed", nil)
	}
	direct.mutex.Lock()
	defer direct.mutex.Unlock()
	path := direct.peers[peerID]
	path.attempting = false
	if err != nil {
		path.failedUntil = time.Now().Add(retryAfterFailure)
		path.lastError = err.Error()
	}
}

func (direct *directPaths) open(peerID string) error {
	n := direct.node
	runtime, err := n.current()
	if err != nil {
		return err
	}
	peer, err := n.peer(runtime, peerID)
	if err != nil {
		return err
	}
	candidates, err := direct.candidates()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
	defer cancel()
	offer, err := natpath.NewOffer(n.nodeID, peerID, candidates, time.Now())
	if err != nil {
		return err
	}
	answer, start, err := natpath.Exchange(ctx, n.client.HTTPClient(peer), "https://"+peer.Address, runtime.id, offer, direct.acceptor)
	if err != nil {
		return err
	}
	time.Sleep(time.Until(start))
	conn, err := direct.endpoint.Dial(ctx, peer.Fingerprint, answer.Candidates)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: natpath.RoundTripper(conn)}
	direct.mutex.Lock()
	path := direct.peers[peerID]
	path.client, path.conn, path.established, path.lastError = client, conn, time.Now(), ""
	direct.mutex.Unlock()
	// A path that dies is dropped; the next request uses TCP and retries.
	go func() {
		<-conn.Context().Done()
		direct.mutex.Lock()
		if path.conn == conn {
			path.client, path.conn = nil, nil
			path.failedUntil = time.Now().Add(retryAfterFailure)
			path.lastError = "the direct connection closed"
		}
		direct.mutex.Unlock()
	}()
	return nil
}

// closeAll drops every direct path; tests use it to force the fallback.
func (direct *directPaths) closeAll() {
	direct.mutex.Lock()
	defer direct.mutex.Unlock()
	for _, path := range direct.peers {
		if path.conn != nil {
			path.conn.CloseWithError(0, "closed")
			path.client, path.conn = nil, nil
		}
	}
}

// DirectStatus reports direct paths for `jellymesh status` (C-NT-6).
type DirectStatus struct {
	Enabled             bool             `json:"enabled"`
	OffReason           string           `json:"off_reason,omitempty"`
	LocalAddress        string           `json:"local_address,omitempty"`
	OutsideAddress      string           `json:"outside_address,omitempty"`
	VariesByDestination bool             `json:"varies_by_destination,omitempty"`
	DiscoveryError      string           `json:"discovery_error,omitempty"`
	ServedDirect        int64            `json:"served_direct"`
	Peers               []PeerPathStatus `json:"peers,omitempty"`
}

// PeerPathStatus is how media reaches one peer.
type PeerPathStatus struct {
	NodeID    string `json:"node_id"`
	Path      string `json:"path"` // "direct" or "tcp"
	Since     string `json:"since,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func (direct *directPaths) status() DirectStatus {
	if direct == nil {
		return DirectStatus{}
	}
	direct.mutex.Lock()
	defer direct.mutex.Unlock()
	status := DirectStatus{Enabled: true, LocalAddress: direct.endpoint.LocalAddr().String(), ServedDirect: direct.served.Load()}
	if direct.discovered.Address.IsValid() {
		status.OutsideAddress = direct.discovered.Address.String()
		status.VariesByDestination = direct.discovered.VariesByDestination
	}
	if direct.discoverErr != nil {
		status.DiscoveryError = direct.discoverErr.Error()
	}
	for id, path := range direct.peers {
		peer := PeerPathStatus{NodeID: id, Path: "tcp", LastError: path.lastError}
		if path.client != nil {
			peer.Path, peer.Since = "direct", path.established.UTC().Format(time.RFC3339)
		}
		status.Peers = append(status.Peers, peer)
	}
	sort.Slice(status.Peers, func(i, j int) bool { return status.Peers[i].NodeID < status.Peers[j].NodeID })
	return status
}
