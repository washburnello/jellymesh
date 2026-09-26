// Package policy models a node's own media-sharing decisions within a group:
// the libraries it publishes, the publications it has learned from peers, the
// libraries it opts out of, the peers it blocks, and the invitations it has
// issued.
//
// All of this is node-local (design-spec section 8, "What is replicated and
// what is not"). Membership is not: who is in the group, who administers it,
// and who has been ejected come from the replicated group log. This package
// consults that through the Roster interface and never keeps a copy of it,
// because a second copy is exactly what drifted from the first before.
package policy

import (
	"errors"
	"strings"
	"time"
)

const MinimumPublishedLibraries = 1

var (
	ErrGroupIDRequired          = errors.New("group ID is required")
	ErrNodeIDRequired           = errors.New("node ID is required")
	ErrLibraryIDRequired        = errors.New("library ID is required")
	ErrPublicationRequired      = errors.New("library must be published before it can be opted out")
	ErrInsufficientPublications = errors.New("member must publish at least one library")
	ErrNotMember                = errors.New("node is not an active group member")
	ErrNotAdministrator         = errors.New("only an owner or administrator can perform this action")
	ErrInvalidInvitation        = errors.New("invitation is invalid")
	ErrInvitationNotFound       = errors.New("invitation was not found")
	ErrInvitationExpired        = errors.New("invitation has expired")
	ErrInvitationAlreadyUsed    = errors.New("invitation has already been used")
	ErrInvitationNotPending     = errors.New("invitation is not awaiting an approval decision")
	ErrAlreadyMember            = errors.New("node is already an active group member")
)

// Roster is the replicated membership this package consults.
// grouplog.State satisfies it.
type Roster interface {
	IsMember(nodeID string) bool
	IsAdministrator(nodeID string) bool
	IsEjected(nodeID string) bool
}

type Library struct {
	ID             string
	Name           string
	CollectionType string
}

type Publication struct {
	GroupID      string
	SourceNodeID string
	Library      Library
	PublishedAt  time.Time
}

type OptOut struct {
	GroupID      string
	SourceNodeID string
	LibraryID    string
	UpdatedAt    time.Time
}

type InvitationStatus string

const (
	InvitationCreated          InvitationStatus = "created"
	InvitationAwaitingApproval InvitationStatus = "awaiting_approval"
	InvitationApproved         InvitationStatus = "approved"
	InvitationDenied           InvitationStatus = "denied"
	InvitationExpired          InvitationStatus = "expired"
)

// Invitation is held only by the node that issued it. Other members learn of
// the invitee through the admission event, never through this record.
type Invitation struct {
	GroupID      string
	InvitationID string
	InviterID    string
	InviteeID    string
	CodeHash     string
	Fingerprint  string
	ApprovalID   string
	Status       InvitationStatus
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type State struct {
	// Now supplies the current time. It is injectable so that invitation
	// expiry can be exercised in tests without waiting. A nil Now means
	// time.Now.
	Now func() time.Time

	GroupID string

	// Published holds libraries offered to the group by members, as this
	// node knows them: its own, and those peers have served it.
	Published map[string]Publication

	// Candidates holds libraries staged by a node that is not yet admitted.
	// The admission rule requires at least one non-empty publication before
	// approval, but a node cannot publish into the live pool until it is a
	// member. Reconcile promotes a candidate's entries once the log admits it.
	Candidates map[string]Publication

	OptOuts      map[string]map[string]OptOut
	BlockedPeers map[string]bool
	Invitations  map[string]Invitation
}

func NewState(groupID string) (*State, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, ErrGroupIDRequired
	}
	return &State{
		GroupID:      groupID,
		Published:    make(map[string]Publication),
		Candidates:   make(map[string]Publication),
		OptOuts:      make(map[string]map[string]OptOut),
		BlockedPeers: make(map[string]bool),
		Invitations:  make(map[string]Invitation),
	}, nil
}

// now returns the state's current time, defaulting to the wall clock.
func (state *State) now() time.Time {
	if state == nil || state.Now == nil {
		return time.Now().UTC()
	}
	return state.Now().UTC()
}

