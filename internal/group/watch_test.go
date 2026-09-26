package group

import (
	"errors"
	"testing"
	"time"
)

var start = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newTestWatch(t *testing.T) *Watch {
	t.Helper()
	watch, err := NewWatch("group-1", "cedar")
	if err != nil {
		t.Fatalf("new watch: %v", err)
	}
	return watch
}

func TestRequiredAbsenceAttestationSizes(t *testing.T) {
	for members, want := range map[int]int{1: 0, 2: 0, 3: 1, 5: 2, 20: 9} {
		if got := RequiredAbsenceAttestations(members); got != want {
			t.Fatalf("RequiredAbsenceAttestations(%d) = %d, want %d", members, got, want)
		}
	}
}

// C-PO-7: the absence window only makes a claim eligible. The watch has no
// power to change the owner.
func TestTheAbsenceWindowOnlyMakesAClaimEligible(t *testing.T) {
	watch := newTestWatch(t)
	if err := watch.OwnerUnavailable(start, true); err != nil {
		t.Fatalf("owner unavailable: %v", err)
	}
	if decision := watch.Advance(start.Add(14*24*time.Hour), true); decision.ClaimEligible || decision.Dissolve {
		t.Fatalf("before the deadline: %#v", decision)
	}
	decision := watch.Advance(start.Add(OwnerSuccessionTimeout), true)
	if !decision.ClaimEligible || decision.Dissolve {
		t.Fatalf("at the deadline: %#v", decision)
	}
	if watch.OwnerID != "cedar" {
		t.Fatal("the watch must never change the owner")
	}
}

func TestRepeatedUnavailabilityDoesNotPushTheDeadlineBack(t *testing.T) {
	watch := newTestWatch(t)
	watch.OwnerUnavailable(start, true)
	watch.OwnerUnavailable(start.Add(10*24*time.Hour), true)
	if !watch.OwnerUnavailableSince.Equal(start) {
		t.Fatalf("window moved to %v", watch.OwnerUnavailableSince)
	}
	if decision := watch.Advance(start.Add(OwnerSuccessionTimeout), true); !decision.ClaimEligible {
		t.Fatal("the deadline must still arrive when every heartbeat failure is reported")
	}
}

func TestOwnerReturningBeforeTheDeadlineRestoresNormalOperation(t *testing.T) {
	watch := newTestWatch(t)
	watch.OwnerUnavailable(start, true)
	if err := watch.OwnerAvailable(start.Add(time.Hour)); err != nil {
		t.Fatalf("owner available: %v", err)
	}
	if watch.Status != StatusActive || !watch.OwnerUnavailableSince.IsZero() {
		t.Fatalf("status %s since %v", watch.Status, watch.OwnerUnavailableSince)
	}
}

func TestOwnerReturningAfterTheDeadlineDoesNotCloseTheClaim(t *testing.T) {
	watch := newTestWatch(t)
	watch.OwnerUnavailable(start, true)
	if err := watch.OwnerAvailable(start.Add(OwnerSuccessionTimeout)); !errors.Is(err, ErrOwnerSuccessionClosed) {
		t.Fatalf("error = %v, want ErrOwnerSuccessionClosed", err)
	}
	if decision := watch.Advance(start.Add(OwnerSuccessionTimeout), true); !decision.ClaimEligible {
		t.Fatal("a succession already due stays eligible")
	}
}

func TestOwnerChangedWatchesTheNewOwner(t *testing.T) {
	watch := newTestWatch(t)
	watch.OwnerUnavailable(start, true)
	watch.OwnerChanged("walnut")
	if watch.OwnerID != "walnut" || watch.Status != StatusActive {
		t.Fatalf("owner %q status %s", watch.OwnerID, watch.Status)
	}
}

func TestGroupDissolvesWithoutOwnerOrAdmins(t *testing.T) {
	watch := newTestWatch(t)
	watch.OwnerUnavailable(start, false)
	if watch.Status != StatusDissolutionPending {
		t.Fatalf("status = %s", watch.Status)
	}
	decision := watch.Advance(start.Add(OwnerSuccessionTimeout), false)
	if !decision.Dissolve || watch.Status != StatusDissolved {
		t.Fatalf("did not dissolve: %#v, %s", decision, watch.Status)
	}
	if err := watch.OwnerUnavailable(start, true); !errors.Is(err, ErrGroupDissolved) {
		t.Fatalf("after dissolution: error = %v, want ErrGroupDissolved", err)
	}
}

func TestAdministratorsLeavingDuringTheWindowLeadsToDissolution(t *testing.T) {
	watch := newTestWatch(t)
	watch.OwnerUnavailable(start, true)
	decision := watch.Advance(start.Add(OwnerSuccessionTimeout), false)
	if !decision.Dissolve || decision.ClaimEligible {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestNotificationsRepeatEveryFiveDays(t *testing.T) {
	watch := newTestWatch(t)
	watch.OwnerUnavailable(start, false)
	if !watch.Advance(start, false).Notify {
		t.Fatal("the first notification is immediate")
	}
	if watch.Advance(start.Add(4*24*time.Hour), false).Notify {
		t.Fatal("no notification inside the interval")
	}
	if !watch.Advance(start.Add(DissolutionNotificationInterval), false).Notify {
		t.Fatal("a notification is due after five days")
	}
}
