package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/store"
)

const passphrase = "correct horse battery"

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	paths    Paths
	identity *node.Identity
	database *store.DB
	group    *membership.Group
	walnut   *node.Identity
}

func pathsIn(directory string) Paths {
	return Paths{
		Database: filepath.Join(directory, "state", "jellymesh.db"),
		Key:      filepath.Join(directory, "identity", "node.key"),
		Cert:     filepath.Join(directory, "identity", "node.crt"),
	}
}

// newFixture is cedar's node: it owns group-1, has admitted walnut, and holds
// a publication, an opt-out, a block, and a sync cursor.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{paths: pathsIn(t.TempDir())}
	var err error
	if f.identity, err = node.LoadOrCreate(f.paths.Key, f.paths.Cert, "cedar.example.org"); err != nil {
		t.Fatalf("identity: %v", err)
	}
	walnutDirectory := t.TempDir()
	if f.walnut, err = node.LoadOrCreate(filepath.Join(walnutDirectory, "k"), filepath.Join(walnutDirectory, "c"), "walnut.example.org"); err != nil {
		t.Fatalf("walnut: %v", err)
	}
	if f.database, err = store.Open(f.paths.Database); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { f.database.Close() })
	ctx := context.Background()
	if f.group, err = membership.Found(ctx, store.NewGroupLogRepository(f.database), store.NewPeerRepository(f.database), f.identity, "group-1", "cedar", "Cedar", "cedar.example.org", now); err != nil {
		t.Fatalf("found: %v", err)
	}
	f.sequence(t, f.group, grouplog.KindAdmission, grouplog.AdmissionBody{MemberID: "walnut", MemberKey: f.walnut.PublicKey(), InvitationID: "i-1", InviterID: "cedar"})

	state, _ := policy.NewState("group-1")
	var roster policy.Roster
	f.group.View(func(s *grouplog.State) { roster = s })
	if err := state.Publish(roster, policy.Publication{GroupID: "group-1", SourceNodeID: "walnut", Library: policy.Library{ID: "movies", Name: "Movies", CollectionType: "movies"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := state.SetOptOut("walnut", "movies", true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	if err := store.NewPolicyRepository(f.database).Save(ctx, state); err != nil {
		t.Fatalf("save policy: %v", err)
	}
	if err := store.NewPeerRepository(f.database).SetBlocked(ctx, "spruce", true); err != nil {
		t.Fatalf("block: %v", err)
	}
	if err := store.NewSyncRepository(f.database).SaveCursor(ctx, "walnut", "cursor-42", now); err != nil {
		t.Fatalf("sync cursor: %v", err)
	}
	return f
}

func (f *fixture) sequence(t *testing.T, group *membership.Group, kind grouplog.Kind, body any) grouplog.Event {
	t.Helper()
	proposal, err := grouplog.NewProposal(f.identity, "group-1", "cedar", kind, body, now)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	event, err := group.Sequence(context.Background(), f.identity, proposal, now)
	if err != nil {
		t.Fatalf("sequence: %v", err)
	}
	return event
}

func (f *fixture) backup(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := Create(context.Background(), f.database, f.identity, f.paths, passphrase, &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	return out.Bytes()
}

// C-ST-7: a backup restores the node's identity and every kind of durable
// state to a fresh location.
func TestBackupRestoresTheNode(t *testing.T) {
	f := newFixture(t)
	data := f.backup(t)
	ctx := context.Background()

	target := pathsIn(t.TempDir())
	manifest, err := Restore(ctx, passphrase, bytes.NewReader(data), target, "cedar.example.org")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if manifest.Fingerprint != f.identity.Fingerprint() {
		t.Fatal("the manifest should name the node")
	}
	restoredIdentity, err := node.LoadOrCreate(target.Key, target.Cert, "cedar.example.org")
	if err != nil || restoredIdentity.Fingerprint() != f.identity.Fingerprint() {
		t.Fatalf("restored identity: %v", err)
	}
	if info, _ := os.Stat(target.Key); info.Mode().Perm() != 0o600 {
		t.Fatalf("restored key mode %v, want 0600", info.Mode().Perm())
	}

	database, err := store.Open(target.Database)
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer database.Close()
	log, found, err := store.NewGroupLogRepository(database).Load(ctx, "group-1")
	if err != nil || !found || log.Head() != f.group.Head() {
		t.Fatalf("restored log: found=%v err=%v", found, err)
	}
	original, _ := store.NewPolicyRepository(f.database).Load(ctx, "group-1")
	restored, err := store.NewPolicyRepository(database).Load(ctx, "group-1")
	if err != nil || !reflect.DeepEqual(original, restored) {
		t.Fatalf("restored policy differs: %v", err)
	}
	if blocked, _ := store.NewPeerRepository(database).IsBlocked(ctx, "spruce"); !blocked {
		t.Fatal("the block should be restored")
	}
	if state, found, err := store.NewSyncRepository(database).Get(ctx, "walnut"); err != nil || !found || state.Cursor != "cursor-42" {
		t.Fatalf("restored sync state: %+v, %v, %v", state, found, err)
	}

	entries, _ := os.ReadDir(filepath.Dir(f.paths.Database))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".backup-") {
			t.Fatal("the staging directory must be removed")
		}
	}
}

// C-ST-10: the backup holds nothing in the clear.
func TestABackupHoldsNothingInTheClear(t *testing.T) {
	f := newFixture(t)
	data := f.backup(t)
	key, _ := os.ReadFile(f.paths.Key)
	for name, plaintext := range map[string][]byte{
		"private key PEM": key,
		"PEM header":      []byte("PRIVATE KEY"),
		"SQLite header":   []byte("SQLite format 3"),
		"group ID":        []byte("group-1"),
	} {
		if bytes.Contains(data, plaintext) {
			t.Errorf("the backup contains the %s in the clear", name)
		}
	}
}

// C-ST-10: a wrong passphrase, any alteration, and a file that is not a
// backup are all refused; a weak passphrase cannot make a backup.
func TestTamperingAndWrongPassphrasesAreRefused(t *testing.T) {
	f := newFixture(t)
	data := f.backup(t)

	if _, err := Inspect("wrong passphrase!", bytes.NewReader(data)); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	flipped := append([]byte(nil), data...)
	flipped[len(flipped)-10] ^= 1
	if _, err := Inspect(passphrase, bytes.NewReader(flipped)); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("altered ciphertext: %v", err)
	}
	salted := append([]byte(nil), data...)
	salted[len(magic)+1] ^= 1
	if _, err := Inspect(passphrase, bytes.NewReader(salted)); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("altered salt: %v", err)
	}
	weakened := append([]byte(nil), data...)
	copy(weakened[len(magic)+1+saltSize:], []byte{0, 0, 0, 1})
	if _, err := Inspect(passphrase, bytes.NewReader(weakened)); !errors.Is(err, ErrNotABackup) {
		t.Fatalf("weakened iteration count: %v", err)
	}
	if _, err := Inspect(passphrase, strings.NewReader("hello")); !errors.Is(err, ErrNotABackup) {
		t.Fatalf("not a backup: %v", err)
	}
	if err := Create(context.Background(), f.database, f.identity, f.paths, "short", &bytes.Buffer{}); !errors.Is(err, ErrWeakPassphrase) {
		t.Fatalf("weak passphrase: %v", err)
	}
}

