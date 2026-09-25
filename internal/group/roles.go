package group

import (
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	OwnerSuccessionTimeout          = 15 * 24 * time.Hour
	DissolutionNotificationInterval = 5 * 24 * time.Hour
)

var (
	ErrGroupIDRequired         = errors.New("group ID is required")
	ErrOwnerIDRequired         = errors.New("owner ID is required")
	ErrNotMember               = errors.New("node is not an active group member")
	ErrOwnerRequired           = errors.New("only the owner can perform this action")
	ErrCannotPromoteOwner      = errors.New("owner is already the owner")
	ErrAdminProtected          = errors.New("only the owner can eject another admin")
	ErrOwnerProtected          = errors.New("the owner cannot be ejected")
	ErrNotAdministrator        = errors.New("only an owner or administrator can perform this action")
	ErrNoAdminSuccessor        = errors.New("no administrator is available for succession")
	ErrOwnerSuccessionClosed   = errors.New("owner succession window has closed")
	ErrSuccessionNotDue        = errors.New("the owner absence window has not elapsed")
	ErrNotSuccessor            = errors.New("claimant is not the eligible successor")
	ErrInsufficientAttestation = errors.New("succession requires attestation from a quorum of members")
	ErrInvalidAttestation      = errors.New("attestation is invalid or does not cover the absence window")
)

type Status string

const (
	StatusActive             Status = "active"
	StatusSuccessionPending  Status = "succession_pending"
	StatusDissolutionPending Status = "dissolution_pending"
	StatusDissolved          Status = "dissolved"
)

type Administrator struct {
	PromotedAt time.Time
}

type Decision struct {
	Notify bool

	// SuccessionEligible names the administrator who may now claim ownership.
	// It is an invitation to act, not a completed promotion: the claim must
	// still be made explicitly and backed by a quorum of attestations.
	SuccessionEligible string

	Dissolve bool
}

// AbsenceAttestation is one member's signed statement that it has been unable
// to reach the owner since a given time. Attestations are what replace a local
// timer as the basis for succession.
type AbsenceAttestation struct {
	GroupID          string
	AbsentOwnerID    string
	AttestorID       string
	UnreachableSince time.Time
	IssuedAt         time.Time
}

// RequiredAbsenceAttestations returns how many attestations from other members
// a claimant must present.
//
// The bar is a strict majority of the members other than the absent owner. The
// claimant's own observation counts as one of those voices, since making the
// claim is itself an assertion that the owner is gone, so the number of
// *additional* attestations required is one fewer. A claimant may never attest
// for itself beyond that implicit voice.
//
//	members   non-owner   majority   attestations required
//	      2           1          1                       0
//	      3           2          2                       1
//	      5           4          3                       2
//	     20          19         10                       9
//
// A two-member group needs none, which is correct rather than lax: with only
// the owner and the claimant there is no second observer to disagree with, so
// there is no split-brain to prevent.
func RequiredAbsenceAttestations(memberCount int) int {
	if memberCount < 2 {
		return 0
	}
	return (memberCount - 1) / 2
}

type State struct {
	// Now supplies the current time. It is injectable so that succession and
	// dissolution windows can be exercised without waiting real days. A nil
	// Now means time.Now.
	Now func() time.Time

	GroupID               string
	OwnerID               string
	Members               map[string]bool
	Admins                map[string]Administrator
	Status                Status
	OwnerUnavailableSince time.Time
	SuccessionDeadline    time.Time
	DissolutionDeadline   time.Time
	LastNotificationAt    time.Time
}

func NewState(groupID string, ownerID string) (*State, error) {
	groupID = strings.TrimSpace(groupID)
	ownerID = strings.TrimSpace(ownerID)
	if groupID == "" {
		return nil, ErrGroupIDRequired
	}
	if ownerID == "" {
		return nil, ErrOwnerIDRequired
	}
	return &State{
		GroupID: groupID,
		OwnerID: ownerID,
		Members: map[string]bool{ownerID: true},
		Admins:  make(map[string]Administrator),
		Status:  StatusActive,
	}, nil
}

// now returns the state's current time, defaulting to the wall clock.
func (state *State) now() time.Time {
	if state == nil || state.Now == nil {
		return time.Now().UTC()
	}
	return state.Now().UTC()
}

func (state *State) IsOwner(nodeID string) bool {
	return state != nil && state.OwnerID == strings.TrimSpace(nodeID)
}

func (state *State) IsAdministrator(nodeID string) bool {
	if state == nil {
		return false
	}
	_, ok := state.Admins[strings.TrimSpace(nodeID)]
	return state.IsOwner(nodeID) || ok
}

