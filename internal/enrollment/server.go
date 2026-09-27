package enrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"jellymesh/internal/audit"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/transport"
)

const (
	RedeemPath   = "/jellymesh/v1/enroll"
	StatusPath   = "/jellymesh/v1/enroll/status"
	requestsPath = "/jellymesh/v1/groups/{group}/requests"
	denyPath     = "/jellymesh/v1/groups/{group}/requests/{invitation}/deny"

	maxRequestBytes = 64 << 10

	// Redemption failures are limited across all clients, because a guesser
	// can present a fresh key on every attempt. Ten failures in ten minutes
	// makes a 64-bit secret unguessable within any invitation's lifetime, at
	// the cost that someone flooding bad codes delays genuine redemptions,
	// which is an acceptable failure for an endpoint used a few times a year.
	failureLimit  = 10
	failureWindow = 10 * time.Minute
)

// PolicyStore persists the inviter's policy state; store.PolicyRepository
// satisfies it.
type PolicyStore interface {
	Save(ctx context.Context, state *policy.State) error
	Load(ctx context.Context, groupID string) (*policy.State, error)
}

// Inviter is the enrollment service of a node that issues invitations for one
// group.
type Inviter struct {
	mutex    sync.Mutex
	nodeID   string
	identity *node.Identity
	address  string
	group    *membership.Group
	groupID  string
	policy   *policy.State
	store    PolicyStore
	failures []time.Time
	now      func() time.Time
	audit    *audit.Log
}

// SetAudit records invitation decisions to log.
func (inviter *Inviter) SetAudit(log *audit.Log) { inviter.audit = log }

// NewInviter serves enrollment for group as nodeID, advertising address as
// where invitees should connect.
func NewInviter(nodeID string, identity *node.Identity, address string, group *membership.Group, groupID string, state *policy.State, store PolicyStore) *Inviter {
	return &Inviter{
		nodeID: nodeID, identity: identity, address: address,
		group: group, groupID: groupID, policy: state, store: store,
		now: time.Now,
	}
}

// Invite creates an invitation from this node, valid until expiresAt.
func (inviter *Inviter) Invite(ctx context.Context, expiresAt time.Time) (Token, error) {
	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()

	var genesis grouplog.Hash
	if events := inviter.group.EventsAfter(0); len(events) > 0 {
		genesis = events[0].Hash()
	}
	token, codeHash, err := NewToken(inviter.address, inviter.identity.Fingerprint(), inviter.groupID, genesis)
	if err != nil {
		return Token{}, err
	}
	// The invitation ID travels in the admission event to every member, so
	// it is random rather than derived from the secret.
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return Token{}, err
	}
	invitationID := hex.EncodeToString(idBytes)
	err = inviter.withRoster(func(roster policy.Roster) error {
		return inviter.policy.CreateInvitation(roster, inviter.nodeID, invitationID, codeHash, expiresAt)
	})
	if err != nil {
		return Token{}, err
	}
	if err := inviter.save(ctx); err != nil {
		return Token{}, err
	}
	_ = inviter.audit.Record(ctx, inviter.nodeID, "invitation.created", invitationID, map[string]string{
		"group_id": inviter.groupID, "invitation_id": invitationID,
	})
	return token, nil
}

// withRoster runs fn with the current roster under the group's read lock.
func (inviter *Inviter) withRoster(fn func(policy.Roster) error) error {
	var result error
	if err := inviter.group.View(func(state *grouplog.State) { result = fn(state) }); err != nil {
		return err
	}
	return result
}

// save persists the policy state, restoring the stored copy if that fails so
// that memory never holds a redemption that disk does not.
func (inviter *Inviter) save(ctx context.Context) error {
	if err := inviter.store.Save(ctx, inviter.policy); err != nil {
		if restored, loadErr := inviter.store.Load(ctx, inviter.groupID); loadErr == nil {
			inviter.policy = restored
		}
		return fmt.Errorf("store invitation state: %w", err)
	}
	return nil
}

