package database

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/otter-runtime/otter/migrations"
)

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at DATETIME NOT NULL
);`

// Migrate applies every embedded migration that has not been applied yet, in
// lexicographic filename order. Each migration runs inside its own
// transaction so a failure leaves the database at a known version.
func Migrate(ctx context.Context, db *DB) error {
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("database: create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("database: read embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			return err
		}
		if applied[version] {
			continue
		}

		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("database: read migration %s: %w", name, err)
		}
		if err := applyMigration(ctx, db, version, name, string(body)); err != nil {
			return err
		}
	}

	return nil
}

func applyMigration(ctx context.Context, db *DB, version int, name, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("database: apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		version, name, FormatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("database: record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit migration %s: %w", name, err)
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