func (state *State) PromoteAdmin(actorID string, memberID string) error {
	return state.PromoteAdminAt(actorID, memberID, state.now())
}

func (state *State) PromoteAdminAt(actorID string, memberID string, promotedAt time.Time) error {
	if state == nil {
		return errors.New("group state is nil")
	}
	if !state.IsOwner(actorID) {
		return ErrOwnerRequired
	}
	memberID = strings.TrimSpace(memberID)
	if memberID == "" {
		return ErrOwnerIDRequired
	}
	if memberID == state.OwnerID {
		return ErrCannotPromoteOwner
	}
	if !state.Members[memberID] {
		return ErrNotMember
	}
	if promotedAt.IsZero() {
		promotedAt = state.now()
	}
	state.Admins[memberID] = Administrator{PromotedAt: promotedAt.UTC()}
	return nil
}

func (state *State) DemoteAdmin(actorID string, adminID string) error {
	if state == nil {
		return errors.New("group state is nil")
	}
	if !state.IsOwner(actorID) {
		return ErrOwnerRequired
	}
	adminID = strings.TrimSpace(adminID)
	if _, ok := state.Admins[adminID]; !ok {
		return ErrNotMember
	}
	delete(state.Admins, adminID)
	return nil
}

func (state *State) Eject(actorID string, memberID string) error {
	if state == nil {
		return errors.New("group state is nil")
	}
	memberID = strings.TrimSpace(memberID)
	if !state.IsAdministrator(actorID) {
		return ErrNotAdministrator
	}
	if memberID == state.OwnerID {
		return ErrOwnerProtected
	}
	if _, isAdmin := state.Admins[memberID]; isAdmin && !state.IsOwner(actorID) {
		return ErrAdminProtected
	}
	if !state.Members[memberID] {
		return ErrNotMember
	}
	delete(state.Members, memberID)
	delete(state.Admins, memberID)
	return nil
}

func (state *State) MarkOwnerUnavailable(now time.Time) error {
	if state == nil {
		return errors.New("group state is nil")
	}
	if state.Status == StatusDissolved {
		return errors.New("group is dissolved")
	}
	if now.IsZero() {
		now = state.now()
	}
	now = now.UTC()
	if len(state.Admins) == 0 {
		state.Status = StatusDissolutionPending
		state.DissolutionDeadline = now.Add(OwnerSuccessionTimeout)
	} else {
		state.Status = StatusSuccessionPending
		state.OwnerUnavailableSince = now
		state.SuccessionDeadline = now.Add(OwnerSuccessionTimeout)
	}
	state.LastNotificationAt = time.Time{}
	return nil
}

func (state *State) MarkOwnerAvailable(now time.Time) error {
	if state == nil {
		return errors.New("group state is nil")
	}
	if state.Status == StatusDissolved {
		return errors.New("group is dissolved")
	}
	if state.Status == StatusActive {
		return nil
	}
	if now.IsZero() {
		now = state.now()
	}
	deadline := state.SuccessionDeadline
	if state.Status == StatusDissolutionPending {
		deadline = state.DissolutionDeadline
	}
	if !now.UTC().Before(deadline) {
		// The window has already closed. Resolve the state machine here rather
		// than leaving it pending until the next Advance, so that a recovery
		// call can never silently strand the group in a transitional state.
		if state.Status == StatusDissolutionPending {
			state.Status = StatusDissolved
		}
		return ErrOwnerSuccessionClosed
	}
	state.Status = StatusActive
	state.OwnerUnavailableSince = time.Time{}
	state.SuccessionDeadline = time.Time{}
	state.DissolutionDeadline = time.Time{}
	state.LastNotificationAt = time.Time{}
	return nil
}

func (state *State) Advance(now time.Time) Decision {
	if state == nil || state.Status == StatusActive || state.Status == StatusDissolved {
		return Decision{}
	}
	if now.IsZero() {
		now = state.now()
	}
	now = now.UTC()
	notificationDue := state.LastNotificationAt.IsZero() || !now.Before(state.LastNotificationAt.Add(DissolutionNotificationInterval))
	if state.Status == StatusSuccessionPending && !now.Before(state.SuccessionDeadline) {
		// Succession is never applied on a local timer. Each node observes the
		// owner's absence independently and on its own clock, so promoting here
		// would let two partitioned nodes install different owners and then sign
		// conflicting membership events. Advance only reports that a claim has
		// become eligible; ClaimOwnership applies it, and only against
		// attestations from a quorum of members.
		successor, err := state.selectSuccessor()
		if err != nil {
			state.Status = StatusDissolved
			return Decision{Notify: notificationDue, Dissolve: true}
		}
		if notificationDue {
			state.LastNotificationAt = now
		}
		return Decision{Notify: notificationDue, SuccessionEligible: successor}
	}
	if state.Status == StatusDissolutionPending && !now.Before(state.DissolutionDeadline) {
		state.Status = StatusDissolved
		return Decision{Notify: notificationDue, Dissolve: true}
	}
	if notificationDue {
		state.LastNotificationAt = now
		return Decision{Notify: true}
	}
	return Decision{}
}