// Register adds the enrollment routes. Redemption and status are public,
// since the caller is not yet a member; the request routes are for members,
// and additionally check that the caller administers the group.
func (inviter *Inviter) Register(public *http.ServeMux, members *http.ServeMux) {
	public.HandleFunc("POST "+RedeemPath, inviter.redeem)
	public.HandleFunc("GET "+StatusPath, inviter.status)
	members.HandleFunc("GET "+requestsPath, inviter.requests)
	members.HandleFunc("POST "+denyPath, inviter.deny)
}

// RedeemRequest is what an invitee sends. Its key is not in it: the key is
// the one the invitee's TLS certificate proved possession of.
type RedeemRequest struct {
	Secret         []byte           `json:"secret"`
	NodeID         string           `json:"node_id"`
	FriendlyName   string           `json:"friendly_name"`
	PublicHostname string           `json:"public_hostname"`
	Libraries      []policy.Library `json:"libraries"`
}

// RedeemResponse tells the invitee which group it is joining. It arrives over
// a connection pinned to the inviter's key, which is what makes the genesis
// hash in it trustworthy when the invitee started from a short code.
type RedeemResponse struct {
	GroupID      string        `json:"group_id"`
	Genesis      grouplog.Hash `json:"genesis"`
	InvitationID string        `json:"invitation_id"`
	Status       string        `json:"status"`
}

