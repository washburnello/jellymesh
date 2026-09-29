package natpath

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"sync"
	"time"
)

// Two members arrange a direct path by exchanging offers over the mutually
// authenticated connection to the source's advertised address (A-16). The
// connection proves who is speaking, so an offer carries no signature of
// its own. It must name the connection's caller and the receiving node as
// its two ends, and it must be fresh and never seen before (C-NT-3).

// OfferLifetime is how long an offer may be acted on. Punching starts at
// once, so an offer older than this is stale or replayed.
const OfferLifetime = 20 * time.Second

// clockSkew tolerates members' clocks disagreeing. Home servers' clocks
// can be off by seconds; the TLS connection and the nonce, not the expiry,
// are what stop a replay.
const clockSkew = 60 * time.Second

// StartLead is how far ahead the answering node schedules the punch. Both
// sides must start sending within much less than the one-way delay between
// them: a common NAT (Linux conntrack) records a peer's packet that arrives
// before its own node has sent anything, then moves that node's outgoing
// packets to another port, and the punch fails (NAT lab, #54). The lead
// covers the answer's trip back to the caller.
const StartLead = 500 * time.Millisecond

// maxCandidates bounds the addresses an offer may name. Punching sends UDP
// to each of them, so a member must not be able to aim it at many hosts.
const maxCandidates = 8

var (
	ErrOfferNotFromPeer    = errors.New("the offer does not come from the connection's caller")
	ErrOfferForAnotherPair = errors.New("the offer is not addressed to this node")
	ErrOfferExpired        = errors.New("the offer has expired or claims too long a life")
	ErrOfferReplayed       = errors.New("the offer has been seen before")
	ErrOfferInvalid        = errors.New("the offer's candidates are not usable")
)

// Offer is one side's invitation to punch: where it may be reached.
type Offer struct {
	From       string           `json:"from"`
	To         string           `json:"to"`
	Candidates []netip.AddrPort `json:"candidates"`
	Nonce      string           `json:"nonce"`
	Expires    time.Time        `json:"expires"`
	// Sent is the sender's clock when it made the offer. An answer's Sent
	// lets the caller estimate how far the two clocks differ.
	Sent time.Time `json:"sent"`
	// Start, in an answer, is when both sides begin punching, by the
	// answering node's clock.
	Start time.Time `json:"start,omitempty"`
}

// NewOffer builds an offer from one node to another, valid for
// OfferLifetime from now.
func NewOffer(from string, to string, candidates []netip.AddrPort, now time.Time) (Offer, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Offer{}, err
	}
	return Offer{From: from, To: to, Candidates: candidates, Nonce: hex.EncodeToString(nonce), Expires: now.Add(OfferLifetime), Sent: now}, nil
}

// Acceptor checks offers arriving for one node and remembers their nonces
// for as long as they could be replayed.
type Acceptor struct {
	self  string
	local bool // accept loopback candidates: nodes on one host only
	now   func() time.Time
	mutex sync.Mutex
	seen  map[string]time.Time // nonce -> when it can be forgotten
}

// NewAcceptor returns an acceptor for node self.
func NewAcceptor(self string) *Acceptor {
	return &Acceptor{self: self, now: time.Now, seen: map[string]time.Time{}}
}

// AllowLocal lets offers name loopback addresses, for nodes on one host
// such as tests. Never set it for a node that talks to other hosts.
func (acceptor *Acceptor) AllowLocal() { acceptor.local = true }

// Accept checks an offer that arrived from the authenticated member caller.
func (acceptor *Acceptor) Accept(caller string, offer Offer) error {
	if caller == "" || offer.From != caller {
		return ErrOfferNotFromPeer
	}
	if offer.To != acceptor.self {
		return ErrOfferForAnotherPair
	}
	now := acceptor.now()
	if !now.Add(-clockSkew).Before(offer.Expires) || offer.Expires.After(now.Add(OfferLifetime+clockSkew)) {
		return ErrOfferExpired
	}
	if err := checkCandidates(offer.Candidates, acceptor.local); err != nil {
		return err
	}
	if len(offer.Nonce) != 32 {
		return fmt.Errorf("%w: malformed nonce", ErrOfferInvalid)
	}
	acceptor.mutex.Lock()
	defer acceptor.mutex.Unlock()
	for nonce, forget := range acceptor.seen {
		if now.After(forget) {
			delete(acceptor.seen, nonce)
		}
	}
	if _, replayed := acceptor.seen[offer.Nonce]; replayed {
		return ErrOfferReplayed
	}
	acceptor.seen[offer.Nonce] = offer.Expires.Add(clockSkew)
	return nil
}