// ---------------------------------------------------------------------------
// Reconciling with the roster.
// ---------------------------------------------------------------------------

// Reconcile brings this node's publication view into line with the roster
// after the group log changes. A node that is no longer a member has its
// publications removed (C-PO-5); a staged candidate that the log has admitted
// is promoted into the live pool; an ejected node's staged candidates are
// discarded. It is idempotent, so it can simply run after every change.
func (state *State) Reconcile(roster Roster) {
	if state == nil || roster == nil {
		return
	}
	for key, publication := range state.Published {
		if !roster.IsMember(publication.SourceNodeID) {
			delete(state.Published, key)
		}
	}
	for key, publication := range state.Candidates {
		switch {
		case roster.IsMember(publication.SourceNodeID):
			state.Published[key] = publication
			delete(state.Candidates, key)
		case roster.IsEjected(publication.SourceNodeID):
			delete(state.Candidates, key)
		}
	}
}

// ---------------------------------------------------------------------------
// Invitations. These are local to the inviting node; admission itself is a
// signed proposal sequenced into the group log.
// ---------------------------------------------------------------------------

func (state *State) CreateInvitation(roster Roster, inviterID string, invitationID string, codeHash string, expiresAt time.Time) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := validateNode(inviterID); err != nil {
		return err
	}
	if roster == nil || !roster.IsMember(inviterID) {
		return ErrNotMember
	}
	invitationID = strings.TrimSpace(invitationID)
	codeHash = strings.TrimSpace(codeHash)
	if invitationID == "" || codeHash == "" || expiresAt.IsZero() {
		return ErrInvalidInvitation
	}
	if _, ok := state.Invitations[invitationID]; ok {
		return ErrInvitationAlreadyUsed
	}
	now := state.now()
	if !expiresAt.After(now) {
		return ErrInvitationExpired
	}
	state.Invitations[invitationID] = Invitation{
		GroupID:      state.GroupID,
		InvitationID: invitationID,
		InviterID:    strings.TrimSpace(inviterID),
		CodeHash:     codeHash,
		Status:       InvitationCreated,
		CreatedAt:    now,
		ExpiresAt:    expiresAt.UTC(),
	}
	return nil
}

func (state *State) InvitationByID(invitationID string) (Invitation, bool) {
	if state == nil {
		return Invitation{}, false
	}
	invitation, ok := state.Invitations[strings.TrimSpace(invitationID)]
	return invitation, ok
}

// RedeemInvitation binds an invitation to the invitee's identity fingerprint.
// Expiration applies until redemption; once redeemed the request stays pending
// until an owner or administrator decides it.
func (state *State) RedeemInvitation(roster Roster, invitationID string, inviteeID string, fingerprint string) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := validateNode(inviteeID); err != nil {
		return err
	}
	invitation, ok := state.Invitations[strings.TrimSpace(invitationID)]
	if !ok {
		return ErrInvitationNotFound
	}
	if state.now().After(invitation.ExpiresAt) {
		invitation.Status = InvitationExpired
		state.Invitations[invitation.InvitationID] = invitation
		return ErrInvitationExpired
	}
	if invitation.Status != InvitationCreated {
		return ErrInvitationAlreadyUsed
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return ErrInvalidInvitation
	}
	if roster != nil && roster.IsMember(inviteeID) {
		return ErrAlreadyMember
	}
	invitation.InviteeID = strings.TrimSpace(inviteeID)
	invitation.Fingerprint = fingerprint
	invitation.Status = InvitationAwaitingApproval
	state.Invitations[invitation.InvitationID] = invitation
	return nil
}

// ApproveInvitation is available to the owner and to any administrator.
func (state *State) ApproveInvitation(roster Roster, actorID string, invitationID string, approvalID string) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if roster == nil || !roster.IsAdministrator(actorID) {
		return ErrNotAdministrator
	}
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return ErrInvalidInvitation
	}
	invitation, ok := state.Invitations[strings.TrimSpace(invitationID)]
	if !ok {
		return ErrInvitationNotFound
	}
	if invitation.Status != InvitationAwaitingApproval {
		return ErrInvitationNotPending
	}
	if err := state.ValidateMemberAdmission(invitation.InviteeID); err != nil {
		return err
	}
	invitation.ApprovalID = approvalID
	invitation.Status = InvitationApproved
	state.Invitations[invitation.InvitationID] = invitation
	return nil
}

