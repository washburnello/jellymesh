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

// TimeFormat is how every timestamp column is encoded. RFC3339 in UTC sorts
// lexicographically in the same order it sorts chronologically, so ordinary
// string comparison in SQL gives correct time ordering, and the stored value
// stays readable to an operator inspecting the database during an incident.
const TimeFormat = time.RFC3339Nano

var (
	ErrPathRequired = errors.New("a database path is required")
	ErrMigrationGap = errors.New("migrations must be numbered consecutively from 1")
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
	// This is load-bearing beyond performance. PeerRepository.Upsert detects a
	// fingerprint collision with a read followed by a write, which is only
	// race-free because no second connection can interleave between them. Two
	// nodes sharing a fingerprint would mean one can impersonate the other, so
	// raising this limit requires replacing that check with a constraint-based
	// guard first.
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
	return time.Parse(TimeFormat, value)
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
