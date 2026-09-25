package timeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
)

// ---------------------------------------------------------------- fixtures

type fixture struct {
	t          *testing.T
	ctx        context.Context
	db         *database.DB
	reader     *Reader
	runs       *runs.Store
	logs       *runs.LogStore
	inspection *inspection.Store
}

func newFixture(t *testing.T, withInspection bool) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}

	runsStore := runs.NewStore(db.DB)
	logsStore := runs.NewLogStore(db.DB)

	f := &fixture{t: t, ctx: ctx, db: db, runs: runsStore, logs: logsStore}
	if withInspection {
		f.inspection = inspection.NewStore(db.DB, inspection.DefaultRedactor(), inspection.DefaultLimits())
	}
	f.reader = NewReader(db, runsStore, logsStore, f.inspection)
	return f
}

// addRun writes a terminal run directly so the test controls its status.
func (f *fixture) addRun(runID string, status runs.Status) {
	f.t.Helper()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	run := &runs.Run{
		ID:            runID,
		IntegrationID: "int-1",
		TriggerType:   runs.TriggerManual,
		Status:        status,
		Attempt:       1,
		CreatedAt:     now,
	}
	if err := f.runs.Create(f.ctx, run); err != nil {
		f.t.Fatalf("create run %s: %v", runID, err)
	}
}

// addLog inserts one log row with an explicit timestamp and returns its id. The
// origin is left empty, which is what a row written before the origin column
// looked like.
func (f *fixture) addLog(runID string, at time.Time, stream, message string) int64 {
	return f.addLogOrigin(runID, at, stream, message, "")
}

