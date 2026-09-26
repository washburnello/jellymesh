package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

var (
	// ErrRefusedByPeer means the peer's TLS layer rejected this node's
	// certificate. Under TLS 1.3 the client's handshake reports success before
	// the server has decided (C-TR-5), so this surfaces on the first request,
	// and it is reported separately from an unreachable peer: one is a network
	// problem, the other means the peer does not consider this node a member.
	ErrRefusedByPeer = errors.New("the peer refused this node's certificate")

	// ErrPeerUnreachable means no TLS session could be used at all.
	ErrPeerUnreachable = errors.New("the peer could not be reached")

	// ErrNotServed means the peer answered but does not serve the group to
	// this node.
	ErrNotServed = errors.New("the peer does not serve this group to this node")
)

// maxResponseBytes bounds how much of a response is read, so a hostile peer
// cannot exhaust memory with an endless body.
const maxResponseBytes = 8 << 20

// Peer is a member to replicate from: where it listens, and the key it must
// present.
type Peer struct {
	Address     string
	Fingerprint node.Fingerprint
}

// Client fetches log heads and events from peers as identity.
type Client struct {
	identity   *node.Identity
	trust      transport.TrustStore
	timeout    time.Duration
	mutex      sync.Mutex
	transports map[node.Fingerprint]*http.Transport
}

// NewClient returns a client that authenticates as identity and accepts a
// peer only if trust accepts its key as well as it matching the expected
// fingerprint.
func NewClient(identity *node.Identity, trust transport.TrustStore) *Client {
	return &Client{
		identity:   identity,
		trust:      trust,
		timeout:    30 * time.Second,
		transports: make(map[node.Fingerprint]*http.Transport),
	}
}

func (client *Client) transportFor(peer Peer) *http.Transport {
	client.mutex.Lock()
	defer client.mutex.Unlock()
	if existing, ok := client.transports[peer.Fingerprint]; ok {
		return existing
	}
	created := &http.Transport{
		TLSClientConfig:     transport.ClientTLSConfig(client.identity.TLSCertificate(), peer.Fingerprint, client.trust),
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
	client.transports[peer.Fingerprint] = created
	return created
}

// Head returns the peer's log head for groupID.
func (client *Client) Head(ctx context.Context, peer Peer, groupID string) (grouplog.Head, error) {
	var head grouplog.Head
	err := client.get(ctx, peer, "/jellymesh/v1/groups/"+url.PathEscape(groupID)+"/log/head", &head)
	return head, err
}

// Events returns up to limit events after sequence from the peer.
func (client *Client) Events(ctx context.Context, peer Peer, groupID string, after uint64, limit int) ([]grouplog.Event, error) {
	query := url.Values{"after": {strconv.FormatUint(after, 10)}, "limit": {strconv.Itoa(limit)}}
	var events []grouplog.Event
	err := client.get(ctx, peer, "/jellymesh/v1/groups/"+url.PathEscape(groupID)+"/log/events?"+query.Encode(), &events)
	return events, err
}

func (client *Client) get(ctx context.Context, peer Peer, path string, into any) error {
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+peer.Address+path, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: client.transportFor(peer)}).Do(request)
	if err != nil {
		return classify(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotServed
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("peer answered %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(into); err != nil {
		return fmt.Errorf("decode peer response: %w", err)
	}
	return nil
}

// classify separates "the peer refused us" from "the peer is not there".
//
// crypto/tls reports an alert received from the peer as a *net.OpError whose
// Op is "remote error"; the alert itself is an unexported type, so its
// description is matched. Every certificate-related alert means the peer
// rejected this node's identity.
func classify(err error) error {
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "remote error" && operation.Err != nil &&
		strings.Contains(operation.Err.Error(), "certificate") {
		return fmt.Errorf("%w: %v", ErrRefusedByPeer, err)
	}
	return fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
}

// Result reports what a sync did.
type Result struct {
	Applied    int
	Superseded bool
}

// Sync brings group up to date with peer's log for groupID, fetching pages of
// pageSize events (MaxPageSize if zero).
//
// Ordinarily it fetches what follows this node's head. If the peer is on a
// later epoch, this node may hold events from the closed epoch that the
// succession will replace, and the point where the logs diverge is not known
// in advance; the log is then fetched from genesis, which is cheap for logs of
// this size and needs no search. Events already held come back as duplicates
// and change nothing.
func Sync(ctx context.Context, client *Client, peer Peer, group *membership.Group, groupID string, pageSize int) (Result, error) {
	if pageSize <= 0 || pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	theirs, err := client.Head(ctx, peer, groupID)
	if err != nil {
		return Result{}, err
	}
	ours := group.Head()

	var after uint64
	switch {
	case theirs == ours, theirs.Epoch < ours.Epoch:
		// Level with this node, or on an epoch this node has closed.
		return Result{}, nil
	case theirs.Epoch > ours.Epoch:
		after = 0
	case theirs.Sequence <= ours.Sequence:
		// Same epoch, and the peer is not ahead, yet its head differs from
		// this node's. Receiving the event at the peer's head is either a
		// duplicate, or two events for one slot, which is equivocation and
		// must be caught here rather than left unnoticed.
		return Result{}, checkHead(ctx, client, peer, group, groupID, theirs)
	default:
		after = ours.Sequence
	}

	var result Result
	for {
		events, err := client.Events(ctx, peer, groupID, after, pageSize)
		if err != nil {
			return result, err
		}
		if len(events) == 0 {
			return result, nil
		}
		for _, event := range events {
			if event.Sequence != after+1 {
				return result, fmt.Errorf("peer sent event %d after %d", event.Sequence, after)
			}
			outcome, err := group.Receive(ctx, event)
			if err != nil {
				return result, fmt.Errorf("event %d from peer: %w", event.Sequence, err)
			}
			switch outcome {
			case grouplog.Applied:
				result.Applied++
			case grouplog.Superseded:
				result.Applied++
				result.Superseded = true
			}
			after = event.Sequence
		}
		if after >= theirs.Sequence {
			return result, nil
		}
	}
}

func checkHead(ctx context.Context, client *Client, peer Peer, group *membership.Group, groupID string, theirs grouplog.Head) error {
	if theirs.Sequence == 0 {
		return nil
	}
	events, err := client.Events(ctx, peer, groupID, theirs.Sequence-1, 1)
	if err != nil {
		return err
	}
	if len(events) != 1 || events[0].Sequence != theirs.Sequence {
		return fmt.Errorf("peer did not serve the event at its own head %d", theirs.Sequence)
	}
	if _, err := group.Receive(ctx, events[0]); err != nil {
		return fmt.Errorf("event %d from peer: %w", theirs.Sequence, err)
	}
	return nil
}
