package natpath

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

var (
	cedarOutside  = netip.MustParseAddrPort("198.98.94.5:41641")
	walnutOutside = netip.MustParseAddrPort("203.0.113.20:40000")
	walnutLAN     = netip.MustParseAddrPort("192.168.87.20:40000")
)

func fixedClock(acceptor *Acceptor, at time.Time) { acceptor.now = func() time.Time { return at } }

// C-NT-3: an offer is accepted only from the connection's authenticated
// caller, only when addressed to this node, only while fresh, and only once.
func TestOffersAreBoundToThePairAndUsedOnce(t *testing.T) {
	now := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	cedar := NewAcceptor("cedar")
	fixedClock(cedar, now)
	offer, err := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside, walnutLAN}, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := cedar.Accept("juniper", offer); !errors.Is(err, ErrOfferNotFromPeer) {
		t.Fatalf("an offer relayed by another member must be refused: %v", err)
	}
	forOther := offer
	forOther.To = "juniper"
	if err := cedar.Accept("walnut", forOther); !errors.Is(err, ErrOfferForAnotherPair) {
		t.Fatalf("an offer for another pair must be refused: %v", err)
	}
	if err := cedar.Accept("walnut", offer); err != nil {
		t.Fatalf("a fresh offer from its sender was refused: %v", err)
	}
	if err := cedar.Accept("walnut", offer); !errors.Is(err, ErrOfferReplayed) {
		t.Fatalf("a replayed offer must be refused: %v", err)
	}

	late, _ := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside}, now)
	fixedClock(cedar, now.Add(OfferLifetime+clockSkew+time.Second))
	if err := cedar.Accept("walnut", late); !errors.Is(err, ErrOfferExpired) {
		t.Fatalf("an expired offer must be refused: %v", err)
	}
	fixedClock(cedar, now)
	forever, _ := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside}, now)
	forever.Expires = now.Add(time.Hour)
	if err := cedar.Accept("walnut", forever); !errors.Is(err, ErrOfferExpired) {
		t.Fatalf("an offer claiming a long life must be refused: %v", err)
	}
}

// C-NT-3: punching sends UDP to an offer's candidates, so an offer may name
// only a few ordinary unicast addresses.
func TestOffersNameOnlyAFewUsableAddresses(t *testing.T) {
	now := time.Now()
	cedar := NewAcceptor("cedar")
	bad := [][]netip.AddrPort{
		nil,
		{netip.MustParseAddrPort("0.0.0.0:1")},
		{netip.MustParseAddrPort("224.0.0.1:5353")},
		{netip.MustParseAddrPort("127.0.0.1:8443")},
		{netip.MustParseAddrPort("169.254.1.1:9")},
		{netip.MustParseAddrPort("203.0.113.20:0")},
	}
	many := make([]netip.AddrPort, maxCandidates+1)
	for i := range many {
		many[i] = netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}), 40000)
	}
	bad = append(bad, many)
	for _, candidates := range bad {
		offer, _ := NewOffer("walnut", "cedar", candidates, now)
		if err := cedar.Accept("walnut", offer); !errors.Is(err, ErrOfferInvalid) {
			t.Errorf("candidates %v should be refused, got %v", candidates, err)
		}
	}
}

// C-NT-3: the exchange runs over the route; the route answers only members
// of its group, and each side checks the other's offer by the same rules.
func TestOffersAreExchangedOverTheMemberRoute(t *testing.T) {
	cedar := NewAcceptor("cedar")
	var accepted []Offer
	server := &Server{
		GroupID:  "group-1",
		Acceptor: cedar,
		Caller: func(request *http.Request) (string, bool) {
			member := request.Header.Get("X-Test-Member") // stands in for the TLS identity
			return member, member == "walnut" || member == "juniper"
		},
		Answer:   func(context.Context, string) ([]netip.AddrPort, error) { return []netip.AddrPort{cedarOutside}, nil },
		Accepted: func(offer Offer, _ time.Time) { accepted = append(accepted, offer) },
	}
	members := http.NewServeMux()
	server.Register(nil, members)
	ts := httptest.NewServer(members)
	defer ts.Close()
	as := func(member string) *http.Client {
		return &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
			request.Header.Set("X-Test-Member", member)
			return http.DefaultTransport.RoundTrip(request)
		})}
	}
	ctx := context.Background()

	walnut := NewAcceptor("walnut")
	offer, _ := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside}, time.Now())
	answer, _, err := Exchange(ctx, as("walnut"), ts.URL, "group-1", offer, walnut)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if answer.From != "cedar" || answer.To != "walnut" || len(answer.Candidates) != 1 || answer.Candidates[0] != cedarOutside {
		t.Fatalf("answer %+v", answer)
	}
	if len(accepted) != 1 || accepted[0].Nonce != offer.Nonce {
		t.Fatal("the accepted offer should be handed on for punching")
	}

	if _, _, err := Exchange(ctx, as("walnut"), ts.URL, "group-1", offer, walnut); err == nil {
		t.Fatal("a replayed offer must be refused over the route too")
	}
	stranger, _ := NewOffer("stranger", "cedar", []netip.AddrPort{walnutOutside}, time.Now())
	if _, _, err := Exchange(ctx, as("stranger"), ts.URL, "group-1", stranger, NewAcceptor("stranger")); err == nil {
		t.Fatal("a non-member must get nothing")
	}
	other, _ := NewOffer("juniper", "cedar", []netip.AddrPort{walnutOutside}, time.Now())
	if _, _, err := Exchange(ctx, as("juniper"), ts.URL, "group-2", other, NewAcceptor("juniper")); err == nil {
		t.Fatal("another group's route must answer nothing")
	}
	spoofed, _ := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside}, time.Now())
	if _, _, err := Exchange(ctx, as("juniper"), ts.URL, "group-1", spoofed, NewAcceptor("juniper")); err == nil {
		t.Fatal("a member must not offer on another member's behalf")
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// C-NT-3: the caller holds the answer to the same rules, so a peer cannot
// answer as someone else, for someone else, or with a stale offer.
func TestAnswersAreCheckedLikeOffers(t *testing.T) {
	now := time.Now()
	answers := map[string]Offer{}
	answers["impostor"], _ = NewOffer("juniper", "walnut", []netip.AddrPort{cedarOutside}, now)
	answers["misaddressed"], _ = NewOffer("cedar", "juniper", []netip.AddrPort{cedarOutside}, now)
	stale, _ := NewOffer("cedar", "walnut", []netip.AddrPort{cedarOutside}, now.Add(-OfferLifetime-clockSkew-time.Second))
	answers["stale"] = stale
	for name, answer := range answers {
		ts := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			json.NewEncoder(response).Encode(answer)
		}))
		offer, _ := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside}, now)
		if _, _, err := Exchange(context.Background(), ts.Client(), ts.URL, "group-1", offer, NewAcceptor("walnut")); err == nil {
			t.Errorf("a %s answer was accepted", name)
		}
		ts.Close()
	}
}