// addLogOrigin inserts one log row with an explicit origin.
func (f *fixture) addLogOrigin(runID string, at time.Time, stream, message, origin string) int64 {
	f.t.Helper()
	res, err := f.db.ExecContext(f.ctx,
		`INSERT INTO run_logs (run_id, timestamp, stream, message, origin) VALUES (?, ?, ?, ?, ?)`,
		runID, database.FormatTime(at), stream, message, origin)
	if err != nil {
		f.t.Fatalf("insert log: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatalf("log last insert id: %v", err)
	}
	return id
}

// addExchange inserts one exchange with an explicit occurred_at.
func (f *fixture) addExchange(runID, requestID string, at time.Time, method, url string, status int) int64 {
	f.t.Helper()
	ingested := at
	res, err := f.db.ExecContext(f.ctx,
		`INSERT INTO http_exchanges
		   (run_id, request_id, integration_id, producer_seq, occurred_at, ingested_at,
		    updated_at, phase, complete, method, sanitized_url, status_code, payloads)
		 VALUES (?, ?, 'int-1', 1, ?, ?, ?, 'completed', 1, ?, ?, ?, 'full')`,
		runID, requestID, database.FormatTime(at), database.FormatTime(ingested),
		database.FormatTime(ingested), method, url, status)
	if err != nil {
		f.t.Fatalf("insert exchange: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatalf("exchange last insert id: %v", err)
	}
	return id
}

func (f *fixture) beginCapture(runID string) {
	f.t.Helper()
	if f.inspection == nil {
		return
	}
	if err := f.inspection.Begin(f.ctx, inspection.CaptureSettings{
		RunID:         runID,
		IntegrationID: "int-1",
		Policy:        inspection.PolicyFull,
	}); err != nil {
		f.t.Fatalf("begin capture: %v", err)
	}
}

func (f *fixture) page(runID string, limit int) *Page {
	f.t.Helper()
	p, err := f.reader.Page(f.ctx, Request{RunID: runID, IncludeHTTP: true, Limit: limit})
	if err != nil {
		f.t.Fatalf("Page(%s): %v", runID, err)
	}
	return p
}

func base() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }

// ------------------------------------------------------------------- tests

// TestMergeOrdersByTimeThenRank pins the total order the cursor depends on. A
// log and an exchange sharing a timestamp must always present in the same
// order, and neither source may be reordered by its own arrival sequence.
func TestMergeOrdersByTimeThenRank(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")

	at := base()
	// Insertion order deliberately disagrees with chronological order.
	f.addLog("run-1", at.Add(30*time.Millisecond), runs.StreamStdout, "third")
	f.addExchange("run-1", "req-late", at.Add(20*time.Millisecond), "POST", "https://a.test/late", 200)
	f.addLogOrigin("run-1", at.Add(10*time.Millisecond), runs.StreamOtter, "first", runs.OriginDaemon)
	// Equal timestamps: the log must come first (rank 0).
	f.addLogOrigin("run-1", at.Add(20*time.Millisecond), runs.StreamOtter, "log-at-20", runs.OriginDaemon)
	f.addExchange("run-1", "req-early", at.Add(10*time.Millisecond), "GET", "https://a.test/early", 200)

	page := f.page("run-1", 100)

	type want struct {
		kind string
		text string
	}
	wants := []want{
		{KindLifecycle, "first"},
		{KindHTTP, "early"},
		{KindLifecycle, "log-at-20"},
		{KindHTTP, "late"},
		{KindLog, "third"},
	}
	if len(page.Events) != len(wants) {
		t.Fatalf("events = %d, want %d: %+v", len(page.Events), len(wants), page.Events)
	}
	for i, w := range wants {
		event := page.Events[i]
		if event.Kind != w.kind {
			t.Errorf("event %d kind = %q, want %q", i, event.Kind, w.kind)
		}
		if w.kind == KindHTTP {
			if event.HTTP == nil || !strings.Contains(event.HTTP.URL, w.text) {
				t.Errorf("event %d = %+v, want the %s exchange", i, event.HTTP, w.text)
			}
			continue
		}
		if event.Message != w.text {
			t.Errorf("event %d message = %q, want %q", i, event.Message, w.text)
		}
	}
}

// TestPaginationHasNoGapsOrDuplicates walks every page size for a trace that
// mixes equal timestamps across both sources.
func TestPaginationHasNoGapsOrDuplicates(t *testing.T) {
	at := base()

	for _, limit := range []int{1, 2, 3, 5, 50} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			f := newFixture(t, true)
			f.addRun("run-1", runs.StatusFailed)
			f.beginCapture("run-1")

			// Nine events, several sharing a timestamp across sources.
			f.addLogOrigin("run-1", at, runs.StreamOtter, "l0", runs.OriginDaemon)
			f.addExchange("run-1", "r0", at, "GET", "https://a.test/0", 200)
			f.addLog("run-1", at, runs.StreamStdout, "l1")
			f.addLog("run-1", at.Add(time.Millisecond), runs.StreamStdout, "l2")
			f.addExchange("run-1", "r1", at.Add(time.Millisecond), "GET", "https://a.test/1", 200)
			f.addLog("run-1", at.Add(2*time.Millisecond), runs.StreamStderr, "l3")
			f.addExchange("run-1", "r2", at.Add(3*time.Millisecond), "POST", "https://a.test/2", 400)
			f.addLogOrigin("run-1", at.Add(4*time.Millisecond), runs.StreamOtter, "l4", runs.OriginDaemon)
			f.addExchange("run-1", "r3", at.Add(5*time.Millisecond), "PUT", "https://a.test/3", 500)

			var (
				seen   []string
				cursor string
				pages  int
			)
			for {
				page, err := f.reader.Page(f.ctx, Request{
					RunID: "run-1", IncludeHTTP: true, Limit: limit, After: cursor,
				})
				if err != nil {
					t.Fatalf("page %d: %v", pages, err)
				}
				pages++
				if pages > 20 {
					t.Fatalf("pagination did not terminate")
				}
				for _, event := range page.Events {
					seen = append(seen, key(event))
				}
				if !page.HasMore {
					if page.NextCursor != "" {
						t.Errorf("a final page must not carry a cursor")
					}
					break
				}
				if page.NextCursor == "" {
					t.Fatalf("has_more with no cursor")
				}
				cursor = page.NextCursor
			}

			// Equal timestamps place the log first, because logs are rank 0. The
			// sequence is therefore l0, l1, r0, l2, r1, ...
			want := []string{
				"lifecycle:l0", "log:l1", "http:r0", "log:l2", "http:r1",
				"log:l3", "http:r2", "lifecycle:l4", "http:r3",
			}
			if len(seen) != len(want) {
				t.Fatalf("got %v, want %v", seen, want)
			}
			for i := range want {
				if seen[i] != want[i] {
					t.Fatalf("got %v, want %v", seen, want)
				}
			}
		})
	}
}

