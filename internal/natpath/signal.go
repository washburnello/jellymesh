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

// clockSkew tolerates members' clocks disagreeing slightly.
const clockSkew = 5 * time.Second

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
}

// NewOffer builds an offer from one node to another, valid for
// OfferLifetime from now.
func NewOffer(from string, to string, candidates []netip.AddrPort, now time.Time) (Offer, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Offer{}, err
	}
	return Offer{From: from, To: to, Candidates: candidates, Nonce: hex.EncodeToString(nonce), Expires: now.Add(OfferLifetime)}, nil
}

// Acceptor checks offers arriving for one node and remembers their nonces
// for as long as they could be replayed.
type Acceptor struct {
	self  string
	now   func() time.Time
	mutex sync.Mutex
	seen  map[string]time.Time // nonce -> when it can be forgotten
}

// NewAcceptor returns an acceptor for node self.
func NewAcceptor(self string) *Acceptor {
	return &Acceptor{self: self, now: time.Now, seen: map[string]time.Time{}}
}

// Accept checks an offer that arrived from the authenticated member caller.
func (acceptor *Acceptor) Accept(caller string, offer Offer) error {
	if caller == "" || offer.From != caller {
		return ErrOfferNotFromPeer
	}
	if offer.To != acceptor.self {
		return ErrOfferForAnotherPair
	}
	now := acceptor.now()
	if !now.Before(offer.Expires) || offer.Expires.After(now.Add(OfferLifetime+clockSkew)) {
		return ErrOfferExpired
	}
	if err := checkCandidates(offer.Candidates); err != nil {
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

func checkCandidates(candidates []netip.AddrPort) error {
	if len(candidates) == 0 || len(candidates) > maxCandidates {
		return fmt.Errorf("%w: %d candidates", ErrOfferInvalid, len(candidates))
	}
	for _, candidate := range candidates {
		address := candidate.Addr()
		if !candidate.IsValid() || candidate.Port() == 0 || address.IsUnspecified() || address.IsMulticast() ||
			address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
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
	// Accepted is told of each accepted offer, so punching can begin.
	Accepted func(offer Offer)
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
	answer, err := NewOffer(server.Acceptor.self, caller, candidates, server.Acceptor.now())
	if err != nil {
		http.Error(response, "no path available", http.StatusServiceUnavailable)
		return
	}
	if server.Accepted != nil {
		server.Accepted(offer)
	}
	response.Header().Set("Content-Type", "application/json")
	json.NewEncoder(response).Encode(answer)
}

// Exchange sends offer to the peer at baseURL over client, which must be
// the mutually authenticated connection to that peer, and returns the
// peer's answer once it has passed the same checks as an incoming offer.
func Exchange(ctx context.Context, client *http.Client, baseURL string, groupID string, offer Offer, acceptor *Acceptor) (Offer, error) {
	body, err := json.Marshal(offer)
	if err != nil {
		return Offer{}, err
	}
	url := baseURL + "/jellymesh/v1/groups/" + groupID + "/path/offer"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Offer{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return Offer{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return Offer{}, fmt.Errorf("the peer refused the offer: %d %s", response.StatusCode, bytes.TrimSpace(message))
	}
	var answer Offer
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		return Offer{}, fmt.Errorf("malformed answer: %w", err)
	}
	if err := acceptor.Accept(offer.To, answer); err != nil {
		return Offer{}, err
	}
	return answer, nil
}
