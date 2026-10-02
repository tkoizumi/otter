package runs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// newRetentionStore returns a run store, a log store and the database handle
// behind them for a fresh migrated database.
func newRetentionStore(t *testing.T) (*database.DB, *Store, *LogStore) {
	t.Helper()

	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, NewStore(db.DB), NewLogStore(db.DB)
}

// seedRun writes one attempt, optionally parented, and one log line stamped at
// its creation time.
func seedRun(t *testing.T, store *Store, logs *LogStore, id string, status Status, createdAt time.Time, parent *string) {
	t.Helper()
	run := &Run{
		ID:          id,
		JobID:       "shopify",
		TriggerType: TriggerManual,
		Status:      status,
		Attempt:     1,
		CreatedAt:   createdAt,
	}
	if parent != nil {
		run.ParentRunID = parent
		run.Attempt = 2
	}
	if err := store.Create(context.Background(), run); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
	if err := logs.Append(context.Background(), LogEntry{
		RunID: id, Stream: StreamStdout, Message: "output of " + id, Timestamp: createdAt,
	}); err != nil {
		t.Fatalf("append log for %s: %v", id, err)
	}
}

func logCount(t *testing.T, logs *LogStore, runID string) int {
	t.Helper()
	entries, err := logs.List(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatalf("list logs for %s: %v", runID, err)
	}
	return len(entries)
}