// TestContinuationIsRefusedWhenEvidenceChanges covers the mutations the plan
// lists: an appended log, an updated exchange and deleted evidence.
func TestContinuationIsRefusedWhenEvidenceChanges(t *testing.T) {
	at := base()
	seed := func(f *fixture) {
		f.addRun("run-1", runs.StatusFailed)
		f.beginCapture("run-1")
		f.addLog("run-1", at, runs.StreamOtter, "a")
		f.addLog("run-1", at.Add(time.Millisecond), runs.StreamOtter, "b")
		f.addExchange("run-1", "r0", at.Add(2*time.Millisecond), "GET", "https://a.test/0", 200)
	}
	// One page of two events leaves a continuation outstanding.
	cursorAfterFirstPage := func(f *fixture) string {
		page := f.page("run-1", 2)
		if !page.HasMore || page.NextCursor == "" {
			t.Fatalf("expected a continuation, got %+v", page)
		}
		return page.NextCursor
	}

	t.Run("unchanged evidence continues", func(t *testing.T) {
		f := newFixture(t, true)
		seed(f)
		cursor := cursorAfterFirstPage(f)

		page, err := f.reader.Page(f.ctx, Request{
			RunID: "run-1", IncludeHTTP: true, Limit: 2, After: cursor,
		})
		if err != nil {
			t.Fatalf("continuation: %v", err)
		}
		if len(page.Events) == 0 {
			t.Fatalf("continuation returned no events")
		}
	})

	t.Run("appended log", func(t *testing.T) {
		f := newFixture(t, true)
		seed(f)
		cursor := cursorAfterFirstPage(f)
		f.addLog("run-1", at.Add(3*time.Millisecond), runs.StreamStdout, "late")

		assertEvidenceChanged(t, f, cursor)
	})

	t.Run("appended exchange", func(t *testing.T) {
		f := newFixture(t, true)
		seed(f)
		cursor := cursorAfterFirstPage(f)
		f.addExchange("run-1", "r1", at.Add(3*time.Millisecond), "GET", "https://a.test/1", 200)

		assertEvidenceChanged(t, f, cursor)
	})

	t.Run("updated exchange", func(t *testing.T) {
		f := newFixture(t, true)
		seed(f)
		cursor := cursorAfterFirstPage(f)
		if _, err := f.db.ExecContext(f.ctx,
			`UPDATE http_exchanges SET status_code = 500, updated_at = ? WHERE run_id = 'run-1'`,
			database.FormatTime(at.Add(time.Second))); err != nil {
			t.Fatalf("update exchange: %v", err)
		}

		assertEvidenceChanged(t, f, cursor)
	})

	t.Run("retention removed the exchanges", func(t *testing.T) {
		f := newFixture(t, true)
		seed(f)
		cursor := cursorAfterFirstPage(f)
		if _, err := f.db.ExecContext(f.ctx, `DELETE FROM http_exchanges WHERE run_id = 'run-1'`); err != nil {
			t.Fatalf("delete exchanges: %v", err)
		}

		assertEvidenceChanged(t, f, cursor)
	})

	t.Run("run status changed", func(t *testing.T) {
		f := newFixture(t, true)
		seed(f)
		cursor := cursorAfterFirstPage(f)
		if _, err := f.db.ExecContext(f.ctx,
			`UPDATE runs SET status = 'cancelled' WHERE id = 'run-1'`); err != nil {
			t.Fatalf("update run: %v", err)
		}

		assertEvidenceChanged(t, f, cursor)
	})
}

func assertEvidenceChanged(t *testing.T, f *fixture, cursor string) {
	t.Helper()
	_, err := f.reader.Page(f.ctx, Request{
		RunID: "run-1", IncludeHTTP: true, Limit: 2, After: cursor,
	})
	if !errors.Is(err, ErrEvidenceChanged) {
		t.Fatalf("continuation error = %v, want ErrEvidenceChanged", err)
	}
}

