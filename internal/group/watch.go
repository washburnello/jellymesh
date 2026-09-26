// Package group holds the parts of group coordination that are local to one
// node: its own observation of whether the owner is reachable, the windows
// that follow from that, and the quorum arithmetic succession uses.
//
// Who owns and administers the group is not here. That is replicated group
// state, derived from the log in internal/grouplog. A Watch cannot change the
// owner: it only tells this node when to attest to the owner's absence, when
// the eligible successor may claim, and when a group with no one left to
// lead it dissolves. Succession itself is a signed log event.
package group

import (
	"errors"
	"strings"
	"time"
)

const (
	OwnerSuccessionTimeout          = 15 * 24 * time.Hour
	DissolutionNotificationInterval = 5 * 24 * time.Hour
)

var (
	ErrGroupIDRequired       = errors.New("group ID is required")
	ErrOwnerIDRequired       = errors.New("owner ID is required")
	ErrGroupDissolved        = errors.New("group is dissolved")
	ErrOwnerSuccessionClosed = errors.New("owner succession window has closed")
)

// RequiredAbsenceAttestations returns how many attestations from other members
// a claimant must present.
//
// The bar is a strict majority of the members other than the absent owner. The
// claimant's own observation counts as one of those voices, since making the
// claim is itself an assertion that the owner is gone, so the number of
// *additional* attestations required is one fewer.
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

type Status string

const (
	StatusActive             Status = "active"
	StatusSuccessionPending  Status = "succession_pending"
	StatusDissolutionPending Status = "dissolution_pending"
	StatusDissolved          Status = "dissolved"
)

// Decision is what Advance tells the node to do now.
type Decision struct {
	Notify bool

	// ClaimEligible means the owner has been unreachable for the full window
	// and an administrator exists to succeed. The eligible successor may now
	// collect attestations and claim; every other member may attest. It is
	// an invitation to act, never a change of owner.
	ClaimEligible bool

	Dissolve bool
}

// Watch is this node's observation of one owner's availability.
type Watch struct {
	GroupID string
	// OwnerID is the owner being watched. When the log's owner changes,
	// OwnerChanged starts watching the new one.
	OwnerID               string
	Status                Status
	OwnerUnavailableSince time.Time
	SuccessionDeadline    time.Time
	DissolutionDeadline   time.Time
	LastNotificationAt    time.Time
}

func NewWatch(groupID string, ownerID string) (*Watch, error) {
	groupID = strings.TrimSpace(groupID)
	ownerID = strings.TrimSpace(ownerID)
	if groupID == "" {
		return nil, ErrGroupIDRequired
	}
	if ownerID == "" {
		return nil, ErrOwnerIDRequired
	}
	return &Watch{GroupID: groupID, OwnerID: ownerID, Status: StatusActive}, nil
}

// OwnerUnavailable records that this node can no longer reach the owner. It
// starts the absence window once: calling it again while the window is open
// leaves the window where it is, so a node that reports every failed
// heartbeat does not keep pushing its own deadline back. With no
// administrator to succeed, the window leads to dissolution instead.
func (watch *Watch) OwnerUnavailable(now time.Time, hasAdministrators bool) error {
	if watch.Status == StatusDissolved {
		return ErrGroupDissolved
	}
	if watch.Status != StatusActive {
		return nil
	}
	now = now.UTC()
	watch.OwnerUnavailableSince = now
	if hasAdministrators {
		watch.Status = StatusSuccessionPending
		watch.SuccessionDeadline = now.Add(OwnerSuccessionTimeout)
	} else {
		watch.Status = StatusDissolutionPending
		watch.DissolutionDeadline = now.Add(OwnerSuccessionTimeout)
	}
	watch.LastNotificationAt = time.Time{}
	return nil
}

// OwnerAvailable records that the owner is reachable again. Before the
// deadline this restores normal operation. After it, the window has closed:
// a pending dissolution completes, and a pending succession stays eligible,
// because other members may already have attested and the claim may already
// be in the log.
func (watch *Watch) OwnerAvailable(now time.Time) error {
	if watch.Status == StatusDissolved {
		return ErrGroupDissolved
	}
	if watch.Status == StatusActive {
		return nil
	}
	deadline := watch.SuccessionDeadline
	if watch.Status == StatusDissolutionPending {
		deadline = watch.DissolutionDeadline
	}
	if !now.UTC().Before(deadline) {
		if watch.Status == StatusDissolutionPending {
			watch.Status = StatusDissolved
		}
		return ErrOwnerSuccessionClosed
	}
	watch.reset()
	return nil
}

// OwnerChanged starts watching a new owner, as when a succession event is
// applied from the log.
func (watch *Watch) OwnerChanged(ownerID string) {
	if watch.Status == StatusDissolved {
		return
	}
	watch.OwnerID = strings.TrimSpace(ownerID)
	watch.reset()
}

func (watch *Watch) reset() {
	watch.Status = StatusActive
	watch.OwnerUnavailableSince = time.Time{}
	watch.SuccessionDeadline = time.Time{}
	watch.DissolutionDeadline = time.Time{}
	watch.LastNotificationAt = time.Time{}
}

// Advance reports what the node should do at now. hasAdministrators is read
// from the log at the moment of asking, since the administrator set may have
// changed since the window opened.
func (watch *Watch) Advance(now time.Time, hasAdministrators bool) Decision {
	if watch.Status == StatusActive || watch.Status == StatusDissolved {
		return Decision{}
	}
	now = now.UTC()
	notificationDue := watch.LastNotificationAt.IsZero() || !now.Before(watch.LastNotificationAt.Add(DissolutionNotificationInterval))
	if notificationDue {
		watch.LastNotificationAt = now
	}

	switch watch.Status {
	case StatusSuccessionPending:
		if now.Before(watch.SuccessionDeadline) {
			return Decision{Notify: notificationDue}
		}
		if !hasAdministrators {
			watch.Status = StatusDissolved
			return Decision{Notify: notificationDue, Dissolve: true}
		}
		return Decision{Notify: notificationDue, ClaimEligible: true}
	case StatusDissolutionPending:
		if now.Before(watch.DissolutionDeadline) {
			return Decision{Notify: notificationDue}
		}
		watch.Status = StatusDissolved
		return Decision{Notify: notificationDue, Dissolve: true}
	}
	return Decision{}
}