func (state *State) DenyInvitation(roster Roster, actorID string, invitationID string) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if roster == nil || !roster.IsAdministrator(actorID) {
		return ErrNotAdministrator
	}
	invitation, ok := state.Invitations[strings.TrimSpace(invitationID)]
	if !ok {
		return ErrInvitationNotFound
	}
	if invitation.Status != InvitationAwaitingApproval {
		return ErrInvitationNotPending
	}
	invitation.Status = InvitationDenied
	state.Invitations[invitation.InvitationID] = invitation
	return nil
}

func (state *State) PendingApprovals() []Invitation {
	if state == nil {
		return nil
	}
	pending := make([]Invitation, 0)
	for _, invitation := range state.Invitations {
		if invitation.Status == InvitationAwaitingApproval {
			pending = append(pending, invitation)
		}
	}
	return pending
}

// ---------------------------------------------------------------------------
// Publication.
// ---------------------------------------------------------------------------

// Publish records a library offered to the group pool. Only a member may
// publish into the live pool; a node awaiting admission stages through
// PublishCandidate instead.
func (state *State) Publish(roster Roster, publication Publication) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := state.validatePublication(&publication); err != nil {
		return err
	}
	if roster == nil || !roster.IsMember(publication.SourceNodeID) {
		return ErrNotMember
	}
	state.Published[publicationKey(publication.SourceNodeID, publication.Library.ID)] = publication
	return nil
}

// PublishCandidate stages a library for a node that is not yet admitted, so
// that the admission rule requiring a non-empty publication can be satisfied
// before approval.
func (state *State) PublishCandidate(roster Roster, publication Publication) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := state.validatePublication(&publication); err != nil {
		return err
	}
	if roster != nil && roster.IsMember(publication.SourceNodeID) {
		return ErrAlreadyMember
	}
	state.Candidates[publicationKey(publication.SourceNodeID, publication.Library.ID)] = publication
	return nil
}

func (state *State) validatePublication(publication *Publication) error {
	if strings.TrimSpace(publication.GroupID) != state.GroupID {
		return ErrGroupIDRequired
	}
	if err := validateNode(publication.SourceNodeID); err != nil {
		return err
	}
	if err := validateLibrary(publication.Library); err != nil {
		return err
	}
	if publication.PublishedAt.IsZero() {
		publication.PublishedAt = state.now()
	}
	return nil
}

func (state *State) Unpublish(sourceNodeID string, libraryID string) error {
	if err := validateNode(sourceNodeID); err != nil {
		return err
	}
	if err := validateLibraryID(libraryID); err != nil {
		return err
	}
	key := publicationKey(sourceNodeID, libraryID)
	if _, ok := state.Published[key]; !ok {
		return ErrPublicationRequired
	}
	delete(state.Published, key)
	return nil
}

// PublishedLibraryCount counts a node's live publications.
func (state *State) PublishedLibraryCount(nodeID string) int {
	return countPublications(state.Published, nodeID)
}

// CandidateLibraryCount counts a node's staged, pre-admission publications.
func (state *State) CandidateLibraryCount(nodeID string) int {
	return countPublications(state.Candidates, nodeID)
}

// ValidateMemberAdmission enforces the admission rule that a joining server
// must offer at least one non-empty library. A node already in the pool
// satisfies it directly; a joining node satisfies it through its candidates.
func (state *State) ValidateMemberAdmission(nodeID string) error {
	if err := validateNode(nodeID); err != nil {
		return err
	}
	if state.PublishedLibraryCount(nodeID)+state.CandidateLibraryCount(nodeID) < MinimumPublishedLibraries {
		return ErrInsufficientPublications
	}
	return nil
}