// TestLogRevisionDetectsDeleteAndReinsert covers the pair that stands in for a
// row count: a whole-prefix removal must move the evidence even when the number
// of remaining rows is unchanged.
func TestLogRevisionDetectsDeleteAndReinsert(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusFailed)
	at := base()
	f.addLog("run-1", at, runs.StreamOtter, "a")
	f.addLog("run-1", at.Add(time.Millisecond), runs.StreamOtter, "b")

	first := f.page("run-1", 1)
	cursor := first.NextCursor
	if cursor == "" {
		t.Fatalf("expected a continuation")
	}

	// Remove the oldest row and append a replacement: same count, different ids.
	if _, err := f.db.ExecContext(f.ctx,
		`DELETE FROM run_logs WHERE run_id = 'run-1' AND message = 'a'`); err != nil {
		t.Fatalf("delete log: %v", err)
	}
	f.addLog("run-1", at.Add(2*time.Millisecond), runs.StreamStdout, "c")

	assertEvidenceChanged(t, f, cursor)
}

// TestCursorValidation covers the rejections that must not be reinterpreted.
func TestCursorValidation(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.addRun("run-2", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()
	for i := 0; i < 4; i++ {
		f.addLog("run-1", at.Add(time.Duration(i)*time.Millisecond), runs.StreamOtter, fmt.Sprintf("l%d", i))
	}
	cursor := f.page("run-1", 2).NextCursor
	if cursor == "" {
		t.Fatalf("expected a continuation")
	}

	cases := []struct {
		name    string
		runID   string
		include bool
		after   string
	}{
		{"garbage", "run-1", true, "not-a-cursor"},
		{"another run", "run-2", true, cursor},
		{"different http setting", "run-1", false, cursor},
		{"oversized", "run-1", true, strings.Repeat("A", 5000)},
		{"empty payload", "run-1", true, "e30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.reader.Page(f.ctx, Request{
				RunID: tc.runID, IncludeHTTP: tc.include, Limit: 2, After: tc.after,
			})
			if !errors.Is(err, ErrCursorInvalid) && !errors.Is(err, ErrEvidenceChanged) {
				t.Fatalf("error = %v, want a cursor rejection", err)
			}
		})
	}
}

// TestCursorRoundTrip pins the encoded form so a cursor that a previous version
// issued cannot silently change meaning.
func TestCursorRoundTrip(t *testing.T) {
	original := Cursor{
		Version:  SchemaVersion,
		RunID:    "run-1",
		Include:  true,
		Revision: "deadbeef",
		Position: Position{At: base(), Rank: RankHTTP, ID: 42},
	}
	token, err := original.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := DecodeCursor(token)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if decoded.RunID != original.RunID || decoded.Include != original.Include ||
		decoded.Revision != original.Revision || decoded.Position.Rank != original.Position.Rank ||
		decoded.Position.ID != original.Position.ID || !decoded.Position.At.Equal(original.Position.At) {
		t.Fatalf("round trip = %+v, want %+v", decoded, original)
	}
}

// TestNonTerminalAndUnknownRunsAreRefused keeps a timeline honest: it is a record
// of a finished attempt, and an unknown run is not an empty one.
func TestNonTerminalAndUnknownRunsAreRefused(t *testing.T) {
	f := newFixture(t, true)

	f.addRun("running-run", runs.StatusRunning)
	if _, err := f.reader.Page(f.ctx, Request{RunID: "running-run", IncludeHTTP: true}); !errors.Is(err, ErrRunNotTerminal) {
		t.Fatalf("running error = %v, want ErrRunNotTerminal", err)
	}

	f.addRun("queued-run", runs.StatusQueued)
	if _, err := f.reader.Page(f.ctx, Request{RunID: "queued-run", IncludeHTTP: true}); !errors.Is(err, ErrRunNotTerminal) {
		t.Fatalf("queued error = %v, want ErrRunNotTerminal", err)
	}

	// retrying is the next attempt waiting to execute, not its finished parent.
	f.addRun("retry-run", runs.StatusRetrying)
	if _, err := f.reader.Page(f.ctx, Request{RunID: "retry-run", IncludeHTTP: true}); !errors.Is(err, ErrRunNotTerminal) {
		t.Fatalf("retrying error = %v, want ErrRunNotTerminal", err)
	}

	if _, err := f.reader.Page(f.ctx, Request{RunID: "no-such-run", IncludeHTTP: true}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run error = %v, want ErrRunNotFound", err)
	}
}

