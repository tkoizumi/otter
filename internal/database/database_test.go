package database

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/otter-runtime/otter/migrations"
)

func openTempDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func migrateTempDB(t *testing.T) *DB {
	t.Helper()
	db := openTempDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func tableExists(t *testing.T, db *DB, name string) bool {
	t.Helper()
	var found string
	err := db.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("query sqlite_master for %s: %v", name, err)
	}
	return found == name
}

type embeddedMigration struct {
	version int
	name    string
}

func embeddedMigrations(t *testing.T) []embeddedMigration {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var out []embeddedMigration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := parseVersion(e.Name())
		if err != nil {
			t.Fatalf("parseVersion(%q): %v", e.Name(), err)
		}
		out = append(out, embeddedMigration{version: version, name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}

func TestOpenCreatesDirectoryAndFile(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "does", "not", "exist", "yet")

	db, err := Open(context.Background(), nested)
	if err != nil {
		t.Fatalf("Open on a nested, non-existent directory: %v", err)
	}
	defer db.Close()

	if !filepath.IsAbs(db.Path) {
		t.Fatalf("db.Path = %q, want an absolute path", db.Path)
	}
	if db.Path != filepath.Join(nested, FileName) {
		t.Fatalf("db.Path = %q, want %q", db.Path, filepath.Join(nested, FileName))
	}

	info, err := os.Stat(db.Path)
	if err != nil {
		t.Fatalf("database file not created: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("db.Path is a directory, want a file")
	}

	dirInfo, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("data directory not created: %v", err)
	}
	if !dirInfo.IsDir() {
		t.Fatalf("data directory is not a directory")
	}
	// The daemon keeps credentials out of other users' reach.
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("data directory permissions = %o, want owner-only", perm)
	}

	// The connection must actually be usable.
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatalf("Open(\"\") should fail")
	}
}

func TestMigrateIsIdempotentAndRecordsEveryMigration(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate should be a no-op: %v", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()

	var got []embeddedMigration
	for rows.Next() {
		var m embeddedMigration
		if err := rows.Scan(&m.version, &m.name); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		got = append(got, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}

	want := embeddedMigrations(t)
	if len(want) == 0 {
		t.Fatalf("no embedded migrations found")
	}
	if len(got) != len(want) {
		t.Fatalf("schema_migrations has %d rows, want %d (one per embedded migration): %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMigrateCreatesExpectedTables(t *testing.T) {
	db := migrateTempDB(t)

	for _, name := range []string{
		"integration_state",
		"runs",
		"run_logs",
		"run_queue",
		"webhook_tokens",
		"schema_migrations",
	} {
		if !tableExists(t, db, name) {
			t.Fatalf("table %q does not exist after Migrate", name)
		}
	}
}

func TestJournalModeIsWAL(t *testing.T) {
	db := migrateTempDB(t)

	var mode string
	if err := db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestTxCommitsAndRollsBack(t *testing.T) {
	db := migrateTempDB(t)
	ctx := context.Background()

	insert := func(tx *sql.Tx, key, value string) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO integration_state (integration_id, key, value, updated_at) VALUES (?, ?, ?, ?)`,
			"int-A", key, value, FormatTime(time.Now()))
		return err
	}

	count := func(key string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM integration_state WHERE integration_id = ? AND key = ?`,
			"int-A", key).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", key, err)
		}
		return n
	}

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		return insert(tx, "committed", `"yes"`)
	}); err != nil {
		t.Fatalf("committing Tx: %v", err)
	}
	if count("committed") != 1 {
		t.Fatalf("committed row is missing")
	}

	sentinel := errors.New("force rollback")
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if err := insert(tx, "rolledback", `"no"`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx error = %v, want the callback error", err)
	}
	if count("rolledback") != 0 {
		t.Fatalf("rolled-back row is still present")
	}
}

