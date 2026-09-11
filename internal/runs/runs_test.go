package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/otter-runtime/otter/internal/database"
)

// newTestStore returns a run and log store backed by a fresh migrated database.
func newTestStore(t *testing.T) (*Store, *LogStore) {
	t.Helper()

	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(db.DB), NewLogStore(db.DB)
}

func sampleRun(id string, status Status, attempt int) *Run {
	return &Run{
		ID:            id,
		IntegrationID: "shopify",
		TriggerType:   TriggerManual,
		Status:        status,
		Attempt:       attempt,
		CreatedAt:     time.Now().UTC(),
		Metadata:      json.RawMessage(`{"type":"manual"}`),
	}
}

func TestStatusSemantics(t *testing.T) {
	for _, status := range AllStatuses() {
		if !status.Valid() {
			t.Errorf("%s should be valid", status)
		}
	}
	if Status("nonsense").Valid() {
		t.Error("an unknown status should not be valid")
	}

	terminal := []Status{StatusSucceeded, StatusFailed, StatusCancelled, StatusTimedOut}
	for _, status := range terminal {
		if !status.Terminal() {
			t.Errorf("%s should be terminal", status)
		}
	}
	inFlight := []Status{StatusQueued, StatusRunning, StatusRetrying}
	for _, status := range inFlight {
		if status.Terminal() {
			t.Errorf("%s should not be terminal", status)
		}
	}
}

func TestCreateGetAndRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	run := sampleRun("run-1", StatusQueued, 1)
	if err := store.Create(ctx, run); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IntegrationID != "shopify" || got.Status != StatusQueued || got.Attempt != 1 {
		t.Errorf("round trip = %+v", got)
	}
	if got.TriggerType != TriggerManual {
		t.Errorf("trigger type = %q, want manual", got.TriggerType)
	}
	if got.StartedAt != nil || got.FinishedAt != nil || got.ExitCode != nil || got.Error != nil {
		t.Errorf("a queued run should have no timing or outcome fields: %+v", got)
	}
	if string(got.Metadata) != `{"type":"manual"}` {
		t.Errorf("metadata = %s", got.Metadata)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should be set")
	}

	if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing run error = %v, want ErrNotFound", err)
	}
}

func TestCreateRejectsAnInvalidStatus(t *testing.T) {
	store, _ := newTestStore(t)

	run := sampleRun("bad", Status("exploded"), 1)
	if err := store.Create(context.Background(), run); err == nil {
		t.Error("creating a run with an unknown status should fail")
	}
}

func TestMarkRunningOnlyTransitionsClaimableRuns(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.Create(ctx, sampleRun("queued", StatusQueued, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, sampleRun("retrying", StatusRetrying, 2)); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, sampleRun("done", StatusSucceeded, 1)); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().UTC()

	for _, id := range []string{"queued", "retrying"} {
		ok, err := store.MarkRunning(ctx, id, startedAt)
		if err != nil {
			t.Fatalf("mark %s running: %v", id, err)
		}
		if !ok {
			t.Errorf("%s should be claimable", id)
		}
		run, _ := store.Get(ctx, id)
		if run.Status != StatusRunning {
			t.Errorf("%s status = %s, want running", id, run.Status)
		}
		if run.StartedAt == nil {
			t.Errorf("%s should have started_at", id)
		}
	}

	// A finished run must not be silently resurrected.
	ok, err := store.MarkRunning(ctx, "done", startedAt)
	if err != nil {
		t.Fatalf("mark done running: %v", err)
	}
	if ok {
		t.Error("a terminal run must not be claimable")
	}

	// Claiming twice must fail the second time.
	ok, err = store.MarkRunning(ctx, "queued", startedAt)
	if err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if ok {
		t.Error("a running run must not be claimable again")
	}
}

