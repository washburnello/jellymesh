package enrollment

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/replication"
	"jellymesh/internal/transport"
)

var (
	// ErrInvitationUnavailable covers a wrong, used, or expired secret. The
	// inviter deliberately does not say which.
	ErrInvitationUnavailable = errors.New("the invitation is not valid")
	ErrRateLimited           = errors.New("the inviter is refusing redemptions for now")
	ErrRefused               = errors.New("the inviter refused the request")
	ErrNotAdmitted           = errors.New("the joining node is not in the group log it downloaded")
)

const requestTimeout = 30 * time.Second

// Redemption is the result of redeeming an invitation: the group, as the
// inviter reported it over the pinned connection, and the inviter's full
// fingerprint, which a short code only carried a prefix of.
type Redemption struct {
	RedeemResponse
	InviterFingerprint node.Fingerprint
}

// Redeem presents the invitation's secret to the inviter as identity. The
// connection is accepted only if the inviter's key matches the invitation,
// and, for a QR invitation, only if the inviter reports the genesis hash the
// invitation carried.
func Redeem(ctx context.Context, identity *node.Identity, token Token, nodeID string, friendlyName string, publicHostname string, libraries []policy.Library) (Redemption, error) {
	body, err := json.Marshal(RedeemRequest{
		Secret: token.Secret, NodeID: nodeID, FriendlyName: friendlyName,
		PublicHostname: publicHostname, Libraries: libraries,
	})
	if err != nil {
		return Redemption{}, err
	}
	config := transport.ClientTLSConfigMatching(identity.TLSCertificate(), token.Matches)
	var response RedeemResponse
	state, err := call(ctx, config, http.MethodPost, token.Address, RedeemPath, body, &response)
	if err != nil {
		return Redemption{}, err
	}
	fingerprint, err := transport.PeerFingerprint(state)
	if err != nil {
		return Redemption{}, err
	}
	if !token.Genesis.IsZero() && (response.Genesis != token.Genesis || response.GroupID != token.GroupID) {
		return Redemption{}, ErrGenesisDiffer
	}
	return Redemption{RedeemResponse: response, InviterFingerprint: fingerprint}, nil
}

// Status asks the inviter where this node's request stands.
func Status(ctx context.Context, identity *node.Identity, inviter replication.Peer) (StatusResponse, error) {
	config := transport.ClientTLSConfig(identity.TLSCertificate(), inviter.Fingerprint, transport.NewMemoryTrustStore(inviter.Fingerprint))
	var response StatusResponse
	_, err := call(ctx, config, http.MethodGet, inviter.Address, StatusPath, nil, &response)
	return response, err
}

// Join downloads the group log from the inviter once this node is admitted,
// anchors it to genesis, stores it, and checks that the log really admits
// this node's own key.
func Join(ctx context.Context, identity *node.Identity, inviter replication.Peer, groupID string, genesis grouplog.Hash, logs membership.LogStore, blocks membership.BlockList) (*membership.Group, error) {
	client := replication.NewClient(identity, transport.NewMemoryTrustStore(inviter.Fingerprint))
	var events []grouplog.Event
	for {
		page, err := client.Events(ctx, inviter, groupID, uint64(len(events)), replication.MaxPageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		events = append(events, page...)
	}
	// Check before storing anything, so a log that does not admit this node
	// never replaces what this node holds.
	verified, err := grouplog.ReplayAnchored(events, genesis)
	if err != nil {
		return nil, err
	}
	if _, admitted := verified.State().MemberByFingerprint(identity.Fingerprint()); !admitted {
		return nil, ErrNotAdmitted
	}
	return membership.Join(ctx, logs, blocks, events, genesis)
}

// Requests fetches the redeemed invitations awaiting a decision from an
// inviter, as an owner or administrator.
func Requests(ctx context.Context, identity *node.Identity, trust transport.TrustStore, inviter replication.Peer, groupID string) ([]Request, error) {
	config := transport.ClientTLSConfig(identity.TLSCertificate(), inviter.Fingerprint, trust)
	var requests []Request
	_, err := call(ctx, config, http.MethodGet, inviter.Address, "/jellymesh/v1/groups/"+url.PathEscape(groupID)+"/requests", nil, &requests)
	return requests, err
}

// Deny refuses a redeemed invitation, as an owner or administrator.
func Deny(ctx context.Context, identity *node.Identity, trust transport.TrustStore, inviter replication.Peer, groupID string, invitationID string) error {
	config := transport.ClientTLSConfig(identity.TLSCertificate(), inviter.Fingerprint, trust)
	path := "/jellymesh/v1/groups/" + url.PathEscape(groupID) + "/requests/" + url.PathEscape(invitationID) + "/deny"
	_, err := call(ctx, config, http.MethodPost, inviter.Address, path, nil, nil)
	return err
}

// call makes one request and returns the TLS state it was made over.
func call(ctx context.Context, config *tls.Config, method string, address string, path string, body []byte, into any) (tls.ConnectionState, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, "https://"+address+path, reader)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config, TLSHandshakeTimeout: 10 * time.Second}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return *response.TLS, nil
	case http.StatusNotFound:
		return tls.ConnectionState{}, ErrInvitationUnavailable
	case http.StatusTooManyRequests:
		return tls.ConnectionState{}, ErrRateLimited
	default:
		reason, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return tls.ConnectionState{}, fmt.Errorf("%w (%s): %s", ErrRefused, response.Status, strings.TrimSpace(string(reason)))
	}
	if into != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(into); err != nil {
			return tls.ConnectionState{}, fmt.Errorf("decode response: %w", err)
		}
	}
	return *response.TLS, nil
}
