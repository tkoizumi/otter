package database

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/otter/migrations"
)

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at DATETIME NOT NULL
);`

// AppliedMigration names one migration a Migrate call applied. It is what the
// daemon turns into a migration_applied log record, so an operator watching an
// upgrade sees the schema move instead of a silent restart.
//
// Name is the migration's filename, which is also its description: the schema
// stores no separate blurb, so the document and the log record agree by
// construction.
type AppliedMigration struct {
	Version int
	Name    string
}

// MigrationError reports which migration failed. A caller that logs the
// failure -- otterd does, as migration_failed -- reads the version and name
// from here, because the wrapped error text is not something to parse.
type MigrationError struct {
	Version int
	Name    string
	Err     error
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("database: apply migration %s: %v", e.Name, e.Err)
}

func (e *MigrationError) Unwrap() error { return e.Err }

// Migrate applies every embedded migration that has not been applied yet, in
// lexicographic filename order, and reports what it applied. Each migration
// runs inside its own transaction so a failure leaves the database at a known
// version.
//
// The returned slice is empty when nothing was pending -- the common case for a
// restart -- so a caller that logs one record per entry reports an upgrade and
// stays quiet otherwise.
func Migrate(ctx context.Context, db *DB) ([]AppliedMigration, error) {
	return migrate(ctx, db, migrations.FS)
}

// migrate is Migrate against an explicit filesystem, so a test can present a
// broken migration without one existing in the embedded set.
func migrate(ctx context.Context, db *DB, fsys fs.FS) ([]AppliedMigration, error) {
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("database: create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return nil, err
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("database: read embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var out []AppliedMigration
	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			return out, err
		}
		if applied[version] {
			continue
		}

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return out, &MigrationError{
				Version: version,
				Name:    name,
				Err:     fmt.Errorf("read: %w", err),
			}
		}
		if err := applyMigration(ctx, db, version, name, string(body)); err != nil {
			return out, err
		}
		out = append(out, AppliedMigration{Version: version, Name: name})
	}

	return out, nil
}

func applyMigration(ctx context.Context, db *DB, version int, name, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return &MigrationError{Version: version, Name: name, Err: fmt.Errorf("begin: %w", err)}
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return &MigrationError{Version: version, Name: name, Err: err}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		version, name, FormatTime(time.Now()),
	); err != nil {
		return &MigrationError{Version: version, Name: name, Err: fmt.Errorf("record: %w", err)}
	}
	if err := tx.Commit(); err != nil {
		return &MigrationError{Version: version, Name: name, Err: fmt.Errorf("commit: %w", err)}
	}
	return nil
}

func appliedVersions(ctx context.Context, db *DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("database: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("database: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// parseVersion extracts the leading numeric version from a migration filename
// such as 0001_init.sql.
func parseVersion(filename string) (int, error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, _, _ := strings.Cut(base, "_")
	v, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("database: migration %q must start with a numeric version, e.g. 0001_init.sql", filename)
	}
	return v, nil
}
