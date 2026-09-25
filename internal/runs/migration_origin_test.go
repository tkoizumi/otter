package runs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// legacyOriginRows is the set the backfill has to get right: the daemon's real
// narration shapes, and the lines an integration can plausibly write on the same
// stream.
var legacyOriginRows = []struct {
	stream, message string
	daemon          bool
}{
	{StreamOtter, "run queued (trigger manual)", true},
	{StreamOtter, "run started (attempt 1 of 3, trigger cron)", true},
	{StreamOtter, "run failed (attempt 1, 41ms), exit code 1: process exited with code 1", true},
	{StreamOtter, "run succeeded (attempt 1, 12ms)", true},
	{StreamOtter, "run timed_out (attempt 1, 60s): timed out after 60s", true},
	{StreamOtter, "run cancelled before execution", true},
	{StreamOtter, "run cancelled: cancelled by operator before execution", true},
	{StreamOtter, "retry 2 of 3 scheduled in 2s as run abc", true},
	{StreamOtter, "not started: missing secret API_KEY", true},
	{StreamOtter, "marked failed: otter daemon restarted during execution", true},

	{StreamOtter, `sync starting {"dry_run":false,"level":"info"}`, false},
	{StreamOtter, "run failed because the token expired", false},
	{StreamStdout, "plain output", false},
	{StreamStderr, "Traceback (most recent call last):", false},
}

// TestOriginBackfillClassifiesLegacyRows checks the migration's backfill, which
// cannot be exercised by writing new rows: the statement is extracted from the
// migration file so the test fails if the migration and the Go rule drift apart.
func TestOriginBackfillClassifiesLegacyRows(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Legacy rows have no origin, exactly as they were written before the column.
	for _, row := range legacyOriginRows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO run_logs (run_id, timestamp, stream, message, origin)
			 VALUES ('run-1', ?, ?, ?, '')`,
			database.FormatTime(time.Now().UTC()), row.stream, row.message); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}

	for _, statement := range backfillStatements(t) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("run backfill: %v", err)
		}
	}

	entries, err := NewLogStore(db.DB).List(ctx, "run-1", 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != len(legacyOriginRows) {
		t.Fatalf("read %d rows, want %d", len(entries), len(legacyOriginRows))
	}
	for i, entry := range entries {
		want := legacyOriginRows[i].daemon
		if got := entry.IsLifecycle(); got != want {
			t.Errorf("row %q: IsLifecycle = %v, want %v (origin=%q)",
				legacyOriginRows[i].message, got, want, entry.Origin)
		}
		if entry.Origin == "" {
			t.Errorf("row %q was left without an origin", legacyOriginRows[i].message)
		}
	}
}

// backfillStatements extracts the origin UPDATEs from the migration, so the test
// exercises the shipped SQL rather than a copy of it. Both are needed: the first
// marks the daemon's narration, the second labels everything else as the
// integration's.
func backfillStatements(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile("../../migrations/0008_run_logs_origin.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(body)

	var statements []string
	for _, marker := range []string{
		"UPDATE run_logs SET origin = 'daemon'",
		"UPDATE run_logs SET origin = 'child'",
	} {
		start := strings.Index(sql, marker)
		if start < 0 {
			t.Fatalf("migration is missing the %q backfill", marker)
		}
		rest := sql[start:]
		// The daemon backfill ends with ");" and the child one with ";".
		end := strings.Index(rest, ");")
		trailer := 2
		if end < 0 {
			end = strings.Index(rest, ";")
			trailer = 1
		}
		if end < 0 {
			t.Fatalf("backfill %q is not terminated", marker)
		}
		statements = append(statements, rest[:end+trailer])
	}
	return statements
}
