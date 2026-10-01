package replication

import (
	"bytes"
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

	// ErrNotOwner means the peer is not the group's owner and cannot
	// sequence a proposal.
	ErrNotOwner = errors.New("the peer is not the group owner")

	// ErrProposalRefused means the owner applied the group's rules to the
	// proposal and refused it.
	ErrProposalRefused = errors.New("the owner refused the proposal")
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

// HTTPClient returns an HTTP client for peer over mutual TLS, pinned to its
// key, for callers that speak their own protocol to it, such as direct-path
// offers.
func (client *Client) HTTPClient(peer Peer) *http.Client {
	return &http.Client{Transport: client.transportFor(peer), Timeout: client.timeout}
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

// Submit sends a signed proposal to the group owner's node and returns the
// event it was sequenced as.
func (client *Client) Submit(ctx context.Context, owner Peer, groupID string, proposal grouplog.Proposal) (grouplog.Event, error) {
	body, err := json.Marshal(proposal)
	if err != nil {
		return grouplog.Event{}, err
	}
	var event grouplog.Event
	err = client.do(ctx, owner, http.MethodPost, "/jellymesh/v1/groups/"+url.PathEscape(groupID)+"/proposals", body, &event)
	return event, err
}

// Push hands a peer an event, which it applies as if it had pulled it.
func (client *Client) Push(ctx context.Context, peer Peer, groupID string, event grouplog.Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return client.do(ctx, peer, http.MethodPost, "/jellymesh/v1/groups/"+url.PathEscape(groupID)+"/log/events", body, nil)
}

// SendAttestation delivers this node's absence attestation to the group's
// eligible successor.
func (client *Client) SendAttestation(ctx context.Context, successor Peer, groupID string, attestation grouplog.Attestation) error {
	body, err := json.Marshal(attestation)
	if err != nil {
		return err
	}
	return client.do(ctx, successor, http.MethodPost, "/jellymesh/v1/groups/"+url.PathEscape(groupID)+"/attestations", body, nil)
}

// Stream makes a request to peer and returns the response for the caller to
// stream, without interpreting its status. Only a failure to reach the peer,
// or its refusal of this node's certificate, is an error. The caller closes
// the body; cancelling ctx stops the transfer.
func (client *Client) Stream(ctx context.Context, peer Peer, method string, path string, header http.Header) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "https://"+peer.Address+path, nil)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := (&http.Client{Transport: client.transportFor(peer)}).Do(request)
	if err != nil {
		return nil, classify(err)
	}
	return response, nil
}

// GetJSON fetches path from peer over the client's pinned mutual-TLS
// transport and decodes the response. Other member-only services, such as the
// catalog, use it so they share one transport and one error classification.
func (client *Client) GetJSON(ctx context.Context, peer Peer, path string, into any) error {
	return client.get(ctx, peer, path, into)
}

func (client *Client) get(ctx context.Context, peer Peer, path string, into any) error {
	return client.do(ctx, peer, http.MethodGet, path, nil, into)
}

func (client *Client) do(ctx context.Context, peer Peer, method string, path string, body []byte, into any) error {
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, "https://"+peer.Address+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Transport: client.transportFor(peer)}).Do(request)
	if err != nil {
		return classify(err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrNotServed
	case http.StatusConflict:
		return ErrNotOwner
	case http.StatusUnprocessableEntity:
		reason, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("%w: %s", ErrProposalRefused, strings.TrimSpace(string(reason)))
	default:
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
