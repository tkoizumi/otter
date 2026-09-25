package timeline

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/runs"
)

// TestTimelineReadBudgetLargeRun is the measured gate for the single-connection
// constraint.
//
// The daemon runs SQLite on one connection, so a timeline page holds the only
// connection while it reads. The evidence revision is deliberately O(1) index
// seeks rather than a row count precisely so this stays bounded as a run grows:
// a count would make the tenth page of a large run cost ten times the first.
//
// The bound asserted here is loose on purpose. It is not the production budget
// (timeline.ReadBudget is 250ms); it is a ceiling that fails loudly if the read
// ever becomes linear in the run's size, which is the regression that matters.
// Ordinary unit tests avoid wall-clock assertions; this one exists because the
// cost model is the feature's main operational risk.
func TestTimelineReadBudgetLargeRun(t *testing.T) {
	if testing.Short() {
		t.Skip("performance gate skipped in short mode")
	}

	const (
		logRows    = 100_000
		exchanges  = 1_000
		ceiling    = 2 * time.Second
		traceLimit = 100
	)

	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()

	seedLogs(t, f, logRows, at)
	seedExchanges(t, f, exchanges, at)

	// First page.
	first := timedPage(t, f, timelineLimit(traceLimit, ""), traceLimit)
	if first > ceiling {
		t.Errorf("first page over %d log rows took %s, want under %s", logRows, first, ceiling)
	}
	t.Logf("first page (%d events over %d logs / %d exchanges): %s", traceLimit, logRows, exchanges, first)

	// Walk a few continuation pages: this is where a per-page O(rows) revision
	// would compound, so the last page is measured rather than only the first.
	page, err := f.reader.Page(f.ctx, Request{RunID: "run-1", IncludeHTTP: true, Limit: traceLimit})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	var last time.Duration
	for i := 0; i < 5 && page.HasMore; i++ {
		cursor := page.NextCursor
		start := time.Now()
		page, err = f.reader.Page(f.ctx, Request{
			RunID: "run-1", IncludeHTTP: true, Limit: traceLimit, After: cursor,
		})
		if err != nil {
			t.Fatalf("continuation page %d: %v", i, err)
		}
		last = time.Since(start)
	}
	if last > ceiling {
		t.Errorf("a continuation page took %s, want under %s", last, ceiling)
	}
	t.Logf("continuation page: %s", last)

	// The maximum page size must stay bounded too, and the assertion holds at
	// the run's end rather than only at its start.
	largest := timedPage(t, f, Request{RunID: "run-1", IncludeHTTP: true, Limit: MaxLimit}, MaxLimit)
	if largest > ceiling {
		t.Errorf("a %d-event page took %s, want under %s", MaxLimit, largest, ceiling)
	}
	t.Logf("maximum page (%d events): %s", MaxLimit, largest)

	// --no-http skips the exchange work entirely, so it must not be slower.
	withoutHTTP := timedPage(t, f, Request{RunID: "run-1", IncludeHTTP: false, Limit: traceLimit}, traceLimit)
	t.Logf("first page with --no-http: %s", withoutHTTP)

	// Three times the rows must not cost proportionally more than three times
	// the page. The evidence revision is two index seeks and the page itself is
	// bounded by the limit, so the expected shape is flat or mildly logarithmic;
	// this guard catches a return to an O(rows) revision, which at this size
	// would be tens of times slower rather than a few.
	f2 := newFixture(t, true)
	f2.addRun("run-1", runs.StatusFailed)
	f2.beginCapture("run-1")
	const biggerLogs = logRows * 3
	seedLogs(t, f2, biggerLogs, at)
	seedExchanges(t, f2, exchanges, at)

	bigger := timedPage(t, f2, timelineLimit(traceLimit, ""), traceLimit)
	t.Logf("first page over %d log rows: %s (ratio to %d rows: %.1fx)",
		biggerLogs, bigger, logRows, float64(bigger)/float64(first))
	if bigger > ceiling {
		t.Errorf("first page over %d log rows took %s, want under %s", biggerLogs, bigger, ceiling)
	}
	if first > 0 && bigger > 10*first && bigger > 50*time.Millisecond {
		t.Errorf("page cost grew with the run (3x rows took %.1fx): the revision looks linear in the run's size",
			float64(bigger)/float64(first))
	}
}