// C-ST-10: a restore never overwrites an existing node.
func TestRestoreNeverOverwritesANode(t *testing.T) {
	f := newFixture(t)
	data := f.backup(t)
	before, _ := os.ReadFile(f.paths.Key)
	if _, err := Restore(context.Background(), passphrase, bytes.NewReader(data), f.paths, "cedar.example.org"); !errors.Is(err, ErrDestinationUsed) {
		t.Fatalf("restoring over the live node: %v", err)
	}
	if after, _ := os.ReadFile(f.paths.Key); !bytes.Equal(before, after) {
		t.Fatal("the existing key must be untouched")
	}
}

// C-ST-11: a restored owner may not sequence until it has caught up. Without
// the hold it would sign a second event for a slot its peers already hold,
// which is equivocation and halts every member.
func TestARestoredOwnerMustCatchUpBeforeSequencing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	data := f.backup(t)

	// After the backup, the owner sequences one more event, which a member
	// receives.
	walnutLog, err := grouplog.Replay(f.group.EventsAfter(0))
	if err != nil {
		t.Fatalf("walnut log: %v", err)
	}
	later := f.sequence(t, f.group, grouplog.KindPromote, grouplog.MemberBody{MemberID: "walnut"})
	if _, err := walnutLog.Append(later); err != nil {
		t.Fatalf("walnut receives: %v", err)
	}

	// The owner's disk is lost and the backup restored.
	target := pathsIn(t.TempDir())
	if _, err := Restore(ctx, passphrase, bytes.NewReader(data), target, "cedar.example.org"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	database, err := store.Open(target.Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()
	restored, err := membership.Open(ctx, store.NewGroupLogRepository(database), store.NewPeerRepository(database), "group-1")
	if err != nil {
		t.Fatalf("open group: %v", err)
	}
	// Unaware of the promotion it already published, the restored owner makes
	// the same decision again, as a new proposal.
	proposal, _ := grouplog.NewProposal(f.identity, "group-1", "cedar", grouplog.KindPromote, grouplog.MemberBody{MemberID: "walnut"}, now)
	if _, err := restored.Sequence(ctx, f.identity, proposal, now); !errors.Is(err, membership.ErrSequencingHeld) {
		t.Fatalf("sequencing before catching up: error = %v, want ErrSequencingHeld", err)
	}

	// What the hold prevents: sequencing now would reuse the slot.
	unheld, _ := grouplog.Replay(restored.EventsAfter(0))
	conflicting, err := unheld.Sequence(f.identity, proposal, now)
	if err != nil {
		t.Fatalf("unheld sequence: %v", err)
	}
	if _, err := walnutLog.Append(conflicting); !errors.Is(err, grouplog.ErrEquivocation) {
		t.Fatalf("without the hold, a member would see equivocation; got %v", err)
	}

	// Catching up, then confirming, lifts the hold.
	if _, err := restored.Receive(ctx, later); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if err := restored.ConfirmCaughtUp(ctx); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	fresh, _ := grouplog.NewProposal(f.identity, "group-1", "cedar", grouplog.KindDemote, grouplog.MemberBody{MemberID: "walnut"}, now)
	if _, err := restored.Sequence(ctx, f.identity, fresh, now); err != nil {
		t.Fatalf("sequencing after catching up: %v", err)
	}
}
