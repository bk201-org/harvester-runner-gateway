package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
	"github.com/bk201/harvester-runner-gateway/internal/config"
	_ "modernc.org/sqlite"
)

// sqliteAllocationStore keeps both per-kind high-water marks and append-only
// reservation history. Cluster objects remain the source of workload status.
type sqliteAllocationStore struct {
	db       *sql.DB
	prefixes config.IDPrefixes
}

func OpenSQLiteAllocationStore(ctx context.Context, path string, prefixes config.IDPrefixes) (AllocationStore, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("database.path must be an absolute filesystem path")
	}
	if prefixes.VM == "" || prefixes.Volume == "" {
		return nil, fmt.Errorf("resource ID prefixes are required")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open allocation database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close allocation database file: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open allocation database: %w", err)
	}
	db.SetMaxOpenConns(1)
	closeOnError := func(err error) (AllocationStore, error) {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("open allocation database: %w", err))
	}
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return closeOnError(fmt.Errorf("initialize allocation database: %w", err))
		}
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return closeOnError(fmt.Errorf("read allocation database version: %w", err))
	}
	if version == 0 {
		var existing int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").Scan(&existing)
		if err != nil {
			return closeOnError(fmt.Errorf("inspect allocation database: %w", err))
		}
		if existing != 0 {
			return closeOnError(fmt.Errorf("legacy allocation database schema: stop the gateway and reset database.path"))
		}
	} else if version != 2 {
		return closeOnError(fmt.Errorf("allocation database version %d requires a reset of database.path", version))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return closeOnError(fmt.Errorf("initialize allocation database: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS allocation_config (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			vm_prefix TEXT NOT NULL,
			volume_prefix TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS allocation_counters (
			kind TEXT PRIMARY KEY CHECK (kind IN ('vm', 'volume')),
			last_sequence INTEGER NOT NULL CHECK (last_sequence >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS allocation_reservations (
			kind TEXT NOT NULL,
			sequence INTEGER NOT NULL CHECK (sequence >= 1),
			namespace TEXT NOT NULL,
			repository_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			run_attempt TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			reserved_at TEXT NOT NULL,
			PRIMARY KEY (kind, sequence)
		)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return closeOnError(fmt.Errorf("initialize allocation database: %w", err))
		}
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_config (singleton, vm_prefix, volume_prefix) VALUES (1, ?, ?)`, prefixes.VM, prefixes.Volume); err != nil {
			return closeOnError(fmt.Errorf("store allocation prefixes: %w", err))
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
			return closeOnError(fmt.Errorf("set allocation database version: %w", err))
		}
	} else {
		var saved config.IDPrefixes
		if err := tx.QueryRowContext(ctx, "SELECT vm_prefix, volume_prefix FROM allocation_config WHERE singleton = 1").Scan(&saved.VM, &saved.Volume); err != nil {
			return closeOnError(fmt.Errorf("read allocation prefixes: %w", err))
		}
		if saved != prefixes {
			return closeOnError(fmt.Errorf("allocation prefix mismatch: stop the gateway, remove old resources, and reset database.path"))
		}
	}
	if err := tx.Commit(); err != nil {
		return closeOnError(fmt.Errorf("initialize allocation database: %w", err))
	}
	return &sqliteAllocationStore{db: db, prefixes: prefixes}, nil
}

func (s *sqliteAllocationStore) Close() error {
	return s.db.Close()
}

func (s *sqliteAllocationStore) Reserve(ctx context.Context, namespace string, owner auth.Owner, kind string, floor uint64) (string, uint64, error) {
	prefix := s.prefixes.ForKind(kind)
	if namespace == "" || !validOwner(owner) || prefix == "" {
		return "", 0, fmt.Errorf("%w: invalid allocation metadata", ErrInvalid)
	}
	if floor >= math.MaxInt64 {
		return "", 0, fmt.Errorf("%w: resource sequence exhausted", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("begin allocation reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The upsert obtains SQLite's writer lock and applies the cluster floor.
	_, err = tx.ExecContext(ctx, `INSERT INTO allocation_counters (kind, last_sequence)
		VALUES (?, ?)
		ON CONFLICT (kind) DO UPDATE SET last_sequence = max(last_sequence, excluded.last_sequence)`,
		kind, int64(max(floor, firstSequence-1)))
	if err != nil {
		return "", 0, fmt.Errorf("set allocation floor: %w", err)
	}
	var current int64
	if err := tx.QueryRowContext(ctx, "SELECT last_sequence FROM allocation_counters WHERE kind = ?", kind).Scan(&current); err != nil {
		return "", 0, fmt.Errorf("read allocation counter: %w", err)
	}
	if current < int64(firstSequence-1) || current == math.MaxInt64 {
		return "", 0, fmt.Errorf("%w: resource sequence exhausted", ErrInvalid)
	}
	next := uint64(current) + 1
	id, err := formatPublicID(prefix, next)
	if err != nil {
		return "", 0, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE allocation_counters SET last_sequence = ? WHERE kind = ?", int64(next), kind); err != nil {
		return "", 0, fmt.Errorf("advance allocation counter: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_reservations
		(kind, sequence, namespace, repository_id, run_id, run_attempt, resource_id, reserved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, int64(next), namespace, owner.RepositoryID, owner.RunID, owner.RunAttempt,
		id, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		return "", 0, fmt.Errorf("record allocation reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("commit allocation reservation: %w", err)
	}
	return id, next, nil
}

var _ AllocationStore = (*sqliteAllocationStore)(nil)
