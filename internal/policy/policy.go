// Package policy models the media-sharing controls of a Jellymesh group:
// library publication, destination opt-out, pairwise blocks, and the
// invitation and membership events that gate them.
//
// Group roles are not modeled here. The owner, the administrator pool, the
// member roster, succession, and dissolution are owned by internal/group, and
// this package consults that state rather than keeping a second copy of it.
package policy

import (
	"errors"
	"strings"
	"time"

	"jellymesh/internal/group"
)

const MinimumPublishedLibraries = 1

var (
	ErrGroupIDRequired          = errors.New("group ID is required")
	ErrOwnerIDRequired          = errors.New("owner node ID is required")
	ErrNodeIDRequired           = errors.New("node ID is required")
	ErrLibraryIDRequired        = errors.New("library ID is required")
	ErrPublicationRequired      = errors.New("library must be published before it can be opted out")
	ErrInsufficientPublications = errors.New("member must publish at least one library")
	ErrNotMember                = errors.New("node is not an active group member")
	ErrNotAdministrator         = errors.New("only an owner or administrator can perform this action")
	ErrCannotEjectOwner         = errors.New("the group owner cannot be ejected")
	ErrInvalidRevocation        = errors.New("revocation sequence must be greater than zero")
	ErrStaleRevocation          = errors.New("revocation sequence is stale")
	ErrInvalidAdmission         = errors.New("admission requires an approved invitation")
	ErrStaleMembershipEvent     = errors.New("membership event sequence is stale")
	ErrInvalidInvitation        = errors.New("invitation is invalid")
	ErrInvitationNotFound       = errors.New("invitation was not found")
	ErrInvitationExpired        = errors.New("invitation has expired")
	ErrInvitationAlreadyUsed    = errors.New("invitation has already been used")
	ErrInvitationNotPending     = errors.New("invitation is not awaiting an approval decision")
	ErrAlreadyMember            = errors.New("node is already an active group member")
)

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

type Admission struct {
	GroupID      string
	MemberID     string
	InvitationID string
	ApprovalID   string
	Sequence     uint64
	IssuedAt     time.Time
}

type Revocation struct {
	GroupID  string
	MemberID string
	Sequence uint64
	IssuedAt time.Time
}

type State struct {
	// Now supplies the current time. It is injectable so that invitation
	// expiry and membership windows can be exercised in tests without waiting
	// for real multi-day windows. A nil Now means time.Now.
	Now func() time.Time

	GroupID string

	// Roles is authoritative for the owner, the administrator pool, the member
	// roster, succession, and dissolution.
	Roles *group.State

	// Published holds libraries offered to the group by admitted members.
	Published map[string]Publication

	// Candidates holds libraries staged by a node that is not yet admitted.
	// The admission rule requires at least one non-empty publication before
	// approval, but a node cannot publish into the live pool until it is a
	// member. Candidates resolve that ordering: a joining or rejoining node
	// stages here, and a verified admission promotes the staged entries.
	Candidates map[string]Publication

	OptOuts            map[string]map[string]OptOut
	BlockedPeers       map[string]bool
	Invitations        map[string]Invitation
	EjectedMembers     map[string]bool
	MembershipSequence uint64
}

