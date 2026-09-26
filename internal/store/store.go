// Package store owns Jellymesh's durable state.
//
// Everything a node must remember across a restart lives here: peer trust,
// group membership and roles, library publication and opt-out decisions,
// invitations, the per-user history ledger, deletion retention records, and
// per-peer sync cursors. The domain packages model the rules; this package
// models where the answers are kept.
//
// SQLite was chosen over an embedded key-value store because two of the access
// patterns genuinely want an index rather than a hand-maintained bucket:
// listing a catalog by source node during ejection or resynchronization, and
// sweeping retention records by expiry. The pure-Go driver is used so that the
// appliance image can still be cross-compiled without a C toolchain.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// TimeFormat is how every timestamp column is encoded: RFC 3339 in UTC with a
// fixed nine-digit fraction. Fixed width is what makes string comparison in
// SQL agree with chronological order, which the retention sweep and any
// ORDER BY on a time column rely on. time.RFC3339Nano does not have that
// property: it trims trailing zeros, so "...:00Z" sorts after "...:00.5Z".
// The value also stays readable to an operator inspecting the database.
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

var (
	ErrPathRequired = errors.New("a database path is required")
	ErrSchemaTooNew = errors.New("the database schema is newer than this build understands")
)

// DB is a handle to the node's durable state.
type DB struct {
	sql *sql.DB
}

// Open prepares the database at path, creating it and applying any outstanding
// migrations. The containing directory is created 0700: the database holds
// trust decisions and invitation material and should not be world-readable.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, ErrPathRequired
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// MkdirAll's mode is masked by the umask, so re-apply it explicitly.
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("set database directory permissions: %w", err)
	}

	handle, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// A single connection removes any possibility of SQLITE_BUSY between this
	// process's own goroutines. A home server's federation workload is a
	// handful of peers on five-minute heartbeats and hourly syncs, so the lost
	// read concurrency costs nothing measurable, and the failure mode it
	// removes is the kind that only appears under load in production.
	//
	// PeerRepository.Upsert checks for a fingerprint collision with a read
	// before its write so it can return a descriptive ErrFingerprintInUse. The
	// UNIQUE constraint on peers.fingerprint is what actually guarantees two
	// nodes never share a key, so a second connection racing between the read
	// and the write would surface as a constraint error, not as a duplicate.
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	handle.SetConnMaxLifetime(0)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		var ignored string
		// Some pragmas return a row and some do not; either is fine.
		if err := handle.QueryRow(pragma).Scan(&ignored); err != nil && !errors.Is(err, sql.ErrNoRows) {
			if _, execErr := handle.Exec(pragma); execErr != nil {
				handle.Close()
				return nil, fmt.Errorf("apply %q: %w", pragma, execErr)
			}
		}
	}

	database := &DB{sql: handle}
	if err := database.migrate(context.Background()); err != nil {
		handle.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		handle.Close()
		return nil, fmt.Errorf("set database permissions: %w", err)
	}
	return database, nil
}

func (database *DB) Close() error {
	if database == nil || database.sql == nil {
		return nil
	}
	return database.sql.Close()
}

// SQL exposes the underlying handle for repository packages built on top of
// this one. Callers should prefer WithTx for anything that writes.
func (database *DB) SQL() *sql.DB { return database.sql }

// WithTx runs fn inside a transaction, committing when it returns nil and
// rolling back on any error or panic. Every multi-statement write must go
// through it: a partially applied membership or revocation event is precisely
// the kind of state the design says must never be observable.
func (database *DB) WithTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	transaction, err := database.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = transaction.Rollback()
			panic(recovered)
		}
		if err != nil {
			_ = transaction.Rollback()
		}
	}()
	if err = fn(transaction); err != nil {
		return err
	}
	if err = transaction.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Snapshot writes a consistent copy of the database to path, which must not
// exist. SQLite's VACUUM INTO produces it from a single read transaction, so
// the copy is never a mix of before and after a concurrent write. The copy is
// made owner-only before anything else can read it.
func (database *DB) Snapshot(ctx context.Context, path string) error {
	if _, err := database.sql.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		os.Remove(path)
		return fmt.Errorf("restrict snapshot permissions: %w", err)
	}
	return nil
}

// SchemaVersion reports the highest migration applied.
func (database *DB) SchemaVersion(ctx context.Context) (int, error) {
	var version sql.NullInt64
	err := database.sql.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, err
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// FormatTime and ParseTime are the only sanctioned conversions for timestamp
// columns, so that every repository stores them identically.
func FormatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(TimeFormat)
}

func ParseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	// RFC3339Nano parsing accepts any fraction length, so values written in
	// the fixed-width format and any written before it both parse.
	return time.Parse(time.RFC3339Nano, value)
}

// nowUTC is a package-level seam so that migration timestamps can be made
// deterministic in tests without threading a clock through Open.
var nowUTC = func() time.Time { return time.Now().UTC() }

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so a repository can
// share one scan function between a single-row lookup and a list query.
type rowScanner interface {
	Scan(dest ...any) error
}

// boolToInt encodes a Go bool for SQLite, which has no native boolean type.
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
