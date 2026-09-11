// Package database owns the SQLite connection used by the Otter daemon.
//
// Otter deliberately depends on SQLite alone: there is no Postgres, Redis or
// message broker to operate. The connection is configured for durability
// (WAL journaling, NORMAL synchronous) and for single-writer safety.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	// Pure-Go SQLite driver: no cgo, so Otter cross-compiles to a static
	// binary for linux/amd64, linux/arm64 and darwin.
	_ "modernc.org/sqlite"
)

// FileName is the database file created inside the data directory.
const FileName = "otter.db"

// DB wraps *sql.DB with the on-disk path for diagnostics.
type DB struct {
	*sql.DB

	Path string
}

// Open creates the data directory if needed and opens the SQLite database in
// WAL mode.
func Open(ctx context.Context, dataDir string) (*DB, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("database: data directory must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("database: create data directory %s: %w", dataDir, err)
	}

	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("database: resolve data directory %s: %w", dataDir, err)
	}
	path := filepath.Join(abs, FileName)

	params := url.Values{}
	// _txlock=immediate makes every transaction take the write lock up front,
	// which removes the read-then-write upgrade deadlock that SQLite would
	// otherwise be able to produce between concurrent claims.
	params.Set("_txlock", "immediate")
	params.Add("_pragma", "busy_timeout(10000)")
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "synchronous(NORMAL)")
	params.Add("_pragma", "foreign_keys(1)")

	dsn := "file:" + path + "?" + params.Encode()

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("database: open %s: %w", path, err)
	}

	// A single connection removes write contention entirely and is plenty for
	// a single-machine runtime. Reads are served from the same connection.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("database: ping %s: %w", path, err)
	}

	return &DB{DB: sqlDB, Path: path}, nil
}

// Close checkpoints the WAL and closes the connection.
func (db *DB) Close() error {
	if db == nil || db.DB == nil {
		return nil
	}
	// Best effort: fold the WAL back into the main database file so that a
	// plain file copy is a valid backup.
	_, _ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return db.DB.Close()
}

// Tx runs fn inside a transaction. It rolls back on error, and also on a
// panic: because the pool is limited to a single connection, leaking a
// transaction would deadlock every later query.
func (db *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Safe after Commit: Rollback returns ErrTxDone, which we ignore.
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
