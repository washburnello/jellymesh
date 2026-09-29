package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"jellymesh/internal/enrollment"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/replication"
)

// AdminTokenName is the file in the data directory holding the admin API's
// bearer token. It is owner-only: whoever can read it administers the node.
const AdminTokenName = "admin.token"

// LoadOrCreateAdminToken returns the admin token at path, creating it on first
// use. A token file readable by group or other is refused, as the node key is.
func LoadOrCreateAdminToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err == nil {
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s must not be readable by group or other", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.WriteString(token + "\n"); err != nil {
		return "", err
	}
	return token, nil
}

// AdminHandler serves the local admin API. It must only be served on a
// loopback address; the token is a second barrier, not the only one.
func (n *Node) AdminHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	api := http.NewServeMux()
	api.HandleFunc("GET /admin/v1/status", n.adminStatus)
	api.HandleFunc("POST /admin/v1/group", n.adminFound)
	api.HandleFunc("POST /admin/v1/invitations", n.adminInvite)
	api.HandleFunc("POST /admin/v1/join/redeem", n.adminRedeem)
	api.HandleFunc("POST /admin/v1/join/complete", n.adminComplete)
	api.HandleFunc("GET /admin/v1/requests", n.adminRequests)
	api.HandleFunc("POST /admin/v1/requests/approve", n.adminApprove)
	api.HandleFunc("POST /admin/v1/requests/deny", n.adminDeny)
	api.HandleFunc("POST /admin/v1/members/{member}/{action}", n.adminMember)
	api.HandleFunc("POST /admin/v1/leave", n.adminLeave)
	api.HandleFunc("PUT /admin/v1/blocks/{node}", n.adminBlock(true))
	api.HandleFunc("DELETE /admin/v1/blocks/{node}", n.adminBlock(false))
	api.HandleFunc("POST /admin/v1/sync", n.adminSync)
	api.HandleFunc("GET /admin/v1/libraries", n.adminLibraries)
	api.HandleFunc("POST /admin/v1/publications", n.adminPublish)
	api.HandleFunc("DELETE /admin/v1/publications/{library}", n.adminUnpublish)
	api.HandleFunc("GET /admin/v1/remote", n.adminRemote)
	api.HandleFunc("PUT /admin/v1/optouts/{source}/{library}", n.adminOptOut(true))
	api.HandleFunc("DELETE /admin/v1/optouts/{source}/{library}", n.adminOptOut(false))
	api.HandleFunc("POST /admin/v1/catalog/sync", n.adminCatalogSync)
	api.HandleFunc("GET /admin/v1/generated", n.adminGenerated)
	mux.Handle("/admin/", requireToken(token, api))
	return mux
}

func requireToken(token string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		presented := []byte(request.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare(presented, expected) != 1 {
			writeJSON(response, http.StatusUnauthorized, errorBody{"a valid admin token is required"})
			return
		}
		next.ServeHTTP(response, request)
	})
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func fail(response http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	switch {
	case errors.Is(err, ErrNoGroup), errors.Is(err, ErrUnknownPeer):
		status = http.StatusNotFound
	case errors.Is(err, ErrAlreadyGroup):
		status = http.StatusConflict
	case errors.Is(err, ErrNoServiceUser):
		status = http.StatusServiceUnavailable
	}
	writeJSON(response, status, errorBody{err.Error()})
}

func decode(response http.ResponseWriter, request *http.Request, into any) bool {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(response, http.StatusBadRequest, errorBody{"malformed request: " + err.Error()})
		return false
	}
	return true
}

// Status is what the admin API reports about the node.
type Status struct {
	NodeID        string           `json:"node_id"`
	Fingerprint   node.Fingerprint `json:"fingerprint"`
	PublicAddress string           `json:"public_address"`
	Group         *GroupStatus     `json:"group,omitempty"`
	Direct        DirectStatus     `json:"direct"`
}

type GroupStatus struct {
	ID       string         `json:"id"`
	Epoch    uint64         `json:"epoch"`
	Sequence uint64         `json:"sequence"`
	Head     string         `json:"head"`
	OwnerID  string         `json:"owner_id"`
	Held     bool           `json:"sequencing_held"`
	Queued   int            `json:"queued_proposals"`
	Members  []MemberStatus `json:"members"`
}