func runExists(t *testing.T, store *Store, id string) bool {
	t.Helper()
	_, err := store.Get(context.Background(), id)
	if errors.Is(err, ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return true
}

// retentionFixture seeds two expired settled chains, a live run, a live retry
// chain whose root is expired, and a recent run.
type retentionFixture struct {
	now   time.Time
	old1  string
	old2  string
	live  string
	root  string
	retry string
}

func seedRetentionFixture(t *testing.T, store *Store, logs *LogStore) retentionFixture {
	t.Helper()
	now := time.Now().UTC()
	f := retentionFixture{
		now:   now,
		old1:  "old-succeeded",
		old2:  "old-failed",
		live:  "live-running",
		root:  "chain-root",
		retry: "chain-retry",
	}

	seedRun(t, store, logs, f.old1, StatusSucceeded, now.Add(-72*time.Hour), nil)
	seedRun(t, store, logs, f.old2, StatusFailed, now.Add(-48*time.Hour), nil)
	seedRun(t, store, logs, f.live, StatusRunning, now.Add(-72*time.Hour), nil)
	seedRun(t, store, logs, f.root, StatusFailed, now.Add(-72*time.Hour), nil)
	seedRun(t, store, logs, f.retry, StatusRetrying, now.Add(-48*time.Hour), &f.root)
	seedRun(t, store, logs, "recent-succeeded", StatusSucceeded, now.Add(-time.Hour), nil)
	return f
}

func TestExpiredRunIDsSelectsOnlySettledOldChains(t *testing.T) {
	_, store, logs := newRetentionStore(t)
	f := seedRetentionFixture(t, store, logs)
	ctx := context.Background()
	cutoff := f.now.Add(-24 * time.Hour)

	ids, err := store.ExpiredRunIDs(ctx, cutoff, RetentionBatchChains)
	if err != nil {
		t.Fatalf("expired run ids: %v", err)
	}

	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[f.old1] || !got[f.old2] {
		t.Fatalf("expired ids = %v, want exactly %s and %s", ids, f.old1, f.old2)
	}
	for _, keep := range []string{f.live, f.root, f.retry, "recent-succeeded"} {
		if got[keep] {
			t.Errorf("expired ids unexpectedly contain %s: %v", keep, ids)
		}
	}
}

func TestExpiredRunIDsReturnsAChainWholeWhenBounded(t *testing.T) {
	_, store, logs := newRetentionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cutoff := now.Add(-24 * time.Hour)

	rootA := "root-a"
	rootB := "root-b"
	seedRun(t, store, logs, rootA, StatusFailed, now.Add(-72*time.Hour), nil)
	seedRun(t, store, logs, "retry-a", StatusFailed, now.Add(-71*time.Hour), &rootA)
	seedRun(t, store, logs, rootB, StatusFailed, now.Add(-70*time.Hour), nil)
	seedRun(t, store, logs, "retry-b", StatusFailed, now.Add(-69*time.Hour), &rootB)

	// One chain per call: a batch must never split a chain across calls.
	ids, err := store.ExpiredRunIDs(ctx, cutoff, 1)
	if err != nil {
		t.Fatalf("expired run ids: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("first batch = %v, want both attempts of one chain", ids)
	}
	first := map[string]bool{ids[0]: true, ids[1]: true}
	if !(first[rootA] && first["retry-a"]) && !(first[rootB] && first["retry-b"]) {
		t.Fatalf("first batch split a chain: %v", ids)
	}
}

func TestDeleteOlderThanPrunesOnlySettledOldChains(t *testing.T) {
	_, store, logs := newRetentionStore(t)
	f := seedRetentionFixture(t, store, logs)
	ctx := context.Background()
	cutoff := f.now.Add(-24 * time.Hour)

	removed, err := logs.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("delete older than: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed %d log rows, want the 2 expired settled runs", removed)
	}
	if n := logCount(t, logs, f.old1); n != 0 {
		t.Errorf("%s kept %d log rows, want 0", f.old1, n)
	}
	if n := logCount(t, logs, f.old2); n != 0 {
		t.Errorf("%s kept %d log rows, want 0", f.old2, n)
	}
	// Live work and a chain with a live attempt keep every line.
	for _, keep := range []string{f.live, f.root, f.retry} {
		if n := logCount(t, logs, keep); n != 1 {
			t.Errorf("%s kept %d log rows, want 1 (live work is never pruned)", keep, n)
		}
	}
	if n := logCount(t, logs, "recent-succeeded"); n != 1 {
		t.Errorf("recent run kept %d log rows, want 1", n)
	}
}

func TestDeleteOlderThanIsBoundedAndIdempotent(t *testing.T) {
	_, store, logs := newRetentionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cutoff := now.Add(-24 * time.Hour)

	seedRun(t, store, logs, "chatty", StatusSucceeded, now.Add(-72*time.Hour), nil)

	// Fill past one batch so a single call has to stop at the bound.
	batch := make([]LogEntry, 0, LogRetentionBatch+5)
	for i := 0; i < LogRetentionBatch+5; i++ {
		batch = append(batch, LogEntry{
			RunID: "chatty", Stream: StreamStdout,
			Message: fmt.Sprintf("line %d", i), Timestamp: now.Add(-72 * time.Hour),
		})
	}
	if err := logs.AppendBatch(ctx, batch); err != nil {
		t.Fatalf("append batch: %v", err)
	}

	first, err := logs.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first != LogRetentionBatch {
		t.Fatalf("first pass removed %d rows, want the %d-row bound", first, LogRetentionBatch)
	}
	second, err := logs.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second != 6 {
		t.Fatalf("second pass removed %d rows, want the remaining 6 (one seeded plus 5)", second)
	}
	third, err := logs.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if third != 0 {
		t.Fatalf("third pass removed %d rows, want 0 (idempotent)", third)
	}
}

func TestDeleteRunsCascadesLogsAndIsIdempotent(t *testing.T) {
	_, store, logs := newRetentionStore(t)
	f := seedRetentionFixture(t, store, logs)
	ctx := context.Background()
	cutoff := f.now.Add(-24 * time.Hour)

	ids, err := store.ExpiredRunIDs(ctx, cutoff, RetentionBatchChains)
	if err != nil {
		t.Fatalf("expired run ids: %v", err)
	}
	removed, err := store.DeleteRuns(ctx, ids)
	if err != nil {
		t.Fatalf("delete runs: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed %d runs, want 2", removed)
	}

	for _, gone := range []string{f.old1, f.old2} {
		if runExists(t, store, gone) {
			t.Errorf("%s still exists after retention", gone)
		}
		if n := logCount(t, logs, gone); n != 0 {
			t.Errorf("%s left %d orphaned log rows, want 0", gone, n)
		}
	}
	for _, keep := range []string{f.live, f.root, f.retry, "recent-succeeded"} {
		if !runExists(t, store, keep) {
			t.Errorf("%s was deleted but is not expired", keep)
		}
		if n := logCount(t, logs, keep); n != 1 {
			t.Errorf("%s kept %d log rows, want 1", keep, n)
		}
	}

	// A second pass finds and removes nothing.
	again, err := store.ExpiredRunIDs(ctx, cutoff, RetentionBatchChains)
	if err != nil {
		t.Fatalf("second expired read: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second pass found %v, want nothing", again)
	}
	secondRemoved, err := store.DeleteRuns(ctx, again)
	if err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if secondRemoved != 0 {
		t.Fatalf("second pass removed %d runs, want 0", secondRemoved)
	}
}

// DeleteRuns must refuse a live run even if a caller hands it one: the
// terminal-only guard is the last line of defence for the "never delete live
// work" rule.
func TestDeleteRunsRefusesLiveWork(t *testing.T) {
	_, store, logs := newRetentionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedRun(t, store, logs, "live", StatusRunning, now.Add(-72*time.Hour), nil)
	seedRun(t, store, logs, "done", StatusSucceeded, now.Add(-72*time.Hour), nil)

	removed, err := store.DeleteRuns(ctx, []string{"live", "done"})
	if err != nil {
		t.Fatalf("delete runs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d runs, want only the terminal one", removed)
	}
	if !runExists(t, store, "live") {
		t.Error("live run was deleted")
	}
	if n := logCount(t, logs, "live"); n != 1 {
		t.Errorf("live run kept %d log rows, want 1", n)
	}
	if runExists(t, store, "done") {
		t.Error("terminal run survived")
	}
}

// The timeline's O(1) evidence revision claims every log mutation is a whole-row
// append or delete. DeleteRuns is a new bulk delete, so pin the claim: the
// revision has to observe it.
func TestLogEvidenceObservesDeleteRuns(t *testing.T) {
	db, store, logs := newRetentionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedRun(t, store, logs, "retired", StatusSucceeded, now.Add(-72*time.Hour), nil)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	before, err := logs.LogEvidenceTx(ctx, tx, "retired")
	if err != nil {
		t.Fatalf("evidence before: %v", err)
	}
	if !before.Present {
		t.Fatal("evidence did not see the seeded log row")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, err := store.DeleteRuns(ctx, []string{"retired"}); err != nil {
		t.Fatalf("delete runs: %v", err)
	}

	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	after, err := logs.LogEvidenceTx(ctx, tx2, "retired")
	if err != nil {
		t.Fatalf("evidence after: %v", err)
	}
	if after.Present {
		t.Fatalf("evidence still present after DeleteRuns: %+v", after)
	}
}