func NewState(groupID string, ownerID string) (*State, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, ErrGroupIDRequired
	}
	if strings.TrimSpace(ownerID) == "" {
		return nil, ErrOwnerIDRequired
	}
	roles, err := group.NewState(groupID, ownerID)
	if err != nil {
		return nil, err
	}
	return &State{
		GroupID:        groupID,
		Roles:          roles,
		Published:      make(map[string]Publication),
		Candidates:     make(map[string]Publication),
		OptOuts:        make(map[string]map[string]OptOut),
		BlockedPeers:   make(map[string]bool),
		Invitations:    make(map[string]Invitation),
		EjectedMembers: make(map[string]bool),
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
// Role and membership queries, delegated to internal/group.
// ---------------------------------------------------------------------------

func (state *State) IsMember(nodeID string) bool {
	if state == nil || state.Roles == nil {
		return false
	}
	return state.Roles.Members[strings.TrimSpace(nodeID)]
}

func (state *State) IsOwner(nodeID string) bool {
	if state == nil || state.Roles == nil {
		return false
	}
	return state.Roles.IsOwner(nodeID)
}

func (state *State) IsAdministrator(nodeID string) bool {
	if state == nil || state.Roles == nil {
		return false
	}
	return state.Roles.IsAdministrator(nodeID)
}

// Advance drives the succession and dissolution state machine owned by the
// roles state.
func (state *State) Advance(now time.Time) group.Decision {
	if state == nil || state.Roles == nil {
		return group.Decision{}
	}
	return state.Roles.Advance(now)
}

// ---------------------------------------------------------------------------
// Invitations.
// ---------------------------------------------------------------------------

func (state *State) CreateInvitation(inviterID string, invitationID string, codeHash string, expiresAt time.Time) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := validateNode(inviterID); err != nil {
		return err
	}
	if !state.IsMember(inviterID) {
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
func (state *State) RedeemInvitation(invitationID string, inviteeID string, fingerprint string) error {
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
	if state.IsMember(inviteeID) {
		return ErrAlreadyMember
	}
	invitation.InviteeID = strings.TrimSpace(inviteeID)
	invitation.Fingerprint = fingerprint
	invitation.Status = InvitationAwaitingApproval
	state.Invitations[invitation.InvitationID] = invitation
	return nil
}

// ApproveInvitation is available to the owner and to any administrator.
func (state *State) ApproveInvitation(actorID string, invitationID string, approvalID string) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if !state.IsAdministrator(actorID) {
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
	invitation.ApprovalID = approvalID
	invitation.Status = InvitationApproved
	state.Invitations[invitation.InvitationID] = invitation
	return nil
}

func (state *State) DenyInvitation(actorID string, invitationID string) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if !state.IsAdministrator(actorID) {
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
// Membership events.
// ---------------------------------------------------------------------------

// EjectMember removes an ordinary member through an owner or administrator.
// Administrator and owner protections are enforced by the roles state.
func (state *State) EjectMember(actorID string, memberID string) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := validateNode(actorID); err != nil {
		return err
	}
	if err := validateNode(memberID); err != nil {
		return err
	}
	if !state.IsAdministrator(actorID) {
		return ErrNotAdministrator
	}
	if state.IsOwner(memberID) {
		return ErrCannotEjectOwner
	}
	if err := state.Roles.Eject(actorID, memberID); err != nil {
		return err
	}
	if !state.EjectedMembers[memberID] {
		state.MembershipSequence++
	}
	state.eject(memberID)
	return nil
}

func (state *State) ApplyVerifiedRevocation(revocation Revocation) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if strings.TrimSpace(revocation.GroupID) != state.GroupID {
		return ErrGroupIDRequired
	}
	if err := validateNode(revocation.MemberID); err != nil {
		return err
	}
	if state.IsOwner(revocation.MemberID) {
		return ErrCannotEjectOwner
	}
	if revocation.Sequence == 0 {
		return ErrInvalidRevocation
	}
	if revocation.Sequence <= state.MembershipSequence {
		return ErrStaleRevocation
	}
	state.MembershipSequence = revocation.Sequence
	delete(state.Roles.Members, revocation.MemberID)
	delete(state.Roles.Admins, revocation.MemberID)
	state.eject(revocation.MemberID)
	return nil
}

// ApplyVerifiedAdmission admits a node whose invitation was approved, and
// promotes that node's staged candidate publications into the live pool.
func (state *State) ApplyVerifiedAdmission(admission Admission) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if strings.TrimSpace(admission.GroupID) != state.GroupID {
		return ErrGroupIDRequired
	}
	if err := validateNode(admission.MemberID); err != nil {
		return err
	}
	if strings.TrimSpace(admission.InvitationID) == "" || strings.TrimSpace(admission.ApprovalID) == "" {
		return ErrInvalidAdmission
	}
	invitation, ok := state.Invitations[admission.InvitationID]
	if !ok || invitation.Status != InvitationApproved ||
		invitation.InviteeID != admission.MemberID || invitation.ApprovalID != admission.ApprovalID {
		return ErrInvalidAdmission
	}
	if admission.Sequence == 0 {
		return ErrInvalidAdmission
	}
	if admission.Sequence <= state.MembershipSequence {
		return ErrStaleMembershipEvent
	}
	if err := state.ValidateMemberAdmission(admission.MemberID); err != nil {
		return err
	}
	state.MembershipSequence = admission.Sequence
	state.Roles.Members[admission.MemberID] = true
	delete(state.EjectedMembers, admission.MemberID)
	state.promoteCandidates(admission.MemberID)
	return nil
}

func (state *State) IsEjected(memberID string) bool {
	if state == nil {
		return false
	}
	return state.EjectedMembers[strings.TrimSpace(memberID)]
}

// eject clears every media-sharing artifact belonging to a removed node.
func (state *State) eject(nodeID string) {
	state.EjectedMembers[nodeID] = true
	state.removePublications(state.Published, nodeID)
	state.removePublications(state.Candidates, nodeID)
}

// ---------------------------------------------------------------------------
// Publication.
// ---------------------------------------------------------------------------

// Publish offers a library to the group pool. Only an admitted member may
// publish into the live pool; a node awaiting admission stages through
// PublishCandidate instead.
func (state *State) Publish(publication Publication) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := state.validatePublication(&publication); err != nil {
		return err
	}
	if !state.IsMember(publication.SourceNodeID) {
		return ErrNotMember
	}
	state.Published[publicationKey(publication.SourceNodeID, publication.Library.ID)] = publication
	return nil
}

// PublishCandidate stages a library for a node that is not yet admitted, so
// that the admission rule requiring a non-empty publication can be satisfied
// before approval. Staged entries are promoted by ApplyVerifiedAdmission.
func (state *State) PublishCandidate(publication Publication) error {
	if state == nil {
		return errors.New("policy state is nil")
	}
	if err := state.validatePublication(&publication); err != nil {
		return err
	}
	if state.IsMember(publication.SourceNodeID) {
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

func (state *State) promoteCandidates(nodeID string) {
	for key, publication := range state.Candidates {
		if publication.SourceNodeID == nodeID {
			state.Published[key] = publication
			delete(state.Candidates, key)
		}
	}
}

func (state *State) removePublications(from map[string]Publication, nodeID string) {
	for key, publication := range from {
		if publication.SourceNodeID == nodeID {
			delete(from, key)
		}
	}
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

// CanConsume reports the federated half of effective visibility:
// source publication AND NOT destination opt-out AND no block or ejection.
// Local Jellyfin library permissions are the remaining, separate control.
func (state *State) CanConsume(sourceNodeID string, libraryID string) bool {
	if state == nil || state.IsPeerBlocked(sourceNodeID) || state.IsEjected(sourceNodeID) {
		return false
	}
	if _, ok := state.Published[publicationKey(sourceNodeID, libraryID)]; !ok {
		return false
	}
	return !state.IsOptedOut(sourceNodeID, libraryID)
}

func (state *State) LibraryStatus(sourceNodeID string, libraryID string) string {
	if state == nil {
		return ""
	}
	if state.IsEjected(sourceNodeID) {
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