func TestTimeFormatRoundTripAndOrdering(t *testing.T) {
	base := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

	formatted := FormatTime(base)
	if formatted != "2024-05-06T07:08:09.000000000Z" {
		t.Fatalf("FormatTime = %q, want the fixed-width UTC rendering", formatted)
	}
	if len(formatted) != 30 {
		t.Fatalf("FormatTime length = %d, want 30 for a fixed-width value", len(formatted))
	}

	later := base.Add(500 * time.Millisecond)
	if !(FormatTime(base) < FormatTime(later)) {
		t.Fatalf("lexicographic order does not match chronological order: %q !< %q",
			FormatTime(base), FormatTime(later))
	}

	parsed, err := ParseTime(formatted)
	if err != nil {
		t.Fatalf("ParseTime: %v", err)
	}
	if !parsed.Equal(base) {
		t.Fatalf("ParseTime = %v, want %v", parsed, base)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("ParseTime location = %v, want UTC", parsed.Location())
	}

	// A non-UTC instant must round-trip to the same instant, rendered in UTC.
	zone := time.FixedZone("X", 3600)
	inZone := time.Date(2024, 5, 6, 8, 8, 9, 0, zone)
	parsedZone, err := ParseTime(FormatTime(inZone))
	if err != nil {
		t.Fatalf("ParseTime(non-UTC): %v", err)
	}
	if !parsedZone.Equal(inZone) {
		t.Fatalf("ParseTime = %v, want the same instant as %v", parsedZone, inZone)
	}
	if parsedZone.Location() != time.UTC {
		t.Fatalf("ParseTime location = %v, want UTC", parsedZone.Location())
	}

	if _, err := ParseTime("not-a-time"); err == nil {
		t.Fatalf("ParseTime(\"not-a-time\") should fail")
	}
}

func TestNullableTimeScan(t *testing.T) {
	value := time.Date(2024, 3, 4, 5, 6, 7, 800000000, time.UTC)

	cases := []struct {
		name      string
		src       any
		wantValid bool
		want      time.Time
		wantErr   bool
	}{
		{"nil", nil, false, time.Time{}, false},
		{"time", value, true, value, false},
		{"string", FormatTime(value), true, value, false},
		{"bytes", []byte(FormatTime(value)), true, value, false},
		{"unsupported int", 42, false, time.Time{}, true},
		{"malformed string", "nope", false, time.Time{}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got NullableTime
			err := got.Scan(tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Scan(%v) should fail", tc.src)
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan(%v): %v", tc.src, err)
			}
			if got.Valid != tc.wantValid {
				t.Fatalf("Valid = %v, want %v", got.Valid, tc.wantValid)
			}
			if tc.wantValid && !got.Time.Equal(tc.want) {
				t.Fatalf("Time = %v, want %v", got.Time, tc.want)
			}
			if ptr := got.Ptr(); tc.wantValid {
				if ptr == nil || !ptr.Equal(tc.want) {
					t.Fatalf("Ptr() = %v, want %v", ptr, tc.want)
				}
			} else if ptr != nil {
				t.Fatalf("Ptr() = %v, want nil for an invalid NullableTime", ptr)
			}
		})
	}
}

func TestFormatNullableAndNullableInt(t *testing.T) {
	if got := FormatNullable(nil); got != nil {
		t.Fatalf("FormatNullable(nil) = %v, want nil", got)
	}

	zero := time.Time{}
	if got := FormatNullable(&zero); got != nil {
		t.Fatalf("FormatNullable(zero) = %v, want nil", got)
	}

	value := time.Date(2024, 7, 8, 9, 10, 11, 0, time.UTC)
	if got := FormatNullable(&value); got != FormatTime(value) {
		t.Fatalf("FormatNullable = %v, want %q", got, FormatTime(value))
	}

	if got := NullableInt(nil); got != nil {
		t.Fatalf("NullableInt(nil) = %v, want nil", got)
	}
	n := 7
	if got := NullableInt(&n); got != 7 {
		t.Fatalf("NullableInt = %v, want 7", got)
	}
}

func TestMigrateSkipsAlreadyAppliedMigration(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	all := embeddedMigrations(t)
	if len(all) == 0 {
		t.Fatalf("no embedded migrations found")
	}
	first := all[0]

	// Simulate a database that already records the first migration as applied
	// without its schema having been (re)created in this process.
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		first.version, first.name, FormatTime(time.Now())); err != nil {
		t.Fatalf("record migration: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate with an already-applied migration: %v", err)
	}
	if tableExists(t, db, "runs") {
		t.Fatalf("migration %s was re-run even though schema_migrations lists it", first.name)
	}

	// Forgetting the record makes the migration run for real; the resulting
	// schema must be usable.
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, first.version); err != nil {
		t.Fatalf("forget migration: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate after forgetting the record: %v", err)
	}
	if !tableExists(t, db, "runs") {
		t.Fatalf("runs table was not created by the migration")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO integration_state (integration_id, key, value, updated_at) VALUES (?, ?, ?, ?)`,
		"int-A", "k", `1`, FormatTime(time.Now())); err != nil {
		t.Fatalf("migrated schema is not usable: %v", err)
	}
}