func countPublications(from map[string]Publication, nodeID string) int {
	nodeID = strings.TrimSpace(nodeID)
	count := 0
	for _, publication := range from {
		if publication.SourceNodeID == nodeID {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// Destination opt-out, blocks, and effective visibility.
// ---------------------------------------------------------------------------

func (state *State) SetOptOut(sourceNodeID string, libraryID string, optedOut bool) error {
	if err := validateNode(sourceNodeID); err != nil {
		return err
	}
	if err := validateLibraryID(libraryID); err != nil {
		return err
	}
	if _, ok := state.Published[publicationKey(sourceNodeID, libraryID)]; !ok {
		return ErrPublicationRequired
	}
	if !optedOut {
		if libraries, ok := state.OptOuts[sourceNodeID]; ok {
			delete(libraries, libraryID)
			if len(libraries) == 0 {
				delete(state.OptOuts, sourceNodeID)
			}
		}
		return nil
	}
	if state.OptOuts[sourceNodeID] == nil {
		state.OptOuts[sourceNodeID] = make(map[string]OptOut)
	}
	state.OptOuts[sourceNodeID][libraryID] = OptOut{
		GroupID:      state.GroupID,
		SourceNodeID: sourceNodeID,
		LibraryID:    libraryID,
		UpdatedAt:    state.now(),
	}
	return nil
}

func (state *State) IsOptedOut(sourceNodeID string, libraryID string) bool {
	if state == nil {
		return false
	}
	if _, ok := state.Published[publicationKey(sourceNodeID, libraryID)]; !ok {
		return false
	}
	libraries, ok := state.OptOuts[sourceNodeID]
	if !ok {
		return false
	}
	_, ok = libraries[libraryID]
	return ok
}

func (state *State) BlockPeer(peerID string) error {
	if err := validateNode(peerID); err != nil {
		return err
	}
	state.BlockedPeers[peerID] = true
	return nil
}

func (state *State) UnblockPeer(peerID string) error {
	if err := validateNode(peerID); err != nil {
		return err
	}
	delete(state.BlockedPeers, peerID)
	return nil
}

func (state *State) IsPeerBlocked(peerID string) bool {
	if state == nil {
		return false
	}
	return state.BlockedPeers[strings.TrimSpace(peerID)]
}

// CanConsume reports the federated half of effective visibility: the source
// is a current member, the library is published, this node has not opted out
// of it, and this node has not blocked the source. It asks the roster
// directly rather than trusting that Reconcile has run, so a stale
// publication from a removed member is never consumable. Local Jellyfin
// library permissions are the remaining, separate control.
func (state *State) CanConsume(roster Roster, sourceNodeID string, libraryID string) bool {
	if state == nil || roster == nil || !roster.IsMember(sourceNodeID) || state.IsPeerBlocked(sourceNodeID) {
		return false
	}
	if _, ok := state.Published[publicationKey(sourceNodeID, libraryID)]; !ok {
		return false
	}
	return !state.IsOptedOut(sourceNodeID, libraryID)
}

func (state *State) LibraryStatus(roster Roster, sourceNodeID string, libraryID string) string {
	if state == nil {
		return ""
	}
	if roster != nil && roster.IsEjected(sourceNodeID) {
		return "ejected"
	}
	publication, ok := state.Published[publicationKey(sourceNodeID, libraryID)]
	if !ok {
		return ""
	}
	if state.IsOptedOut(sourceNodeID, libraryID) {
		return publication.Library.CollectionType + ":opted-out"
	}
	return publication.Library.CollectionType + ":available"
}

// ---------------------------------------------------------------------------

func validateNode(nodeID string) error {
	if strings.TrimSpace(nodeID) == "" {
		return ErrNodeIDRequired
	}
	return nil
}

func validateLibrary(library Library) error {
	if err := validateLibraryID(library.ID); err != nil {
		return err
	}
	if strings.TrimSpace(library.Name) == "" {
		return errors.New("library name is required")
	}
	if strings.TrimSpace(library.CollectionType) == "" {
		return errors.New("library collection type is required")
	}
	return nil
}

func validateLibraryID(libraryID string) error {
	if strings.TrimSpace(libraryID) == "" {
		return ErrLibraryIDRequired
	}
	return nil
}

func publicationKey(sourceNodeID string, libraryID string) string {
	return strings.TrimSpace(sourceNodeID) + "/" + strings.TrimSpace(libraryID)
}
