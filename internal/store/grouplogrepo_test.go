package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"jellymesh/internal/group"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/node"
)

var logTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

type logFixture struct {
	log        *grouplog.Log
	identities map[string]*node.Identity
}

// newLogFixture founds group-1 with cedar as owner, admits walnut, birch,
// and spruce, and promotes walnut so that it can succeed.
func newLogFixture(t *testing.T) *logFixture {
	t.Helper()
	f := &logFixture{identities: map[string]*node.Identity{}}
	for _, name := range []string{"cedar", "walnut", "birch", "spruce"} {
		directory := t.TempDir()
		identity, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
		if err != nil {
			t.Fatalf("identity: %v", err)
		}
		f.identities[name] = identity
	}
	log, err := grouplog.Create(f.identities["cedar"], "group-1", "cedar", "Cedar", "cedar.example.org", logTime)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.log = log
	for _, name := range []string{"walnut", "birch", "spruce"} {
		f.sequence(t, grouplog.KindAdmission, grouplog.AdmissionBody{
			MemberID: name, MemberKey: f.identities[name].PublicKey(), InvitationID: "invite-" + name, InviterID: "cedar",
		})
	}
	f.sequence(t, grouplog.KindPromote, grouplog.MemberBody{MemberID: "walnut"})
	return f
}

func (f *logFixture) proposal(t *testing.T, kind grouplog.Kind, body any) grouplog.Proposal {
	t.Helper()
	proposal, err := grouplog.NewProposal(f.identities["cedar"], "group-1", "cedar", kind, body, logTime)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	return proposal
}

func (f *logFixture) sequence(t *testing.T, kind grouplog.Kind, body any) grouplog.Event {
	t.Helper()
	event, err := f.log.Sequence(f.identities["cedar"], f.proposal(t, kind, body), logTime)
	if err != nil {
		t.Fatalf("sequence: %v", err)
	}
	return event
}

func (f *logFixture) copyLog(t *testing.T) *grouplog.Log {
	t.Helper()
	copied, err := grouplog.Replay(f.log.EventsAfter(0))
	if err != nil {
		t.Fatalf("copy log: %v", err)
	}
	return copied
}

func reopen(t *testing.T, path string) *DB {
	t.Helper()
	database, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// C-ST-9: the group log survives a restart and reloads to identical state.
func TestGroupLogRepositoryRoundTrip(t *testing.T) {
	f := newLogFixture(t)
	path := filepath.Join(t.TempDir(), "state", "jellymesh.db")
	ctx := context.Background()

	first := reopen(t, path)
	if err := NewGroupLogRepository(first).Save(ctx, f.log); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Saving again, and after one more event, is incremental and idempotent.
	if err := NewGroupLogRepository(first).Save(ctx, f.log); err != nil {
		t.Fatalf("second save: %v", err)
	}
	f.sequence(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "spruce"})
	if err := NewGroupLogRepository(first).Save(ctx, f.log); err != nil {
		t.Fatalf("save after append: %v", err)
	}
	first.Close()

	loaded, found, err := NewGroupLogRepository(reopen(t, path)).Load(ctx, "group-1")
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if loaded.Head() != f.log.Head() || !reflect.DeepEqual(loaded.State(), f.log.State()) {
		t.Fatal("the reloaded log differs from the saved one")
	}
	if _, found, err := NewGroupLogRepository(reopen(t, path)).Load(ctx, "group-2"); found || err != nil {
		t.Fatalf("unknown group: found=%v err=%v", found, err)
	}
}

// C-ST-9: a stored log is re-verified on load, so tampering is refused.
func TestGroupLogRepositoryRefusesATamperedStore(t *testing.T) {
	f := newLogFixture(t)
	database := openTestDB(t)
	ctx := context.Background()
	if err := NewGroupLogRepository(database).Save(ctx, f.log); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := database.SQL().Exec(
		`UPDATE group_log SET event = replace(event, '"kind":"promote"', '"kind":"demote"') WHERE sequence = 5`,
	); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, _, err := NewGroupLogRepository(database).Load(ctx, "group-1"); err == nil {
		t.Fatal("a tampered stored log must not load")
	}
}

// C-ST-9: a succession's truncation is persisted, and the removed events are
// kept rather than deleted.
func TestGroupLogRepositoryPersistsSupersessionAndKeepsTheRemovedEvents(t *testing.T) {
	f := newLogFixture(t)
	database := openTestDB(t)
	ctx := context.Background()
	repo := NewGroupLogRepository(database)

	majority := f.copyLog(t)
	base := f.log.State()
	var attestations []grouplog.Attestation
	for _, attestor := range []string{"birch"} {
		attestation, err := grouplog.Attest(f.identities[attestor], attestor, base, logTime, logTime.Add(group.OwnerSuccessionTimeout+time.Hour))
		if err != nil {
			t.Fatalf("attest: %v", err)
		}
		attestations = append(attestations, attestation)
	}

	// This node still reached the old owner, and stored its later event.
	stale := f.sequence(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "spruce"})
	if err := repo.Save(ctx, f.log); err != nil {
		t.Fatalf("save stale: %v", err)
	}

	claim, err := majority.Claim(f.identities["walnut"], "walnut", attestations, logTime)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if outcome, err := f.log.Append(claim); err != nil || outcome != grouplog.Superseded {
		t.Fatalf("append claim: %v, %v", outcome, err)
	}
	if err := repo.Save(ctx, f.log); err != nil {
		t.Fatalf("save after supersession: %v", err)
	}

	loaded, _, err := repo.Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Head() != majority.Head() || !loaded.State().IsOwner("walnut") {
		t.Fatal("the stored log should follow the succession")
	}
	superseded, err := repo.Superseded(ctx, "group-1")
	if err != nil || len(superseded) != 1 || superseded[0].Hash() != stale.Hash() {
		t.Fatalf("superseded = %d events, err %v; want the stale event", len(superseded), err)
	}
}

// C-PO-19 across a restart: a halted log stays halted.
func TestGroupLogRepositoryHaltSurvivesRestart(t *testing.T) {
	f := newLogFixture(t)
	path := filepath.Join(t.TempDir(), "state", "jellymesh.db")
	ctx := context.Background()
	follower := f.copyLog(t)

	first := f.sequence(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "spruce"})
	// A rival log from just before first, which sequences a different event.
	rival, err := grouplog.Replay(f.log.EventsAfter(0)[:first.Sequence-1])
	if err != nil {
		t.Fatalf("rival: %v", err)
	}
	second, err := rival.Sequence(f.identities["cedar"], f.proposal(t, grouplog.KindEjection, grouplog.MemberBody{MemberID: "birch"}), logTime)
	if err != nil {
		t.Fatalf("rival sequence: %v", err)
	}
	follower.Append(first)
	if _, err := follower.Append(second); !errors.Is(err, grouplog.ErrEquivocation) {
		t.Fatalf("error = %v, want ErrEquivocation", err)
	}

	database := reopen(t, path)
	if err := NewGroupLogRepository(database).Save(ctx, follower); err != nil {
		t.Fatalf("save: %v", err)
	}
	database.Close()

	loaded, _, err := NewGroupLogRepository(reopen(t, path)).Load(ctx, "group-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, halted := loaded.Halted(); !halted {
		t.Fatal("a halted log must stay halted after a restart")
	}
}
