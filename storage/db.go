// Package storage provides an embedded SQLite-backed store for denyzer's query
// logs and dashboard metrics.
//
// The database keeps exactly two business tables: query_logs for the detail
// view and metrics for every aggregated counter, including the trailing window
// rollups. The SQLite amalgamation is compiled into the binary by the CGO
// driver, so no external database service or system library is required.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver
)

// Table names. The dashboard deliberately keeps only two business tables:
// query_logs for the detail view and metrics for every aggregated counter.
const (
	TableQueryLogs = "query_logs"
	TableMetrics   = "metrics"
)

const (
	// dbFileName is used when the configured target is a directory, so the
	// same Docker volume (or systemd StateDirectory) can hold the database
	// without the operator having to name the file.
	dbFileName = "denyzer.db"

	// schemaVersion is stored in PRAGMA user_version. A database created by a
	// newer version of denyzer is refused instead of being silently used.
	schemaVersion = 1

	// writeConnections bounds the pool. SQLite allows one writer at a time and
	// any number of readers under WAL, so a small pool is enough.
	writeConnections = 8

	// dirPerm is the mode of the directories the store creates.
	dirPerm = 0o750
)

//nolint:gochecknoglobals // process-wide database handle, initialised once
var (
	initMu sync.Mutex
	dbPtr  atomic.Pointer[sql.DB]
)