type MemberStatus struct {
	NodeID        string           `json:"node_id"`
	FriendlyName  string           `json:"friendly_name"`
	Address       string           `json:"address"`
	Fingerprint   node.Fingerprint `json:"fingerprint"`
	Administrator bool             `json:"administrator"`
	Blocked       bool             `json:"blocked"`
}

// Status reports the node's identity and, if it has one, its group.
func (n *Node) Status(ctx context.Context) (Status, error) {
	status := Status{NodeID: n.nodeID, Fingerprint: n.identity.Fingerprint(), PublicAddress: n.cfg.PublicAddress(), Direct: n.direct.status()}
	runtime, err := n.current()
	if err != nil {
		return status, nil
	}
	group := &GroupStatus{ID: runtime.id}
	var members []grouplog.Member
	_ = runtime.group.View(func(state *grouplog.State) {
		group.Epoch, group.Sequence, group.Head, group.OwnerID = state.Head.Epoch, state.Head.Sequence, state.Head.Hash.String(), state.OwnerID
		members = state.Members()
		for _, member := range members {
			group.Members = append(group.Members, MemberStatus{
				NodeID: member.NodeID, FriendlyName: member.FriendlyName, Address: member.PublicHostname,
				Fingerprint: member.Fingerprint, Administrator: state.IsAdministrator(member.NodeID),
			})
		}
	})
	for index := range group.Members {
		group.Members[index].Blocked, _ = n.peers.IsBlocked(ctx, group.Members[index].NodeID)
	}
	group.Held, _ = n.logs.SequencingHeld(ctx)
	if queued, err := n.queue.Pending(ctx, runtime.id); err == nil {
		group.Queued = len(queued)
	}
	status.Group = group
	return status, nil
}

func (n *Node) adminStatus(response http.ResponseWriter, request *http.Request) {
	status, err := n.Status(request.Context())
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

type FoundRequest struct {
	GroupID string `json:"group_id"`
}

func (n *Node) adminFound(response http.ResponseWriter, request *http.Request) {
	var body FoundRequest
	if !decode(response, request, &body) {
		return
	}
	if err := n.Found(request.Context(), body.GroupID); err != nil {
		fail(response, err)
		return
	}
	n.adminStatus(response, request)
}

type InviteRequest struct {
	ValidForSeconds int `json:"valid_for_seconds"`
}

type InviteResponse struct {
	ShortCode string `json:"short_code"`
	Address   string `json:"address"`
	QR        string `json:"qr"`
	ExpiresAt string `json:"expires_at"`
}

func (n *Node) adminInvite(response http.ResponseWriter, request *http.Request) {
	var body InviteRequest
	if !decode(response, request, &body) {
		return
	}
	validFor := time.Duration(body.ValidForSeconds) * time.Second
	if validFor <= 0 {
		validFor = 24 * time.Hour
	}
	token, err := n.Invite(request.Context(), validFor)
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, InviteResponse{
		ShortCode: token.ShortCode(), Address: token.Address, QR: token.QR(),
		ExpiresAt: n.now().Add(validFor).UTC().Format(time.RFC3339),
	})
}

// RedeemAdminRequest carries an invitation in either form, and the libraries
// this node will publish.
type RedeemAdminRequest struct {
	ShortCode string           `json:"short_code,omitempty"`
	Address   string           `json:"address,omitempty"`
	QR        string           `json:"qr,omitempty"`
	Libraries []policy.Library `json:"libraries"`
}

// PendingJoin is what a node must remember between redeeming and joining.
type PendingJoin struct {
	GroupID            string           `json:"group_id"`
	Genesis            grouplog.Hash    `json:"genesis"`
	InviterAddress     string           `json:"inviter_address"`
	InviterFingerprint node.Fingerprint `json:"inviter_fingerprint"`
	InvitationID       string           `json:"invitation_id"`
	Status             string           `json:"status"`
}

func (n *Node) adminRedeem(response http.ResponseWriter, request *http.Request) {
	var body RedeemAdminRequest
	if !decode(response, request, &body) {
		return
	}
	var token enrollment.Token
	var err error
	if body.QR != "" {
		token, err = enrollment.ParseQR(body.QR)
	} else {
		token, err = enrollment.ParseShortCode(body.Address, body.ShortCode)
	}
	if err != nil {
		fail(response, err)
		return
	}
	redemption, err := n.Redeem(request.Context(), token, body.Libraries)
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, PendingJoin{
		GroupID: redemption.GroupID, Genesis: redemption.Genesis, InviterAddress: token.Address,
		InviterFingerprint: redemption.InviterFingerprint, InvitationID: redemption.InvitationID, Status: redemption.Status,
	})
}

