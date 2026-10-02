package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/migrations"
)

// The upgrade an operator watches has to show the schema moving: one
// migration_applied record per migration on the start that applies them, and
// none on the restart that finds nothing pending.
func TestMigrationsLogEachAppliedMigrationOnce(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var first bytes.Buffer
	if err := applyMigrations(ctx, logging.New(&first, logging.FormatJSON, logging.LevelInfo), db); err != nil {
		t.Fatalf("first start: %v", err)
	}

	want := countEmbeddedMigrations(t)
	records := decodeRecords(t, &first)
	if len(records) != want {
		t.Fatalf("first start logged %d records, want one migration_applied per migration (%d):\n%s",
			len(records), want, first.String())
	}
	previous := -1.0
	for i, rec := range records {
		if rec["event"] != "migration_applied" {
			t.Fatalf("record %d event = %v, want migration_applied: %s", i, rec["event"], first.String())
		}
		if rec["level"] != "info" {
			t.Errorf("record %d level = %v, want info", i, rec["level"])
		}
		name, _ := rec["name"].(string)
		if name == "" {
			t.Errorf("record %d has no name: %v", i, rec)
		}
		version, ok := rec["version"].(float64)
		if !ok {
			t.Fatalf("record %d has no numeric version: %v", i, rec)
		}
		// Migration order is the order they are applied in, so a reader
		// following the journal sees the schema move forwards.
		if version <= previous {
			t.Errorf("record %d version = %v, want it after %v", i, version, previous)
		}
		previous = version
	}

	var second bytes.Buffer
	if err := applyMigrations(ctx, logging.New(&second, logging.FormatJSON, logging.LevelInfo), db); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := decodeRecords(t, &second); len(got) != 0 {
		t.Fatalf("second start logged %d records, want none (nothing was pending):\n%s", len(got), second.String())
	}
}

// A failed upgrade must name the migration that failed, because "startup
// aborted" alone leaves the operator grepping SQL by hand.
func TestMigrationFailureIsLoggedWithVersionAndName(t *testing.T) {
	var buf bytes.Buffer
	logMigrationFailure(logging.New(&buf, logging.FormatJSON, logging.LevelInfo),
		&database.MigrationError{Version: 7, Name: "0007_broken.sql", Err: errors.New("no such table: nope")})

	records := decodeRecords(t, &buf)
	if len(records) != 1 {
		t.Fatalf("logged %d records, want exactly one:\n%s", len(records), buf.String())
	}
	rec := records[0]
	if rec["event"] != "migration_failed" {
		t.Errorf("event = %v, want migration_failed", rec["event"])
	}
	if rec["level"] != "error" {
		t.Errorf("level = %v, want error", rec["level"])
	}
	if rec["version"] != float64(7) || rec["name"] != "0007_broken.sql" {
		t.Errorf("record = %v, want version 7 and name 0007_broken.sql", rec)
	}
	if msg, _ := rec["error"].(string); !strings.Contains(msg, "0007_broken.sql") {
		t.Errorf("error field %q does not name the migration", msg)
	}
}

// A database a newer binary migrated is refused, and the record names both
// versions so the operator can tell a downgrade from a broken migration.
func TestSchemaTooNewIsLoggedWithBothVersions(t *testing.T) {
	var buf bytes.Buffer
	logMigrationFailure(logging.New(&buf, logging.FormatJSON, logging.LevelInfo),
		&database.SchemaTooNewError{Applied: 13, Known: 12})

	records := decodeRecords(t, &buf)
	if len(records) != 1 {
		t.Fatalf("logged %d records, want exactly one:\n%s", len(records), buf.String())
	}
	rec := records[0]
	if rec["event"] != "migration_failed" || rec["level"] != "error" {
		t.Errorf("record = %v, want an error-level migration_failed", rec)
	}
	if rec["applied"] != float64(13) || rec["known"] != float64(12) {
		t.Errorf("record = %v, want applied 13 and known 12", rec)
	}
	if msg, _ := rec["error"].(string); !strings.Contains(msg, "newer than this binary") {
		t.Errorf("error field %q does not explain the refusal", msg)
	}
}

// A failure that is not tied to one migration -- an unreadable embedded set,
// for example -- is still a failed upgrade and must still be reported.
func TestMigrationFailureWithoutAMigrationStillLogs(t *testing.T) {
	var buf bytes.Buffer
	logMigrationFailure(logging.New(&buf, logging.FormatJSON, logging.LevelInfo),
		errors.New("database: read embedded migrations: boom"))

	records := decodeRecords(t, &buf)
	if len(records) != 1 {
		t.Fatalf("logged %d records, want exactly one:\n%s", len(records), buf.String())
	}
	if records[0]["event"] != "migration_failed" {
		t.Errorf("event = %v, want migration_failed", records[0]["event"])
	}
	if msg, _ := records[0]["error"].(string); !strings.Contains(msg, "boom") {
		t.Errorf("error field = %q, want the underlying failure", msg)
	}
	if _, ok := records[0]["version"]; ok {
		t.Errorf("record invents a version for a failure that has none: %v", records[0])
	}
}

func countEmbeddedMigrations(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}

func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}