// TestContextSurvivesMissingLogsAndExpiredCapture covers the facts the header has
// to carry when there is nothing else to show.
func TestContextSurvivesMissingLogsAndExpiredCapture(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()
	f.addExchange("run-1", "r0", at, "POST", "https://a.test/x", 400)

	// Expire the payloads, keeping the summary: the trace must say expired
	// rather than showing an empty list as if nothing was recorded. The capture
	// summary's own clock is the daemon's, so age it past the cutoff first.
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE run_capture SET started_at = ? WHERE run_id = 'run-1'`,
		database.FormatTime(at)); err != nil {
		t.Fatalf("age capture summary: %v", err)
	}
	if _, _, err := f.inspection.ExpireOlderThan(f.ctx, at.Add(time.Hour), 10); err != nil {
		t.Fatalf("ExpireOlderThan: %v", err)
	}

	page := f.page("run-1", 100)
	if page.Context.Capture == nil {
		t.Fatalf("capture summary is missing")
	}
	if page.Context.Capture.State != inspection.CaptureExpired {
		t.Errorf("capture state = %q, want %q", page.Context.Capture.State, inspection.CaptureExpired)
	}
	// The status is authoritative even though no lifecycle line was ever written.
	if page.Context.Status != string(runs.StatusFailed) {
		t.Errorf("status = %q, want failed", page.Context.Status)
	}
	for _, event := range page.Events {
		if event.Kind == KindHTTP {
			t.Errorf("an expired recording must not still show exchanges")
		}
	}
}

// TestUnavailableCaptureWithoutInspection covers the oldest runs: no recording
// at all is different from a recording that observed nothing.
func TestUnavailableCaptureWithoutInspection(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusSucceeded)
	at := base()
	f.addLog("run-1", at, runs.StreamStdout, "hello")

	page := f.page("run-1", 100)
	if page.Context.Capture == nil || page.Context.Capture.State != inspection.CaptureUnavailable {
		t.Fatalf("capture = %+v, want unavailable", page.Context.Capture)
	}
	if len(page.Events) != 1 || page.Events[0].Message != "hello" {
		t.Fatalf("events = %+v, want the log line", page.Events)
	}
}

// TestNoHTTPExclusionStillReportsCapture keeps --no-http honest: excluding the
// events must not hide whether a recording existed.
func TestNoHTTPExclusionStillReportsCapture(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()
	f.addExchange("run-1", "r0", at, "POST", "https://a.test/x", 400)
	f.addLog("run-1", at.Add(time.Millisecond), runs.StreamOtter, "done")

	page, err := f.reader.Page(f.ctx, Request{RunID: "run-1", IncludeHTTP: false, Limit: 100})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.Context.Capture == nil || page.Context.Capture.Policy != inspection.PolicyFull {
		t.Fatalf("capture summary was dropped when HTTP was excluded: %+v", page.Context.Capture)
	}
	for _, event := range page.Events {
		if event.Kind == KindHTTP {
			t.Errorf("--no-http still returned an exchange: %+v", event)
		}
	}
	if page.Context.IncludeHTTP {
		t.Errorf("context must record that HTTP events were excluded by the operator")
	}
}

// TestLimitIsBounded keeps a caller from asking for an unbounded read.
func TestLimitIsBounded(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusSucceeded)
	at := base()
	for i := 0; i < MaxLimit+5; i++ {
		f.addLog("run-1", at.Add(time.Duration(i)*time.Microsecond), runs.StreamStdout, fmt.Sprintf("l%d", i))
	}

	page, err := f.reader.Page(f.ctx, Request{RunID: "run-1", IncludeHTTP: true, Limit: MaxLimit + 500})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Events) != MaxLimit {
		t.Fatalf("events = %d, want the %d cap", len(page.Events), MaxLimit)
	}
	if !page.HasMore {
		t.Fatalf("expected a continuation past the cap")
	}
}

// TestReadDeadlineIsReportedNotSwallowed keeps a slow read a clear failure rather
// than a partial page.
func TestReadDeadlineIsReportedNotSwallowed(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusSucceeded)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.reader.Page(ctx, Request{RunID: "run-1", IncludeHTTP: true, Limit: 10})
	if err == nil {
		t.Fatalf("a cancelled read must fail")
	}
	if !errors.Is(err, ErrReadDeadline) {
		t.Fatalf("error = %v, want ErrReadDeadline", err)
	}
}

// TestPageDoesNotReadPayloadColumns guards the projection: a body or header must
// never be able to reach the timeline, so a page assembled over an exchange whose
// payload columns are unreadable still succeeds.
func TestPageDoesNotReadPayloadColumns(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()
	id := f.addExchange("run-1", "r0", at, "POST", "https://a.test/x", 400)

	// Store a body that is not valid JSON in the payload column. If any timeline
	// read touched it, decoding would break; the projection must not care.
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE http_exchanges SET request_headers = 'not json at all', request_body = 'not json either' WHERE id = ?`,
		id); err != nil {
		t.Fatalf("write payload columns: %v", err)
	}

	page := f.page("run-1", 100)
	if len(page.Events) != 1 || page.Events[0].HTTP == nil {
		t.Fatalf("events = %+v, want the exchange summary", page.Events)
	}
	if page.Events[0].HTTP.Method != "POST" {
		t.Errorf("method = %q, want POST", page.Events[0].HTTP.Method)
	}
}