func (inviter *Inviter) redeem(response http.ResponseWriter, request *http.Request) {
	key, ok := callerKey(response, request)
	if !ok {
		return
	}
	var body RedeemRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		http.Error(response, "malformed request", http.StatusBadRequest)
		return
	}

	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()
	if !inviter.allowAttempt() {
		_ = inviter.audit.Record(request.Context(), "", "invitation.redeem_rate_limited", inviter.groupID, map[string]string{"group_id": inviter.groupID})
		http.Error(response, "too many failed redemptions; try again later", http.StatusTooManyRequests)
		return
	}

	var invitation policy.Invitation
	err := inviter.withRoster(func(roster policy.Roster) error {
		var err error
		invitation, err = inviter.policy.RedeemInvitation(roster, CodeHash(body.Secret), policy.Invitee{
			NodeID: body.NodeID, PublicKey: key, FriendlyName: body.FriendlyName,
			PublicHostname: body.PublicHostname, Libraries: body.Libraries,
		})
		return err
	})
	switch {
	case errors.Is(err, policy.ErrInvitationNotFound), errors.Is(err, policy.ErrInvitationAlreadyUsed), errors.Is(err, policy.ErrInvitationExpired):
		// The same answer for a wrong, used, or expired secret, so that a
		// guesser learns nothing about which invitations exist.
		inviter.recordFailure()
		callerFingerprint, _ := node.FingerprintOfPublicKey(key)
		_ = inviter.audit.Record(request.Context(), string(callerFingerprint), "invitation.redeem_failed", inviter.groupID, map[string]string{
			"group_id": inviter.groupID, "fingerprint": string(callerFingerprint), "reason": err.Error(),
		})
		if errors.Is(err, policy.ErrInvitationExpired) {
			_ = inviter.save(request.Context())
		}
		http.NotFound(response, request)
		return
	case errors.Is(err, policy.ErrInsufficientPublications):
		http.Error(response, "an invitation must be redeemed with at least one library to publish", http.StatusUnprocessableEntity)
		return
	case err != nil:
		http.Error(response, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := inviter.save(request.Context()); err != nil {
		http.Error(response, "could not record the redemption", http.StatusInternalServerError)
		return
	}
	_ = inviter.audit.Record(request.Context(), invitation.InviteeID, "invitation.redeemed", invitation.InvitationID, map[string]string{
		"group_id": inviter.groupID, "invitation_id": invitation.InvitationID,
		"node_id": invitation.InviteeID, "fingerprint": invitation.Fingerprint,
	})
	writeJSON(response, RedeemResponse{
		GroupID: inviter.groupID, Genesis: inviter.genesis(),
		InvitationID: invitation.InvitationID, Status: string(invitation.Status),
	})
}

func (inviter *Inviter) genesis() grouplog.Hash {
	if events := inviter.group.EventsAfter(0); len(events) > 0 {
		return events[0].Hash()
	}
	return grouplog.Hash{}
}

func (inviter *Inviter) allowAttempt() bool {
	cutoff := inviter.now().Add(-failureWindow)
	kept := inviter.failures[:0]
	for _, failure := range inviter.failures {
		if failure.After(cutoff) {
			kept = append(kept, failure)
		}
	}
	inviter.failures = kept
	return len(inviter.failures) < failureLimit
}

func (inviter *Inviter) recordFailure() {
	inviter.failures = append(inviter.failures, inviter.now())
}

// StatusResponse tells an invitee where its request stands.
type StatusResponse struct {
	Status  string        `json:"status"`
	GroupID string        `json:"group_id"`
	Genesis grouplog.Hash `json:"genesis"`
}

func (inviter *Inviter) status(response http.ResponseWriter, request *http.Request) {
	key, ok := callerKey(response, request)
	if !ok {
		return
	}
	fingerprint, err := node.FingerprintOfPublicKey(key)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()

	status := ""
	_ = inviter.group.View(func(state *grouplog.State) {
		if _, member := state.MemberByFingerprint(fingerprint); member {
			status = string(policy.InvitationAdmitted)
		}
	})
	if status == "" {
		if invitation, found := inviter.policy.InvitationByFingerprint(string(fingerprint)); found {
			status = string(invitation.Status)
		}
	}
	if status == "" {
		http.NotFound(response, request)
		return
	}
	writeJSON(response, StatusResponse{Status: status, GroupID: inviter.groupID, Genesis: inviter.genesis()})
}

// Request is a redeemed invitation awaiting a decision, as an owner or
// administrator sees it before approving.
type Request struct {
	InvitationID   string           `json:"invitation_id"`
	InviterID      string           `json:"inviter_id"`
	NodeID         string           `json:"node_id"`
	PublicKey      []byte           `json:"public_key"`
	Fingerprint    node.Fingerprint `json:"fingerprint"`
	FriendlyName   string           `json:"friendly_name"`
	PublicHostname string           `json:"public_hostname"`
	Libraries      []policy.Library `json:"libraries"`
}

// administrator returns the node ID of the caller if it is an owner or
// administrator of the group this inviter serves.
func (inviter *Inviter) administrator(response http.ResponseWriter, request *http.Request) (string, bool) {
	key, ok := callerKey(response, request)
	if !ok {
		return "", false
	}
	fingerprint, _ := node.FingerprintOfPublicKey(key)
	var callerID string
	_ = inviter.group.View(func(state *grouplog.State) {
		if member, ok := state.MemberByFingerprint(fingerprint); ok && state.IsAdministrator(member.NodeID) {
			callerID = member.NodeID
		}
	})
	if callerID == "" || request.PathValue("group") != inviter.groupID {
		http.NotFound(response, request)
		return "", false
	}
	return callerID, true
}

func (inviter *Inviter) requests(response http.ResponseWriter, request *http.Request) {
	if _, ok := inviter.administrator(response, request); !ok {
		return
	}
	writeJSON(response, inviter.PendingRequests())
}

// PendingRequests returns the redeemed invitations awaiting a decision,
// excluding any whose invitee the log has already admitted.
func (inviter *Inviter) PendingRequests() []Request {
	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()

	admitted := map[string]bool{}
	_ = inviter.group.View(func(state *grouplog.State) {
		for _, invitation := range inviter.policy.PendingApprovals() {
			admitted[invitation.InvitationID] = state.IsMember(invitation.InviteeID)
		}
	})
	pending := []Request{}
	for _, invitation := range inviter.policy.PendingApprovals() {
		if admitted[invitation.InvitationID] {
			continue
		}
		var libraries []policy.Library
		for _, candidate := range inviter.policy.Candidates {
			if candidate.SourceNodeID == invitation.InviteeID {
				libraries = append(libraries, candidate.Library)
			}
		}
		pending = append(pending, Request{
			InvitationID: invitation.InvitationID, InviterID: invitation.InviterID,
			NodeID: invitation.InviteeID, PublicKey: invitation.PublicKey,
			Fingerprint: node.Fingerprint(invitation.Fingerprint), FriendlyName: invitation.FriendlyName,
			PublicHostname: invitation.PublicHostname, Libraries: libraries,
		})
	}
	return pending
}

// DenyLocally denies a request as administratorID on this node itself.
func (inviter *Inviter) DenyLocally(ctx context.Context, administratorID string, invitationID string) error {
	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()
	err := inviter.withRoster(func(roster policy.Roster) error {
		return inviter.policy.DenyInvitation(roster, administratorID, invitationID)
	})
	if err != nil {
		return err
	}
	if err := inviter.save(ctx); err != nil {
		return err
	}
	_ = inviter.audit.Record(ctx, administratorID, "invitation.denied", invitationID, map[string]string{
		"group_id": inviter.groupID, "invitation_id": invitationID,
	})
	return nil
}

func (inviter *Inviter) deny(response http.ResponseWriter, request *http.Request) {
	administratorID, ok := inviter.administrator(response, request)
	if !ok {
		return
	}
	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()
	err := inviter.withRoster(func(roster policy.Roster) error {
		return inviter.policy.DenyInvitation(roster, administratorID, request.PathValue("invitation"))
	})
	if err != nil {
		http.Error(response, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := inviter.save(request.Context()); err != nil {
		http.Error(response, "could not record the denial", http.StatusInternalServerError)
		return
	}
	_ = inviter.audit.Record(request.Context(), administratorID, "invitation.denied", request.PathValue("invitation"), map[string]string{
		"group_id": inviter.groupID, "invitation_id": request.PathValue("invitation"),
	})
	response.WriteHeader(http.StatusNoContent)
}

// WithPolicy runs change against the node's policy state under the inviter's
// lock, with the current roster, and saves the result. The inviter owns the
// policy state; anything else that changes it, such as opt-outs or the
// publications learned from peers, goes through here so there is one writer.
func (inviter *Inviter) WithPolicy(ctx context.Context, change func(state *policy.State, roster policy.Roster) error) error {
	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()
	err := inviter.withRoster(func(roster policy.Roster) error {
		return change(inviter.policy, roster)
	})
	if err != nil {
		return err
	}
	return inviter.save(ctx)
}

// Reconcile updates invitation state after the group log changes, marking
// invitations whose invitee has been admitted.
func (inviter *Inviter) Reconcile(ctx context.Context) error {
	inviter.mutex.Lock()
	defer inviter.mutex.Unlock()
	_ = inviter.withRoster(func(roster policy.Roster) error {
		inviter.policy.Reconcile(roster)
		return nil
	})
	return inviter.save(ctx)
}

// AdmissionFromRequest builds the admission an approver signs from a request
// it fetched from the inviter. It checks what the approver can check for
// itself: that the key matches the fingerprint the request names, and that
// the node offers at least one library.
func AdmissionFromRequest(request Request) (grouplog.AdmissionBody, error) {
	fingerprint, err := node.FingerprintOfPublicKey(request.PublicKey)
	if err != nil || fingerprint != request.Fingerprint {
		return grouplog.AdmissionBody{}, errors.New("the request's key does not match its fingerprint")
	}
	if len(request.Libraries) < policy.MinimumPublishedLibraries {
		return grouplog.AdmissionBody{}, policy.ErrInsufficientPublications
	}
	return grouplog.AdmissionBody{
		MemberID: request.NodeID, MemberKey: request.PublicKey,
		FriendlyName: request.FriendlyName, PublicHostname: request.PublicHostname,
		InvitationID: request.InvitationID, InviterID: request.InviterID,
	}, nil
}

func callerKey(response http.ResponseWriter, request *http.Request) (ed25519.PublicKey, bool) {
	if request.TLS == nil {
		http.NotFound(response, request)
		return nil, false
	}
	key, err := transport.PeerPublicKey(*request.TLS)
	if err != nil {
		http.NotFound(response, request)
		return nil, false
	}
	return key, true
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}