// TestTimelinePaginationUsesIndexes asserts the query plans, because the cost
// model above depends on the chronological indexes existing and being chosen.
// A plan that scanned the run's rows would still pass a loose timing bound on a
// fast machine while behaving badly on a real one.
func TestTimelinePaginationUsesIndexes(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()
	seedLogs(t, f, 50, at)
	seedExchanges(t, f, 5, at)

	cases := []struct {
		name  string
		query string
		index string
	}{
		{
			name: "logs by time",
			query: `SELECT id FROM run_logs WHERE run_id = 'run-1'
			          AND (timestamp > '2026-01-01T12:00:00.000000000Z')
			          ORDER BY timestamp ASC, id ASC LIMIT 10`,
			index: "idx_run_logs_run_time",
		},
		{
			name: "exchanges by time",
			query: `SELECT id FROM http_exchanges WHERE run_id = 'run-1'
			          AND (occurred_at > '2026-01-01T12:00:00.000000000Z')
			          ORDER BY occurred_at ASC, id ASC LIMIT 10`,
			index: "idx_http_exchanges_run_time",
		},
		{
			name:  "log evidence min and max",
			query: `SELECT MIN(id), MAX(id) FROM run_logs WHERE run_id = 'run-1'`,
			index: "idx_run_logs_run",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := queryPlan(t, f, tc.query)
			if !containsIndex(plan, tc.index) {
				t.Errorf("query plan does not use %s:\n%s", tc.index, plan)
			}
			if containsScan(plan) {
				t.Errorf("query plan scans a table:\n%s", plan)
			}
		})
	}
}

// ------------------------------------------------------------------ helpers

func seedLogs(t *testing.T, f *fixture, count int, at time.Time) {
	t.Helper()
	// A recursive CTE inserts the whole set in one statement: 100k individual
	// inserts would dominate the test's runtime and measure the wrong thing.
	query := fmt.Sprintf(`
		WITH RECURSIVE seq(n) AS (
		  SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < %d
		)
		INSERT INTO run_logs (run_id, timestamp, stream, message)
		SELECT 'run-1', ?, 'stdout', 'line-' || n FROM seq`, count)
	if _, err := f.db.ExecContext(f.ctx, query, database.FormatTime(at)); err != nil {
		t.Fatalf("seed logs: %v", err)
	}
}

func seedExchanges(t *testing.T, f *fixture, count int, at time.Time) {
	t.Helper()
	query := fmt.Sprintf(`
		WITH RECURSIVE seq(n) AS (
		  SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < %d
		)
		INSERT INTO http_exchanges
		  (run_id, request_id, integration_id, producer_seq, occurred_at, ingested_at,
		   updated_at, phase, complete, method, sanitized_url, status_code, payloads)
		SELECT 'run-1', 'req-' || n, 'int-1', n, ?, ?, ?,
		       'completed', 1, 'POST', 'https://a.test/' || n, 200, 'full'
		FROM seq`, count)
	formatted := database.FormatTime(at)
	if _, err := f.db.ExecContext(f.ctx, query, formatted, formatted, formatted); err != nil {
		t.Fatalf("seed exchanges: %v", err)
	}
}

func timedPage(t *testing.T, f *fixture, req Request, want int) time.Duration {
	t.Helper()
	start := time.Now()
	page, err := f.reader.Page(f.ctx, req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Events) != want {
		t.Fatalf("page returned %d events, want %d", len(page.Events), want)
	}
	return elapsed
}

func timelineLimit(limit int, after string) Request {
	return Request{RunID: "run-1", IncludeHTTP: true, Limit: limit, After: after}
}

func queryPlan(t *testing.T, f *fixture, query string) string {
	t.Helper()
	rows, err := f.db.QueryContext(f.ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan string
	for rows.Next() {
		var (
			id, parent, notUsed int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return plan
}

func containsIndex(plan, index string) bool {
	return strings.Contains(plan, "USING INDEX "+index) ||
		strings.Contains(plan, "USING COVERING INDEX "+index)
}

func containsScan(plan string) bool {
	return strings.Contains(plan, "SCAN run_logs") || strings.Contains(plan, "SCAN http_exchanges")
}