// C-NT-4: the punch starts at one instant on both sides even when the
// nodes' clocks disagree, because the caller converts the answer's start
// time by the clock difference it measures over the exchange.
func TestThePunchStartsTogetherDespiteClockDifference(t *testing.T) {
	for _, skew := range []time.Duration{0, 7 * time.Second, -40 * time.Second} {
		cedar := NewAcceptor("cedar")
		cedar.now = func() time.Time { return time.Now().Add(skew) } // cedar's clock is off
		var cedarStart time.Time
		server := &Server{
			GroupID: "group-1", Acceptor: cedar,
			Caller:   func(*http.Request) (string, bool) { return "walnut", true },
			Answer:   func(context.Context, string) ([]netip.AddrPort, error) { return []netip.AddrPort{cedarOutside}, nil },
			Accepted: func(_ Offer, start time.Time) { cedarStart = start },
		}
		members := http.NewServeMux()
		server.Register(nil, members)
		ts := httptest.NewServer(members)
		offer, _ := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside}, time.Now())
		_, walnutStart, err := Exchange(context.Background(), ts.Client(), ts.URL, "group-1", offer, NewAcceptor("walnut"))
		ts.Close()
		if err != nil {
			t.Fatalf("skew %v: %v", skew, err)
		}
		// cedarStart is on cedar's clock; bring it to real time to compare.
		gap := walnutStart.Sub(cedarStart.Add(-skew))
		if gap < -20*time.Millisecond || gap > 20*time.Millisecond {
			t.Fatalf("skew %v: the two starts differ by %v", skew, gap)
		}
		if until := time.Until(walnutStart); until < StartLead/2 || until > StartLead {
			t.Fatalf("skew %v: the start is %v away, want about %v", skew, until, StartLead)
		}
	}
}

// #55: a member may not set off punching at will: a second offer within
// MinOfferInterval is refused, and one after it is accepted.
func TestAMemberMayNotOfferTooOften(t *testing.T) {
	now := time.Now()
	cedar := NewAcceptor("cedar")
	fixedClock(cedar, now)
	var refused []error
	server := &Server{
		GroupID: "group-1", Acceptor: cedar,
		Caller:  func(*http.Request) (string, bool) { return "walnut", true },
		Answer:  func(context.Context, string) ([]netip.AddrPort, error) { return []netip.AddrPort{cedarOutside}, nil },
		Refused: func(_ string, err error) { refused = append(refused, err) },
	}
	members := http.NewServeMux()
	server.Register(nil, members)
	ts := httptest.NewServer(members)
	defer ts.Close()
	offer := func() error {
		o, _ := NewOffer("walnut", "cedar", []netip.AddrPort{walnutOutside}, now)
		_, _, err := Exchange(context.Background(), ts.Client(), ts.URL, "group-1", o, NewAcceptor("walnut"))
		return err
	}
	// The answer's clock is fixed; the caller's check tolerates that.
	if err := offer(); err != nil {
		t.Fatalf("a first offer: %v", err)
	}
	if err := offer(); err == nil || len(refused) != 1 || !errors.Is(refused[0], ErrOfferTooSoon) {
		t.Fatalf("a second offer within the interval must be refused: %v, %v", err, refused)
	}
	fixedClock(cedar, now.Add(MinOfferInterval))
	if err := offer(); err != nil {
		t.Fatalf("an offer after the interval: %v", err)
	}
}
