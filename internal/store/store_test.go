package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "state", "jellymesh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestOpenAppliesMigrations(t *testing.T) {
	database := openTestDB(t)
	version, err := database.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("expected schema at %d, got %d", len(migrations), version)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jellymesh.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := first.SQL().Exec(`INSERT INTO peers(node_id,fingerprint,created_at,updated_at) VALUES('n','f','','')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()
	var count int
	if err := second.SQL().QueryRow(`SELECT count(*) FROM peers`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("reopening must not lose or duplicate data, got %d rows", count)
	}
}

func TestDatabaseFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "jellymesh.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database must not be group or world accessible, got %v", info.Mode().Perm())
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if directory.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database directory must be owner-only, got %v", directory.Mode().Perm())
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	database := openTestDB(t)
	sentinel := errors.New("deliberate failure")

	err := database.WithTx(context.Background(), func(transaction *sql.Tx) error {
		if _, err := transaction.Exec(
			`INSERT INTO peers(node_id,fingerprint,created_at,updated_at) VALUES('rolled','back','','')`,
		); err != nil {
			return err
		}
		// A multi-statement membership change that fails partway must leave no
		// trace; a half-applied revocation is exactly what the design forbids.
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the callback error to surface, got %v", err)
	}

	var count int
	if err := database.SQL().QueryRow(`SELECT count(*) FROM peers WHERE node_id='rolled'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("a failed transaction left its write behind")
	}
}

func TestWithTxCommitsOnSuccess(t *testing.T) {
	database := openTestDB(t)
	err := database.WithTx(context.Background(), func(transaction *sql.Tx) error {
		_, err := transaction.Exec(
			`INSERT INTO peers(node_id,fingerprint,created_at,updated_at) VALUES('kept','fp','','')`,
		)
		return err
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	var count int
	if err := database.SQL().QueryRow(`SELECT count(*) FROM peers WHERE node_id='kept'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatal("a successful transaction did not commit")
	}
}

func TestWithTxRollsBackOnPanic(t *testing.T) {
	database := openTestDB(t)
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("the panic should propagate to the caller")
			}
		}()
		_ = database.WithTx(context.Background(), func(transaction *sql.Tx) error {
			_, _ = transaction.Exec(
				`INSERT INTO peers(node_id,fingerprint,created_at,updated_at) VALUES('panicked','fp','','')`,
			)
			panic("boom")
		})
	}()

	var count int
	if err := database.SQL().QueryRow(`SELECT count(*) FROM peers WHERE node_id='panicked'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("a panicking transaction left its write behind")
	}
}

func TestForeignKeyCascadeRemovesMembers(t *testing.T) {
	database := openTestDB(t)
	mustExec(t, database, `INSERT INTO group_state(group_id,owner_id,status) VALUES('g','cedar','active')`)
	mustExec(t, database, `INSERT INTO members(group_id,node_id) VALUES('g','walnut')`)
	mustExec(t, database, `DELETE FROM group_state WHERE group_id='g'`)
	var count int
	if err := database.SQL().QueryRow(`SELECT count(*) FROM members`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("deleting a group must cascade to its members")
	}
}

func TestPublicationStateIsConstrained(t *testing.T) {
	database := openTestDB(t)
	_, err := database.SQL().Exec(
		`INSERT INTO publications(group_id,source_node_id,library_id,library_name,collection_type,state,published_at)
		 VALUES('g','cedar','movies','Movies','movies','bogus','')`)
	if err == nil {
		t.Fatal("an unknown publication state must be rejected by the schema")
	}
}

func mustExec(t *testing.T, database *DB, statement string) {
	t.Helper()
	if _, err := database.SQL().Exec(statement); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func TestWALAndForeignKeysAreEnabled(t *testing.T) {
	database := openTestDB(t)
	var mode string
	if err := database.SQL().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected WAL journal mode, got %q", mode)
	}
	var foreignKeys int
	if err := database.SQL().QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatal("foreign key enforcement must be on")
	}
}

func TestRetentionExpiryUsesTheIndex(t *testing.T) {
	database := openTestDB(t)
	// The retention sweep is the access pattern that motivated SQLite over a
	// key-value store. Assert the planner actually uses the index rather than
	// scanning every retained item.
	rows, err := database.SQL().Query(
		`EXPLAIN QUERY PLAN SELECT source_item_id FROM retention WHERE expires_at <= ?`,
		FormatTime(time.Now()),
	)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan += detail + " "
	}
	if !contains(plan, "idx_retention_expires") {
		t.Fatalf("retention expiry sweep is not using its index, plan was: %s", plan)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	original := time.Date(2026, 9, 25, 13, 45, 30, 123456789, time.UTC)
	parsed, err := ParseTime(FormatTime(original))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Equal(original) {
		t.Fatalf("round trip changed the value: %v -> %v", original, parsed)
	}
	empty, err := ParseTime(FormatTime(time.Time{}))
	if err != nil {
		t.Fatalf("parse zero: %v", err)
	}
	if !empty.IsZero() {
		t.Fatal("a zero time must round trip to zero")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// Migration 2 moves blocks off the peers table. A database created at version
// 1 with a blocked peer must still refuse that peer after upgrading.
func TestMigrationCarriesExistingBlocksForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "jellymesh.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		migrations[0],
		`INSERT INTO schema_migrations(version, applied_at) VALUES (1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO peers(node_id, fingerprint, trusted, blocked, created_at, updated_at)
		 VALUES ('maple', 'fingerprint-maple', 1, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("build version 1 database: %v", err)
		}
	}
	raw.Close()

	database, err := Open(path)
	if err != nil {
		t.Fatalf("open and migrate: %v", err)
	}
	defer database.Close()
	if version, _ := database.SchemaVersion(context.Background()); version != len(migrations) {
		t.Fatalf("schema version = %d, want %d", version, len(migrations))
	}
	if NewPeerRepository(database).IsTrusted("fingerprint-maple") {
		t.Fatal("a peer blocked before the migration must stay blocked after it")
	}
}

// C-ST-8: stored timestamps sort as strings in chronological order, including
// across values whose fractional seconds have different numbers of digits.
func TestStoredTimestampsSortChronologically(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	offsets := []time.Duration{
		0,
		100 * time.Millisecond,
		150 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		time.Second + time.Nanosecond,
	}
	for index := 1; index < len(offsets); index++ {
		earlier := FormatTime(base.Add(offsets[index-1]))
		later := FormatTime(base.Add(offsets[index]))
		if !(earlier < later) {
			t.Fatalf("%q does not sort before %q", earlier, later)
		}
	}
	for _, offset := range offsets {
		want := base.Add(offset)
		got, err := ParseTime(FormatTime(want))
		if err != nil || !got.Equal(want) {
			t.Fatalf("round trip of %v = %v, %v", want, got, err)
		}
	}
	// Values written in the previous, variable-width encoding still parse.
	if got, err := ParseTime("2026-01-01T00:00:00.5Z"); err != nil || !got.Equal(base.Add(500*time.Millisecond)) {
		t.Fatalf("legacy value parsed as %v, %v", got, err)
	}
}
