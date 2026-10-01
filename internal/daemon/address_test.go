package daemon

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"jellymesh/internal/config"
)

func (d *testDaemon) addressInGroup(t *testing.T, of *testDaemon) string {
	t.Helper()
	for _, member := range d.status().Group.Members {
		if member.NodeID == of.node.NodeID() {
			return member.Address
		}
	}
	t.Fatalf("%s is not in %s's roster", of.name, d.name)
	return ""
}

// C-NT-8: a member that moves proposes its new address itself; the owner
// sequences it only after reaching the member's key there, and from then on
// dials it there. An address where another key answers is refused, and the
// group keeps the old one.
func TestAMemberThatMovesReadvertisesItsAddress(t *testing.T) {
	cedar := startDaemon(t, "cedar", t.TempDir(), "127.0.0.1:0")
	walnutDir := t.TempDir()
	walnut := startDaemon(t, "walnut", walnutDir, "127.0.0.1:0")
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	join(t, cedar, walnut, cedar)
	first := walnut.address
	if got := cedar.addressInGroup(t, walnut); got != first {
		t.Fatalf("walnut joined at %q, roster says %q", first, got)
	}

	// walnut moves: a new listener, a new advertised address.
	walnut.shutdown()
	walnut = startDaemon(t, "walnut", walnutDir, "127.0.0.1:0")
	if walnut.address == first {
		t.Fatal("the test needs a new port")
	}
	walnut.node.readvertise(context.Background())
	if change := walnut.status().Address; change.Change != "sequenced" || change.InGroup != walnut.address || change.Configured != walnut.address {
		t.Fatalf("walnut's change: %+v", change)
	}
	if got := cedar.addressInGroup(t, walnut); got != walnut.address {
		t.Fatalf("cedar should dial walnut at %q, roster says %q", walnut.address, got)
	}
	// cedar reaches walnut at the new address.
	if result := cedar.sync(); result.Reached == 0 {
		t.Fatalf("cedar could not reach walnut at its new address: %+v", result)
	}
	// Already current, and not held back by the interval: nothing to do.
	sequence := cedar.status().Group.Sequence
	walnut.node.mutex.Lock()
	walnut.node.readvertised = time.Time{}
	walnut.node.mutex.Unlock()
	walnut.node.readvertise(context.Background())
	if cedar.status().Group.Sequence != sequence {
		t.Fatal("an unchanged address must not be proposed again")
	}

	// walnut claims an address where cedar's key answers.
	walnut.shutdown()
	walnut = startDaemonWith(t, "walnut", walnutDir, "127.0.0.1:0", func(cfg *config.Config) { cfg.PublicHostname = cedar.address })
	walnut.node.readvertise(context.Background())
	change := walnut.status().Address
	if !strings.HasPrefix(change.Change, "refused") || change.InGroup == cedar.address {
		t.Fatalf("an address answered by another key must be refused: %+v", change)
	}
	if got := cedar.addressInGroup(t, walnut); got == cedar.address {
		t.Fatal("the group took an address another node answers at")
	}
}

// C-NT-8: when the owner moves, members only know its old address, so it
// hands them the change itself, and they then reach it at the new one. A
// pushed event that does not verify is refused.
func TestAnOwnerThatMovesTellsItsMembers(t *testing.T) {
	cedarDir := t.TempDir()
	cedar := startDaemon(t, "cedar", cedarDir, "127.0.0.1:0")
	walnut := startDaemon(t, "walnut", t.TempDir(), "127.0.0.1:0")
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	join(t, cedar, walnut, cedar)
	walnut.sync()

	cedar.shutdown()
	cedar = startDaemon(t, "cedar", cedarDir, "127.0.0.1:0")
	cedar.node.readvertise(context.Background())
	if got := walnut.addressInGroup(t, cedar); got != cedar.address {
		t.Fatalf("walnut should have been told cedar's new address %q, has %q", cedar.address, got)
	}
	if result := walnut.sync(); result.Reached == 0 {
		t.Fatalf("walnut could not reach the owner at its new address: %+v", result)
	}

	// A forged event pushed by a member is refused and changes nothing.
	runtime, _ := walnut.node.current()
	events := runtime.group.EventsAfter(0)
	forged := events[len(events)-1]
	forged.Sequence++
	forged.PrevHash = forged.Hash()
	before := cedar.status().Group.Head
	peer := walnut.node.otherMembers(runtime)[0]
	if err := walnut.node.client.Push(context.Background(), peer, runtime.id, forged); err == nil {
		t.Fatal("a forged event was accepted")
	}
	if cedar.status().Group.Head != before {
		t.Fatal("a forged event changed the owner's log")
	}
}