func TestFinishRecordsOutcome(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	run := sampleRun("run-1", StatusRunning, 1)
	startedAt := time.Now().UTC().Add(-2 * time.Second)
	run.StartedAt = &startedAt
	if err := store.Create(ctx, run); err != nil {
		t.Fatal(err)
	}

	code := 3
	if err := store.Finish(ctx, "run-1", Finish{
		Status:   StatusFailed,
		ExitCode: &code,
		Error:    "process exited with code 3",
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	run, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusFailed {
		t.Errorf("status = %s, want failed", run.Status)
	}
	if run.ExitCode == nil || *run.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", run.ExitCode)
	}
	if run.ErrorString() != "process exited with code 3" {
		t.Errorf("error = %q", run.ErrorString())
	}
	if run.FinishedAt == nil {
		t.Error("finished_at should be set")
	}
	if run.Duration() <= 0 {
		t.Error("duration should be positive once started and finished are set")
	}

	if err := store.Finish(ctx, "missing", Finish{Status: StatusFailed}); !errors.Is(err, ErrNotFound) {
		t.Errorf("finishing a missing run = %v, want ErrNotFound", err)
	}
	if err := store.Finish(ctx, "run-1", Finish{Status: StatusRunning}); err == nil {
		t.Error("finishing with a non-terminal status should fail")
	}
}

func TestChainFollowsRetryAttempts(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// attempt 1 fails, attempt 2 is scheduled, attempt 3 succeeds.
	first := sampleRun("run-1", StatusFailed, 1)
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := sampleRun("run-2", StatusRetrying, 2)
	second.ParentRunID = &first.ID
	if err := store.Create(ctx, second); err != nil {
		t.Fatal(err)
	}

	third := sampleRun("run-3", StatusSucceeded, 3)
	third.ParentRunID = &second.ID
	if err := store.Create(ctx, third); err != nil {
		t.Fatal(err)
	}

	// Looking up any attempt yields the whole chain, in order.
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		root, attempts, err := store.Chain(ctx, id)
		if err != nil {
			t.Fatalf("chain %s: %v", id, err)
		}
		if root.ID != "run-1" {
			t.Errorf("chain(%s) root = %s, want run-1", id, root.ID)
		}
		if len(attempts) != 3 {
			t.Fatalf("chain(%s) has %d attempts, want 3", id, len(attempts))
		}
		for i, attempt := range attempts {
			if attempt.Attempt != i+1 {
				t.Errorf("attempts[%d].Attempt = %d, want %d", i, attempt.Attempt, i+1)
			}
		}
	}

	// A run with no parent is its own root.
	_, attempts, err := store.Chain(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if attempts[0].ParentRunID != nil {
		t.Error("the root attempt should have no parent")
	}

	if _, _, err := store.Chain(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("chain of a missing run = %v, want ErrNotFound", err)
	}
}

func TestListFiltersOrderingAndPaging(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 5; i++ {
		run := sampleRun("run-"+string(rune('0'+i)), StatusQueued, 1)
		run.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if i%2 == 0 {
			run.IntegrationID = "netsuite"
		}
		if err := store.Create(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("list returned %d runs, want 5", len(all))
	}
	// Newest first by default.
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Error("default ordering should be newest first")
		}
	}

	ascending, err := store.List(ctx, Filter{Ascending: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(ascending); i++ {
		if ascending[i].CreatedAt.Before(ascending[i-1].CreatedAt) {
			t.Error("ascending ordering should be oldest first")
		}
	}

	// Runs 2 and 4 belong to netsuite, leaving 1, 3 and 5 on shopify.
	shopify, err := store.List(ctx, Filter{IntegrationID: "shopify"})
	if err != nil {
		t.Fatal(err)
	}
	if len(shopify) != 3 {
		t.Errorf("shopify runs = %d, want 3", len(shopify))
	}

	queued, err := store.List(ctx, Filter{Status: StatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 5 {
		t.Errorf("queued runs = %d, want 5", len(queued))
	}
	none, err := store.List(ctx, Filter{Status: StatusSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("succeeded runs = %d, want 0", len(none))
	}

	page, err := store.List(ctx, Filter{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("page size = %d, want 2", len(page))
	}
	if page[0].ID != all[1].ID {
		t.Errorf("offset paging returned %s, want %s", page[0].ID, all[1].ID)
	}

	byParent, err := store.List(ctx, Filter{ParentRunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byParent) != 0 {
		t.Errorf("children of run-1 = %d, want 0", len(byParent))
	}
}

func TestCountByStatusAndListByStatus(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	for i, status := range []Status{StatusQueued, StatusQueued, StatusRunning, StatusSucceeded} {
		if err := store.Create(ctx, sampleRun("run-"+string(rune('a'+i)), status, 1)); err != nil {
			t.Fatal(err)
		}
	}

	counts, err := store.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("count by status: %v", err)
	}
	if counts[StatusQueued] != 2 || counts[StatusRunning] != 1 || counts[StatusSucceeded] != 1 {
		t.Errorf("counts = %v", counts)
	}

	queued, err := store.ListByStatus(ctx, StatusQueued, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 {
		t.Errorf("queued = %d, want 2", len(queued))
	}
}

func TestSetStatus(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.Create(ctx, sampleRun("run-1", StatusQueued, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus(ctx, "run-1", StatusCancelled, "cancelled by operator"); err != nil {
		t.Fatalf("set status: %v", err)
	}

	run, _ := store.Get(ctx, "run-1")
	if run.Status != StatusCancelled || run.ErrorString() != "cancelled by operator" {
		t.Errorf("run = %+v", run)
	}

	if err := store.SetStatus(ctx, "run-1", Status("bogus"), ""); err == nil {
		t.Error("setting an unknown status should fail")
	}
}

func TestLogStoreAppendAndList(t *testing.T) {
	_, logs := newTestStore(t)
	ctx := context.Background()

	entries := []LogEntry{
		{RunID: "run-1", Stream: StreamStdout, Message: "first"},
		{RunID: "run-1", Stream: StreamStderr, Message: "second"},
		{RunID: "run-1", Stream: StreamOtter, Message: "third"},
		{RunID: "run-2", Stream: StreamStdout, Message: "other run"},
	}
	if err := logs.AppendBatch(ctx, entries); err != nil {
		t.Fatalf("append batch: %v", err)
	}

	got, err := logs.List(ctx, "run-1", 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("run-1 has %d log lines, want 3", len(got))
	}
	if got[0].Message != "first" || got[1].Message != "second" || got[2].Message != "third" {
		t.Errorf("ordering = %v", []string{got[0].Message, got[1].Message, got[2].Message})
	}
	if got[0].ID == 0 || got[0].Timestamp.IsZero() {
		t.Error("log entries should carry an id and a timestamp")
	}

	// after_id makes polling incremental.
	after, err := logs.List(ctx, "run-1", got[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].Message != "second" {
		t.Errorf("after_id list = %v", after)
	}

	other, err := logs.List(ctx, "run-2", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 {
		t.Errorf("run-2 log lines = %d, want 1", len(other))
	}

	if err := logs.AppendBatch(ctx, nil); err != nil {
		t.Errorf("an empty batch should be a no-op, got %v", err)
	}
}

func TestLogStoreDeleteAndPrune(t *testing.T) {
	_, logs := newTestStore(t)
	ctx := context.Background()

	old := time.Now().UTC().Add(-48 * time.Hour)
	recent := time.Now().UTC()

	if err := logs.AppendBatch(ctx, []LogEntry{
		{RunID: "run-1", Stream: StreamStdout, Message: "old", Timestamp: old},
		{RunID: "run-1", Stream: StreamStdout, Message: "new", Timestamp: recent},
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := logs.DeleteOlderThan(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("pruned %d lines, want 1", removed)
	}

	remaining, err := logs.List(ctx, "run-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Message != "new" {
		t.Errorf("remaining = %+v", remaining)
	}

	deleted, err := logs.DeleteForRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("delete for run: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d lines, want 1", deleted)
	}
	empty, err := logs.List(ctx, "run-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("remaining after delete = %d, want 0", len(empty))
	}
}

// guard against an accidental signature change in the stores.
var (
	_ func(context.Context, string) (*Run, error) = (&Store{}).Get
	_ func(context.Context, *sql.Tx, *Run) error  = (&Store{}).CreateTx
)