// Schema. The metrics primary key is ordered granularity → dimension →
// bucket_start → entity, which makes every hot read a contiguous primary-key
// range scan:
//
//   - series:      granularity + dimension + bucket range
//   - top lists:   granularity + dimension (window rollups only have bucket 0)
//   - retention:   granularity + bucket range
//
// Because the table is WITHOUT ROWID, scanning that range reads the full rows
// from the B-tree without a second lookup.
//
//nolint:gochecknoglobals // immutable schema definition
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS query_logs (
		id            INTEGER PRIMARY KEY,
		timestamp     INTEGER NOT NULL,
		client_ip     TEXT    NOT NULL,
		client_name   TEXT    NOT NULL,
		domain        TEXT    NOT NULL,
		query_type    TEXT    NOT NULL,
		response_type TEXT    NOT NULL,
		reason        TEXT    NOT NULL,
		duration_ms   INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_query_logs_timestamp ON query_logs (timestamp)`,
	`CREATE TABLE IF NOT EXISTS metrics (
		granularity      TEXT    NOT NULL,
		dimension        TEXT    NOT NULL,
		bucket_start     INTEGER NOT NULL,
		entity           TEXT    NOT NULL,
		entity_label     TEXT    NOT NULL DEFAULT '',
		total            INTEGER NOT NULL DEFAULT 0,
		blocked          INTEGER NOT NULL DEFAULT 0,
		cached           INTEGER NOT NULL DEFAULT 0,
		forwarded        INTEGER NOT NULL DEFAULT 0,
		filtered         INTEGER NOT NULL DEFAULT 0,
		local            INTEGER NOT NULL DEFAULT 0,
		other            INTEGER NOT NULL DEFAULT 0,
		dropped          INTEGER NOT NULL DEFAULT 0,
		errors           INTEGER NOT NULL DEFAULT 0,
		total_latency_ms INTEGER NOT NULL DEFAULT 0,
		updated_at       INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (granularity, dimension, bucket_start, entity)
	) WITHOUT ROWID`,
}

// InitDB opens (or reuses) the local SQLite database and creates the schema if
// it does not exist yet. It is idempotent: the database is opened at most once
// per process.
//
// dbPath may point at a file or at a directory. A directory (including a path
// with a trailing separator) stores the database as <dir>/denyzer.db, which
// lets the Docker volume and the systemd state directory keep working without
// naming the file.
func InitDB(dbPath string) error {
	if dbPath == "" {
		return errors.New("SQLite query log requires a target path")
	}

	initMu.Lock()
	defer initMu.Unlock()

	if dbPtr.Load() != nil {
		return nil
	}

	path, err := resolveDBPath(dbPath)
	if err != nil {
		return err
	}

	conn, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return fmt.Errorf("can't open SQLite database: %w", err)
	}

	conn.SetMaxOpenConns(writeConnections)
	conn.SetMaxIdleConns(writeConnections)
	conn.SetConnMaxIdleTime(0)

	if err := conn.PingContext(context.Background()); err != nil {
		_ = conn.Close()

		return fmt.Errorf("can't open SQLite database %s: %w", path, err)
	}

	if err := ensureSchema(conn); err != nil {
		_ = conn.Close()

		return err
	}

	dbPtr.Store(conn)

	return nil
}

// CloseDB closes the database. The next InitDB call opens it again; tests use
// this to simulate a process restart.
func CloseDB() error {
	initMu.Lock()
	defer initMu.Unlock()

	conn := dbPtr.Swap(nil)
	if conn == nil {
		return nil
	}

	return conn.Close()
}

// getDB returns the open database handle.
func getDB() (*sql.DB, error) {
	if conn := dbPtr.Load(); conn != nil {
		return conn, nil
	}

	return nil, errors.New("storage is not initialised")
}

// resolveDBPath maps the configured target to the SQLite file path.
func resolveDBPath(dbPath string) (string, error) {
	info, err := os.Stat(dbPath)
	switch {
	case err == nil && info.IsDir():
		return filepath.Join(dbPath, dbFileName), nil
	case err == nil:
		return dbPath, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("can't stat SQLite target %s: %w", dbPath, err)
	}

	if strings.HasSuffix(dbPath, string(os.PathSeparator)) {
		if err := os.MkdirAll(dbPath, dirPerm); err != nil {
			return "", fmt.Errorf("can't create SQLite directory %s: %w", dbPath, err)
		}

		return filepath.Join(dbPath, dbFileName), nil
	}

	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("can't create SQLite directory %s: %w", dir, err)
	}

	return dbPath, nil
}

// dsn builds the driver connection string. The pragmas are applied to every
// pooled connection, which is what makes them effective.
func dsn(path string) string {
	params := []string{
		"_busy_timeout=5000",
		"_journal_mode=WAL",
		"_synchronous=NORMAL",
		"_txlock=immediate",
		"_foreign_keys=off",
		"_cache_size=-65536",   // 64 MiB page cache
		"_mmap_size=268435456", // 256 MiB memory map
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	return abs + "?" + strings.Join(params, "&")
}

// ensureSchema creates the tables and validates the schema version.
func ensureSchema(conn *sql.DB) error {
	ctx := context.Background()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("can't read SQLite schema version: %w", err)
	}

	if version > schemaVersion {
		return fmt.Errorf("SQLite schema version %d is newer than this binary supports (%d)",
			version, schemaVersion)
	}

	for _, statement := range schemaStatements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("can't create SQLite schema: %w", err)
		}
	}

	if version < schemaVersion {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("can't set SQLite schema version: %w", err)
		}
	}

	return nil
}

// Batch is a transaction that spans multiple tables. Callers stage all writes
// for one flush cycle into it and call CommitBatch once, making the entire
// cycle atomic. An uncommitted SQLite transaction keeps the write lock, so
// every error path must call RollbackBatch.
type Batch struct {
	tx *sql.Tx
}

// NewBatch starts a write transaction.
func NewBatch() (Batch, error) {
	conn, err := getDB()
	if err != nil {
		return Batch{}, err
	}

	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return Batch{}, fmt.Errorf("can't begin SQLite transaction: %w", err)
	}

	return Batch{tx: tx}, nil
}

// CommitBatch atomically applies every operation staged in batch.
func CommitBatch(batch Batch) error {
	if batch.tx == nil {
		return nil
	}

	if err := batch.tx.Commit(); err != nil {
		return fmt.Errorf("can't commit SQLite transaction: %w", err)
	}

	return nil
}

// RollbackBatch discards every operation staged in batch. It is safe to call
// after CommitBatch: the transaction is already finished and the error is
// ignored.
func RollbackBatch(batch Batch) {
	if batch.tx != nil {
		_ = batch.tx.Rollback()
	}
}

// withTx runs fn inside a write transaction, committing on success and rolling
// back on failure.
func withTx(fn func(*sql.Tx) error) error {
	batch, err := NewBatch()
	if err != nil {
		return err
	}

	if err := fn(batch.tx); err != nil {
		RollbackBatch(batch)

		return err
	}

	return CommitBatch(batch)
}

// updatedAt returns the row modification timestamp in Unix seconds.
func updatedAt() int64 {
	return time.Now().Unix()
}