func (state *State) selectSuccessor() (string, error) {
	if len(state.Admins) == 0 {
		return "", ErrNoAdminSuccessor
	}
	candidates := make([]string, 0, len(state.Admins))
	for adminID := range state.Admins {
		candidates = append(candidates, adminID)
	}
	sort.Slice(candidates, func(left int, right int) bool {
		leftTime := state.Admins[candidates[left]].PromotedAt
		rightTime := state.Admins[candidates[right]].PromotedAt
		if leftTime.Equal(rightTime) {
			return candidates[left] < candidates[right]
		}
		return leftTime.Before(rightTime)
	})
	return candidates[0], nil
}

// ClaimOwnership installs a new owner after the absence window has elapsed.
//
// It exists because succession cannot safely be a local decision. Every node
// observes the owner's reachability independently and on its own wall clock, a
// blocked peer sees no heartbeats from the peer it blocked, and a partitioned
// node sees none from anyone. If each node promoted on its own timer, two of
// them could install different owners and then sign conflicting admissions and
// revocations, with no rule to resolve the conflict.
//
// The confirmed product decision about *who* succeeds is preserved: the oldest
// administrator by promotion time, with member ID breaking exact ties. What
// changes is *how* nodes agree that the moment has arrived. A claimant must
// present attestations from a quorum of distinct other members, each stating
// that it too has been unable to reach the owner since before the deadline.
func (state *State) ClaimOwnership(claimantID string, attestations []AbsenceAttestation, now time.Time) error {
	if state == nil {
		return errors.New("group state is nil")
	}
	if state.Status == StatusDissolved {
		return errors.New("group is dissolved")
	}
	if state.Status != StatusSuccessionPending {
		return ErrSuccessionNotDue
	}
	if now.IsZero() {
		now = state.now()
	}
	now = now.UTC()
	if now.Before(state.SuccessionDeadline) {
		return ErrSuccessionNotDue
	}

	claimantID = strings.TrimSpace(claimantID)
	successor, err := state.selectSuccessor()
	if err != nil {
		return err
	}
	if claimantID != successor {
		return ErrNotSuccessor
	}

	required := RequiredAbsenceAttestations(len(state.Members))
	seen := make(map[string]bool, len(attestations))
	for _, attestation := range attestations {
		if err := state.validateAttestation(attestation, claimantID, seen); err != nil {
			return err
		}
		seen[strings.TrimSpace(attestation.AttestorID)] = true
	}
	if len(seen) < required {
		return ErrInsufficientAttestation
	}

	delete(state.Admins, claimantID)
	state.OwnerID = claimantID
	state.Status = StatusActive
	state.OwnerUnavailableSince = time.Time{}
	state.SuccessionDeadline = time.Time{}
	state.DissolutionDeadline = time.Time{}
	state.LastNotificationAt = time.Time{}
	return nil
}

// validateAttestation rejects anything that would let a single node manufacture
// a quorum: attestations from non-members, from the claimant itself, from the
// absent owner, duplicates, and any whose observation window is too recent to
// support the claim.
func (state *State) validateAttestation(attestation AbsenceAttestation, claimantID string, seen map[string]bool) error {
	attestorID := strings.TrimSpace(attestation.AttestorID)
	if attestorID == "" {
		return ErrInvalidAttestation
	}
	if strings.TrimSpace(attestation.GroupID) != state.GroupID {
		return ErrInvalidAttestation
	}
	if strings.TrimSpace(attestation.AbsentOwnerID) != state.OwnerID {
		return ErrInvalidAttestation
	}
	if attestorID == claimantID {
		return ErrInvalidAttestation
	}
	if attestorID == state.OwnerID {
		return ErrInvalidAttestation
	}
	if !state.Members[attestorID] {
		return ErrInvalidAttestation
	}
	if seen[attestorID] {
		return ErrInvalidAttestation
	}
	if attestation.UnreachableSince.IsZero() {
		return ErrInvalidAttestation
	}
	// The attestor must have observed the owner as unreachable since at least
	// as far back as this node's own absence window began. An attestation that
	// only noticed the owner recently does not support a fifteen-day claim.
	if attestation.UnreachableSince.UTC().After(state.OwnerUnavailableSince) {
		return ErrInvalidAttestation
	}
	return nil
}