func checkCandidates(candidates []netip.AddrPort, local bool) error {
	if len(candidates) == 0 || len(candidates) > maxCandidates {
		return fmt.Errorf("%w: %d candidates", ErrOfferInvalid, len(candidates))
	}
	for _, candidate := range candidates {
		address := candidate.Addr()
		if !candidate.IsValid() || candidate.Port() == 0 || address.IsUnspecified() || address.IsMulticast() ||
			(address.IsLoopback() && !local) || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
			return fmt.Errorf("%w: %v", ErrOfferInvalid, candidate)
		}
	}
	return nil
}

// OfferPath is the member-only route on which a node answers offers.
const OfferPath = "/jellymesh/v1/groups/{group}/path/offer"

// Server answers offers on the federation listener's member routes.
type Server struct {
	GroupID  string
	Acceptor *Acceptor
	// Caller names the authenticated member behind a request, or reports
	// that there is none (not a member, or blocked).
	Caller func(*http.Request) (string, bool)
	// Answer returns this node's candidates for a peer.
	Answer func(ctx context.Context, peer string) ([]netip.AddrPort, error)
	// Accepted is told of each accepted offer and when, by this node's
	// clock, punching begins.
	Accepted func(offer Offer, start time.Time)
}

// Register adds the offer route for members only.
func (server *Server) Register(_ *http.ServeMux, members *http.ServeMux) {
	members.HandleFunc("POST "+OfferPath, server.offer)
}

func (server *Server) offer(response http.ResponseWriter, request *http.Request) {
	caller, ok := server.Caller(request)
	if request.PathValue("group") != server.GroupID || !ok {
		http.NotFound(response, request)
		return
	}
	var offer Offer
	decoder := json.NewDecoder(io.LimitReader(request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&offer); err != nil {
		http.Error(response, "malformed offer", http.StatusBadRequest)
		return
	}
	if err := server.Acceptor.Accept(caller, offer); err != nil {
		http.Error(response, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	candidates, err := server.Answer(request.Context(), caller)
	if err != nil {
		http.Error(response, "no path available", http.StatusServiceUnavailable)
		return
	}
	now := server.Acceptor.now()
	answer, err := NewOffer(server.Acceptor.self, caller, candidates, now)
	if err != nil {
		http.Error(response, "no path available", http.StatusServiceUnavailable)
		return
	}
	answer.Start = now.Add(StartLead)
	if server.Accepted != nil {
		server.Accepted(offer, answer.Start)
	}
	response.Header().Set("Content-Type", "application/json")
	json.NewEncoder(response).Encode(answer)
}

// Exchange sends offer to the peer at baseURL over client, which must be
// the mutually authenticated connection to that peer, and returns the
// peer's answer once it has passed the same checks as an incoming offer,
// with the punch's start converted to this node's clock. The clocks are
// compared as NTP does: the peer's clock when it answered, against the
// midpoint of the request's round trip here, so neither node needs an
// accurate clock.
func Exchange(ctx context.Context, client *http.Client, baseURL string, groupID string, offer Offer, acceptor *Acceptor) (Offer, time.Time, error) {
	body, err := json.Marshal(offer)
	if err != nil {
		return Offer{}, time.Time{}, err
	}
	url := baseURL + "/jellymesh/v1/groups/" + groupID + "/path/offer"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Offer{}, time.Time{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	// Time only the request's own round trip: from the moment it has been
	// written to the first byte of the answer. Connection setup (TCP, TLS)
	// happens before that and would skew the clock estimate by its round
	// trips, which is enough to lose the punch (NAT lab, #54).
	var sent, firstByte time.Time
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		WroteRequest:         func(httptrace.WroteRequestInfo) { sent = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}))
	response, err := client.Do(request)
	if err != nil {
		return Offer{}, time.Time{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return Offer{}, time.Time{}, fmt.Errorf("the peer refused the offer: %d %s", response.StatusCode, bytes.TrimSpace(message))
	}
	var answer Offer
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		return Offer{}, time.Time{}, fmt.Errorf("malformed answer: %w", err)
	}
	received := time.Now()
	if sent.IsZero() || firstByte.IsZero() {
		sent, firstByte = received, received
	}
	if err := acceptor.Accept(offer.To, answer); err != nil {
		return Offer{}, time.Time{}, err
	}
	if answer.Start.IsZero() || answer.Start.Before(answer.Sent) || answer.Start.After(answer.Sent.Add(OfferLifetime)) {
		return Offer{}, time.Time{}, fmt.Errorf("%w: no usable start time", ErrOfferInvalid)
	}
	// The peer's clock minus this one's, at the round trip's midpoint.
	offset := answer.Sent.Sub(sent.Add(firstByte.Sub(sent) / 2))
	start := answer.Start.Add(-offset)
	if start.Before(received) {
		start = received // the round trip outlasted the lead; start now
	}
	return answer, start, nil
}
