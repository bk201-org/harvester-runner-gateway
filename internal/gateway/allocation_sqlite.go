package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	_ "modernc.org/sqlite"
)

// sqliteAllocationStore keeps both the current high-water mark and an
// append-only reservation history. Cluster objects remain the source of
// workload status; no request bodies or credentials are stored here.
type sqliteAllocationStore struct {
	db *sql.DB
}

func OpenSQLiteAllocationStore(ctx context.Context, path string) (AllocationStore, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("database.path must be an absolute filesystem path")
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
		`CREATE TABLE IF NOT EXISTS allocation_counters (
			namespace TEXT NOT NULL,
			repository_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			run_attempt TEXT NOT NULL,
			kind TEXT NOT NULL,
			last_sequence INTEGER NOT NULL CHECK (last_sequence >= 0),
			PRIMARY KEY (namespace, repository_id, run_id, run_attempt, kind)
		)`,
		`CREATE TABLE IF NOT EXISTS allocation_reservations (
			namespace TEXT NOT NULL,
			repository_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			run_attempt TEXT NOT NULL,
			kind TEXT NOT NULL,
			sequence INTEGER NOT NULL CHECK (sequence > 0),
			resource_id TEXT NOT NULL,
			reserved_at TEXT NOT NULL,
			PRIMARY KEY (namespace, repository_id, run_id, run_attempt, kind, sequence)
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return closeOnError(fmt.Errorf("initialize allocation database: %w", err))
		}
	}
	return &sqliteAllocationStore{db: db}, nil
}

func (s *sqliteAllocationStore) Close() error {
	return s.db.Close()
}

func (s *sqliteAllocationStore) Reserve(ctx context.Context, namespace string, owner auth.Owner, kind string, floor uint64) (string, uint64, error) {
	if namespace == "" || (kind != "vm" && kind != "volume") {
		return "", 0, fmt.Errorf("%w: invalid allocation scope", ErrInvalid)
	}
	if floor >= math.MaxInt64 {
		return "", 0, fmt.Errorf("%w: resource sequence exhausted", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("begin allocation reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	args := []any{namespace, owner.RepositoryID, owner.RunID, owner.RunAttempt, kind}
	// This first statement obtains SQLite's writer lock. A recovered Kubernetes
	// sequence only sets a floor: it does not create a historical reservation.
	_, err = tx.ExecContext(ctx, `INSERT INTO allocation_counters
		(namespace, repository_id, run_id, run_attempt, kind, last_sequence)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (namespace, repository_id, run_id, run_attempt, kind)
		DO UPDATE SET last_sequence = max(last_sequence, excluded.last_sequence)`,
		namespace, owner.RepositoryID, owner.RunID, owner.RunAttempt, kind, int64(floor))
	if err != nil {
		return "", 0, fmt.Errorf("set allocation floor: %w", err)
	}
	var current int64
	err = tx.QueryRowContext(ctx, `SELECT last_sequence FROM allocation_counters
		WHERE namespace = ? AND repository_id = ? AND run_id = ?
		AND run_attempt = ? AND kind = ?`, args...).Scan(&current)
	if err != nil {
		return "", 0, fmt.Errorf("read allocation counter: %w", err)
	}
	if current < 0 || current == math.MaxInt64 {
		return "", 0, fmt.Errorf("%w: resource sequence exhausted", ErrInvalid)
	}
	next := uint64(current) + 1
	id, err := formatPublicID(owner, next)
	if err != nil {
		return "", 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE allocation_counters SET last_sequence = ?
		WHERE namespace = ? AND repository_id = ? AND run_id = ?
		AND run_attempt = ? AND kind = ?`,
		int64(next), namespace, owner.RepositoryID, owner.RunID, owner.RunAttempt, kind)
	if err != nil {
		return "", 0, fmt.Errorf("advance allocation counter: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO allocation_reservations
		(namespace, repository_id, run_id, run_attempt, kind, sequence, resource_id, reserved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		namespace, owner.RepositoryID, owner.RunID, owner.RunAttempt, kind,
		int64(next), id, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		return "", 0, fmt.Errorf("record allocation reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("commit allocation reservation: %w", err)
	}
	return id, next, nil
}

var _ AllocationStore = (*sqliteAllocationStore)(nil)
