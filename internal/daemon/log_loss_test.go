package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/runs"
)

// errDiskFull is the error SQLite returns when the filesystem behind it is out
// of space, copied from the HW-6b host finding so the test names the defect it
// covers rather than a generic failure.
var errDiskFull = errors.New("runs: commit log batch: database or disk is full (13)")

// failingBatchAppender is a log store whose writes always fail. It stands in
// for the full disk: an ENOSPC filesystem cannot be produced portably in a unit
// test, so the failure is injected at the one call the sink makes.
type failingBatchAppender struct {
	err     error
	batches int
}

func (f *failingBatchAppender) AppendBatch(_ context.Context, _ []runs.LogEntry) error {
	f.batches++
	return f.err
}

// newLogLossDaemon returns a daemon whose run store is real and migrated, and
// enough of a Daemon for GetRunDetail. It deliberately does not go through
// daemon.New: this defect involves no manifest, no release and no Python, and a
// test that needed those would skip on exactly the hosts the regression must
// run on.
func newLogLossDaemon(t *testing.T) (*Daemon, *runs.Store, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := runs.NewStore(db.DB)
	return &Daemon{
		runs: store,
		logs: runs.NewLogStore(db.DB),
		reg:  &registry{},
		log:  testLogger(),
	}, store, db.DB
}

// createLogLossRun writes a finished run with the honest exit code the HW-6b
// finding describes: succeeded, exit_code = 0.
func createLogLossRun(t *testing.T, store *runs.Store, id string) {
	t.Helper()
	ctx := context.Background()

	if err := store.Create(ctx, &runs.Run{
		ID:          id,
		JobID:       "counter",
		JobName:     "counter",
		TriggerType: runs.TriggerManual,
		Status:      runs.StatusSucceeded,
		Attempt:     1,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	exitCode := 0
	if err := store.Finish(ctx, id, runs.Finish{
		Status:     runs.StatusSucceeded,
		ExitCode:   &exitCode,
		FinishedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

// TestCapturedOutputWriteFailureIsReportedOnTheRun is the regression for
// OT-020's sink half.
//
// On the HW-6b host a full disk made every `run_logs` batch write fail while
// the run still reported `status=succeeded, exit_code=0`, and the only signal
// was a `run_log_write_failed` journal line. The run view must say the output
// was lost, and how much of it, without the operator reading the journal.
//
// With the marking removed from `runLogSink.write` this test fails: the failure
// is rendered, but the run view is indistinguishable from one that lost
// nothing.
func TestCapturedOutputWriteFailureIsReportedOnTheRun(t *testing.T) {
	ctx := context.Background()
	d, store, _ := newLogLossDaemon(t)

	const runID = "run-log-loss"
	createLogLossRun(t, store, runID)

	appender := &failingBatchAppender{err: errDiskFull}
	sink := newRunLogSink(appender, store, runID, testLogger())
	batch := []runs.LogEntry{
		{RunID: runID, Stream: runs.StreamStdout, Message: "one"},
		{RunID: runID, Stream: runs.StreamStdout, Message: "two"},
		{RunID: runID, Stream: runs.StreamStdout, Message: "three"},
	}
	sink.write(batch)

	if appender.batches != 1 {
		t.Fatalf("the failing appender was called %d times, want 1", appender.batches)
	}

	view, err := d.GetRunDetail(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if !view.LogLost {
		t.Errorf("run %s reports no log loss after every batch write failed: %+v", runID, view.Run)
	}
	if view.LogLinesDropped != int64(len(batch)) {
		t.Errorf("log_lines_dropped = %d, want %d (the whole rolled-back batch)",
			view.LogLinesDropped, len(batch))
	}

	// The loss has to be on the JSON surface too, which is what the CLI and a
	// control plane read.
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal run view: %v", err)
	}
	if !strings.Contains(string(encoded), `"log_lost":true`) {
		t.Errorf("the run view JSON does not carry log_lost: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"log_lines_dropped":3`) {
		t.Errorf("the run view JSON does not carry the dropped count: %s", encoded)
	}
}

// TestOtterNarrationWriteFailureIsReportedOnTheRun is the same regression for
// the other writer: the daemon's own lifecycle narration, written through
// `appendOtterLog` in a single-line transaction. It must mark the run exactly
// as the captured-output path does.
func TestOtterNarrationWriteFailureIsReportedOnTheRun(t *testing.T) {
	ctx := context.Background()
	d, store, _ := newLogLossDaemon(t)

	const runID = "run-narration-loss"
	createLogLossRun(t, store, runID)

	// A log store whose database is closed: every append fails, which is the
	// daemon-narration half of the same full-disk path.
	logDB, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open log database: %v", err)
	}
	d.logs = runs.NewLogStore(logDB.DB)
	if err := logDB.Close(); err != nil {
		t.Fatalf("close log database: %v", err)
	}

	d.appendOtterLog(runID, "run started (attempt 1 of 1, trigger manual)")

	view, err := d.GetRunDetail(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if !view.LogLost {
		t.Errorf("run %s reports no log loss after its lifecycle line failed to write: %+v", runID, view.Run)
	}
	if view.LogLinesDropped != 1 {
		t.Errorf("log_lines_dropped = %d, want 1 (the lost narration line)", view.LogLinesDropped)
	}
}

// TestRunWithoutLogLossReportsNone pins the additive half: an intact run must
// not carry the keys at all, so the ordinary run record and its consumers are
// unchanged.
func TestRunWithoutLogLossReportsNone(t *testing.T) {
	ctx := context.Background()
	d, store, _ := newLogLossDaemon(t)

	const runID = "run-intact"
	createLogLossRun(t, store, runID)

	view, err := d.GetRunDetail(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if view.LogLost || view.LogLinesDropped != 0 {
		t.Errorf("an intact run reports log loss: lost=%t dropped=%d", view.LogLost, view.LogLinesDropped)
	}

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal run view: %v", err)
	}
	for _, key := range []string{`"log_lost"`, `"log_lines_dropped"`} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("an intact run carries %s, so the field is not additive-and-omitted: %s", key, encoded)
		}
	}
}