// TestRevisionIsStableAcrossReads makes the cursor reproducible: two pages read
// from unchanged evidence must agree on the revision.
func TestRevisionIsStableAcrossReads(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()
	f.addLog("run-1", at, runs.StreamOtter, "a")
	f.addExchange("run-1", "r0", at.Add(time.Millisecond), "GET", "https://a.test/0", 200)

	first, err := f.reader.Page(f.ctx, Request{RunID: "run-1", IncludeHTTP: true, Limit: 1})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := f.reader.Page(f.ctx, Request{RunID: "run-1", IncludeHTTP: true, Limit: 1})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if first.NextCursor != second.NextCursor {
		t.Fatalf("cursor changed without an evidence change:\n%s\n%s", first.NextCursor, second.NextCursor)
	}
}

// TestSnapshotAtIsInformational documents that the field does not pin anything.
func TestSnapshotAtIsInformational(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusSucceeded)

	page := f.page("run-1", 10)
	if page.SnapshotAt.IsZero() {
		t.Fatalf("snapshot_at should record when the page was read")
	}
}

// TestFailedRetryParentIsTraceableWhileRetryWaits is the plan's retry case: the
// finished parent is inspectable, and the waiting retry is refused.
func TestFailedRetryParentIsTraceableWhileRetryWaits(t *testing.T) {
	f := newFixture(t, false)
	parent := "run-parent"
	child := "run-child"
	f.addRun(parent, runs.StatusFailed)
	f.addRun(child, runs.StatusRetrying)

	parentID := parent
	if err := f.runs.Finish(f.ctx, parent, runs.Finish{Status: runs.StatusFailed}); err != nil {
		t.Fatalf("finish parent: %v", err)
	}
	// Link the retry to its parent.
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE runs SET parent_run_id = ? WHERE id = ?`, parentID, child); err != nil {
		t.Fatalf("link retry: %v", err)
	}
	f.addLog(parent, base(), runs.StreamOtter, "run started")

	page := f.page(parent, 10)
	if page.Context.Status != string(runs.StatusFailed) {
		t.Errorf("parent status = %q, want failed", page.Context.Status)
	}

	if _, err := f.reader.Page(f.ctx, Request{RunID: child, IncludeHTTP: true}); !errors.Is(err, ErrRunNotTerminal) {
		t.Fatalf("retry error = %v, want ErrRunNotTerminal", err)
	}
}

// TestPaginationSurvivesCursorTimestampPrecision is a regression test for a
// subtle failure mode: the cursor carries a formatted timestamp, so a position
// with precision beyond that format must still be able to resume without losing
// the event it points at. It is why a continuation is an exclusive bound on the
// ordering key rather than an inclusive bound on the displayed time.
func TestPaginationSurvivesCursorTimestampPrecision(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusSucceeded)

	// Nanosecond timestamps, one nanosecond apart: the formatted cursor keeps
	// nine decimal places, so an inclusive comparison is the risky case.
	at := base()
	for i := 0; i < 5; i++ {
		f.addLog("run-1", at.Add(time.Duration(i)*time.Nanosecond), runs.StreamStdout, fmt.Sprintf("l%d", i))
	}

	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatalf("pagination did not terminate")
		}
		result, err := f.reader.Page(f.ctx, Request{
			RunID: "run-1", IncludeHTTP: true, Limit: 1, After: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, event := range result.Events {
			seen = append(seen, event.Message)
		}
		if !result.HasMore {
			break
		}
		cursor = result.NextCursor
	}

	want := []string{"l0", "l1", "l2", "l3", "l4"}
	if len(seen) != len(want) {
		t.Fatalf("got %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("got %v, want %v", seen, want)
		}
	}
}

// key renders an event for order comparisons in tests.
func key(event Event) string {
	if event.Kind == KindHTTP && event.HTTP != nil {
		return "http:" + event.HTTP.RequestID
	}
	return event.Kind + ":" + event.Message
}

// TestCursorsAreOpaqueJSON guards against a cursor accidentally embedding a
// payload: it may only carry framing and position.
func TestCursorsAreOpaqueJSON(t *testing.T) {
	f := newFixture(t, true)
	f.addRun("run-1", runs.StatusFailed)
	f.beginCapture("run-1")
	at := base()
	f.addLog("run-1", at, runs.StreamOtter, "a")
	f.addLog("run-1", at.Add(time.Millisecond), runs.StreamOtter, "b")

	cursor := f.page("run-1", 1).NextCursor
	decoded, err := DecodeCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	if strings.Contains(string(raw), "a") && strings.Contains(string(raw), "b") && len(raw) > 400 {
		t.Fatalf("cursor looks like it carries event data: %s", raw)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal cursor: %v", err)
	}
	for _, forbidden := range []string{"message", "url", "call_site", "request_headers"} {
		if _, ok := probe[forbidden]; ok {
			t.Errorf("cursor carries %q", forbidden)
		}
	}
}

var _ = sql.ErrNoRows

// TestCtxLogIsNotALifecycleEvent is a regression test for a real
// misclassification: the daemon's narration and the SDK's ctx.log output share
// the `otter` stream, so classifying by stream alone labelled an integration's
// own log line a lifecycle event.
func TestCtxLogIsNotALifecycleEvent(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusFailed)
	at := base()

	// Exactly the shape the SDK stores: message, a space, then the JSON fields.
	ctxLog := `sync starting {"dry_run":false,"level":"info","logger":"otter","object":"Contact","page_size":100}`
	f.addLogOrigin("run-1", at, runs.StreamOtter, ctxLog, runs.OriginChild)
	f.addLogOrigin("run-1", at.Add(time.Millisecond), runs.StreamOtter,
		"run started (attempt 1 of 1, trigger manual)", runs.OriginDaemon)
	f.addLogOrigin("run-1", at.Add(2*time.Millisecond), runs.StreamStdout, "plain output", runs.OriginChild)

	page := f.page("run-1", 100)
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(page.Events))
	}
	if got := page.Events[0].Kind; got != KindLog {
		t.Errorf("a ctx.log line on the otter stream is kind %q, want %q", got, KindLog)
	}
	if got := page.Events[0].Message; got != ctxLog {
		t.Errorf("the stored message must be preserved verbatim, got %q", got)
	}
	if got := page.Events[1].Kind; got != KindLifecycle {
		t.Errorf("a narration line is kind %q, want %q", got, KindLifecycle)
	}
	if got := page.Events[2].Kind; got != KindLog {
		t.Errorf("child stdout is kind %q, want %q", got, KindLog)
	}
}

// TestLegacyCtxLogIsClassifiedByItsMarker covers rows written before the origin
// column existed: the SDK's structured suffix is the only available signal.
func TestLegacyCtxLogIsClassifiedByItsMarker(t *testing.T) {
	f := newFixture(t, false)
	f.addRun("run-1", runs.StatusFailed)
	at := base()

	f.addLog("run-1", at, runs.StreamOtter,
		`sync starting {"dry_run":false,"level":"info"}`)
	f.addLog("run-1", at.Add(time.Millisecond), runs.StreamOtter, "run queued (trigger manual)")
	// A child sentence that merely starts with a narration word must not be
	// mistaken for narration of its own.
	f.addLog("run-1", at.Add(2*time.Millisecond), runs.StreamOtter, "run failed because the token expired")

	page := f.page("run-1", 100)
	kinds := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		kinds = append(kinds, event.Kind)
	}
	want := []string{KindLog, KindLifecycle, KindLog}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}
