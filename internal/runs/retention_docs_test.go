package runs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDocumentedRetentionSQLExecutes runs the manual-pruning SQL printed in
// operations.md against a freshly migrated database. The documented SQL was
// wrong once -- it filtered on runs.queued_at, a column that does not exist --
// and this test is what keeps the document executable.
func TestDocumentedRetentionSQLExecutes(t *testing.T) {
	const heading = "## Log rotation and run-log retention"

	path := filepath.Join("..", "..", "docs", "operations.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	document := string(body)

	// The defect OT-001 fixed was a filter on a column that never existed.
	if strings.Contains(document, "queued_at") {
		t.Error("operations.md still mentions queued_at; runs stores created_at")
	}

	blocks := sqlBlocksInSection(t, document, heading)
	if len(blocks) < 3 {
		t.Fatalf("found %d sql blocks under %q, want at least 3", len(blocks), heading)
	}

	for i, block := range blocks {
		db, store, logs := newRetentionStore(t)
		ctx := context.Background()
		now := time.Now().UTC()

		// An expired run and a recent one, so each statement has rows to
		// consider and cannot pass by operating on an empty table.
		seedRun(t, store, logs, "expired", StatusSucceeded, now.Add(-20*24*time.Hour), nil)
		seedRun(t, store, logs, "recent", StatusSucceeded, now.Add(-5*24*time.Hour), nil)

		if _, err := db.ExecContext(ctx, block); err != nil {
			t.Errorf("documented SQL block %d failed to execute: %v\n%s", i, err, block)
			continue
		}

		// The per-run log delete is the one block that must actually remove
		// the expired run's output while keeping the recent run's.
		if strings.Contains(block, "DELETE FROM run_logs") && strings.Contains(block, "created_at") {
			if n := logCount(t, logs, "expired"); n != 0 {
				t.Errorf("documented 14-day log delete left %d rows for the expired run", n)
			}
			if n := logCount(t, logs, "recent"); n != 1 {
				t.Errorf("documented 14-day log delete removed the recent run's output (%d rows left)", n)
			}
		}
	}
}

// sqlBlocksInSection returns every ```sql fenced block in the section that
// starts at heading, up to the next level-two heading.
func sqlBlocksInSection(t *testing.T, document, heading string) []string {
	t.Helper()

	start := strings.Index(document, heading)
	if start < 0 {
		t.Fatalf("operations.md has no %q heading", heading)
	}
	rest := document[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}

	var (
		blocks []string
		open   bool
		lines  []string
	)
	for _, line := range strings.Split(rest, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case !open && trimmed == "```sql":
			open = true
			lines = nil
		case open && trimmed == "```":
			open = false
			blocks = append(blocks, strings.Join(lines, "\n"))
		case open:
			lines = append(lines, line)
		}
	}
	if open {
		t.Fatalf("unterminated ```sql block under %q", heading)
	}
	return blocks
}