func (n *Node) adminComplete(response http.ResponseWriter, request *http.Request) {
	var body PendingJoin
	if !decode(response, request, &body) {
		return
	}
	status, err := n.CompleteJoin(request.Context(),
		replication.Peer{Address: body.InviterAddress, Fingerprint: body.InviterFingerprint}, body.GroupID, body.Genesis)
	if err != nil {
		fail(response, err)
		return
	}
	body.Status = status
	writeJSON(response, http.StatusOK, body)
}

func (n *Node) adminRequests(response http.ResponseWriter, request *http.Request) {
	requests, err := n.Requests(request.Context())
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, requests)
}

type DecisionRequest struct {
	InviterID    string `json:"inviter_id"`
	InvitationID string `json:"invitation_id"`
}

func (n *Node) adminApprove(response http.ResponseWriter, request *http.Request) {
	var body DecisionRequest
	if !decode(response, request, &body) {
		return
	}
	outcome, err := n.Approve(request.Context(), body.InviterID, body.InvitationID)
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, outcome)
}

func (n *Node) adminDeny(response http.ResponseWriter, request *http.Request) {
	var body DecisionRequest
	if !decode(response, request, &body) {
		return
	}
	if err := n.Deny(request.Context(), body.InviterID, body.InvitationID); err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "denied"})
}

func (n *Node) adminMember(response http.ResponseWriter, request *http.Request) {
	kinds := map[string]grouplog.Kind{"eject": grouplog.KindEjection, "promote": grouplog.KindPromote, "demote": grouplog.KindDemote}
	kind, ok := kinds[request.PathValue("action")]
	if !ok {
		writeJSON(response, http.StatusNotFound, errorBody{"unknown action"})
		return
	}
	outcome, err := n.Propose(request.Context(), kind, grouplog.MemberBody{MemberID: request.PathValue("member")})
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, outcome)
}

func (n *Node) adminLeave(response http.ResponseWriter, request *http.Request) {
	outcome, err := n.Propose(request.Context(), grouplog.KindLeave, grouplog.MemberBody{MemberID: n.nodeID})
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, outcome)
}

func (n *Node) adminBlock(blocked bool) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if err := n.SetBlocked(request.Context(), request.PathValue("node"), blocked); err != nil {
			fail(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]bool{"blocked": blocked})
	}
}

func (n *Node) adminSync(response http.ResponseWriter, request *http.Request) {
	result, err := n.SyncOnce(request.Context())
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (n *Node) adminLibraries(response http.ResponseWriter, request *http.Request) {
	libraries, err := n.Libraries(request.Context())
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, libraries)
}

// PublishRequest publishes a library with its declared root paths.
type PublishRequest struct {
	LibraryID string   `json:"library_id"`
	Roots     []string `json:"roots"`
}

func (n *Node) adminPublish(response http.ResponseWriter, request *http.Request) {
	var body PublishRequest
	if !decode(response, request, &body) {
		return
	}
	if err := n.Publish(request.Context(), body.LibraryID, body.Roots); err != nil {
		fail(response, err)
		return
	}
	n.adminLibraries(response, request)
}

func (n *Node) adminUnpublish(response http.ResponseWriter, request *http.Request) {
	if err := n.Unpublish(request.Context(), request.PathValue("library")); err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "unpublished"})
}

func (n *Node) adminRemote(response http.ResponseWriter, request *http.Request) {
	libraries, err := n.Remote(request.Context())
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, libraries)
}

func (n *Node) adminOptOut(optedOut bool) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if err := n.SetOptOut(request.Context(), request.PathValue("source"), request.PathValue("library"), optedOut); err != nil {
			fail(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]bool{"opted_out": optedOut})
	}
}

func (n *Node) adminCatalogSync(response http.ResponseWriter, request *http.Request) {
	result, err := n.SyncCatalogOnce(request.Context())
	if err != nil {
		fail(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (n *Node) adminGenerated(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, n.GeneratedRoots())
}
