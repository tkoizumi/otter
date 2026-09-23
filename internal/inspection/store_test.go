package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

func newTestStore(t *testing.T, limits Limits) *Store {
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
	if limits.MaxBodyBytes == 0 {
		limits = DefaultLimits()
	}
	return NewStore(db.DB, DefaultRedactor(), limits)
}

func beginTestCapture(t *testing.T, store *Store, runID string, policy Policy) {
	t.Helper()
	if err := store.Begin(context.Background(), CaptureSettings{
		RunID:         runID,
		IntegrationID: "int-1",
		Policy:        policy,
	}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
}

func startedEvent(requestID string, seq int64) RequestEvent {
	return RequestEvent{
		Kind:        EventStarted,
		RequestID:   requestID,
		ProducerSeq: seq,
		OccurredAt:  time.Now().UTC(),
		Method:      "POST",
		URL:         "https://api.example.com/v1/items?access_token=secret",
		CallSite:    "source.py:42 in push",
	}
}

func completedEvent(requestID string, seq int64, status int) RequestEvent {
	return RequestEvent{
		Kind:            EventCompleted,
		RequestID:       requestID,
		ProducerSeq:     seq,
		OccurredAt:      time.Now().UTC(),
		StatusCode:      &status,
		DurationTotalMS: int64PtrValue(12),
	}
}

func int64PtrValue(v int64) *int64 { return &v }
func intPtrValue(v int) *int       { return &v }

func TestBeginIsIdempotentAndCaptureUnavailableWithoutIt(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})

	if _, err := store.Capture(ctx, "missing-run"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Capture on an unconfigured run = %v, want ErrNotConfigured", err)
	}

	beginTestCapture(t, store, "run-1", PolicyMetadata)
	// A second submission of the same run must not disturb the first row.
	beginTestCapture(t, store, "run-1", PolicyFull)

	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.Policy != PolicyMetadata {
		t.Errorf("policy = %q, want the first recorded policy %q", capture.Policy, PolicyMetadata)
	}
	if capture.State != CapturePending {
		t.Errorf("state = %q, want %q", capture.State, CapturePending)
	}
	if len(capture.Adapters) != 1 || capture.Adapters[0] != AdapterURLLib {
		t.Errorf("adapters = %v, want [%s]", capture.Adapters, AdapterURLLib)
	}
	if capture.SchemaVersion != SchemaVersion || capture.PolicyVersion != PolicyVersion {
		t.Errorf("versions = %d/%d, want %d/%d",
			capture.SchemaVersion, capture.PolicyVersion, SchemaVersion, PolicyVersion)
	}
}

func TestIngestLifecycleMergesAndCounts(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyFull)

	start := startedEvent("req-1", 1)
	start.RequestHeaders = []HeaderPair{{Name: "Authorization", Value: "Bearer abc"}}
	start.RequestBody = &BodyDescriptor{
		State:       BodyCaptured,
		ContentType: "application/json",
		JSON:        json.RawMessage(`{"password":"hunter2","name":"ok"}`),
	}
	batch := EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyFull, Events: []RequestEvent{start}}
	result, err := store.Ingest(ctx, "run-1", batch)
	if err != nil {
		t.Fatalf("Ingest start: %v", err)
	}
	if result.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", result.Accepted)
	}

	// Response headers arrive next, then the terminal event.
	response := RequestEvent{
		Kind:                EventResponse,
		RequestID:           "req-1",
		ProducerSeq:         2,
		OccurredAt:          time.Now().UTC(),
		DurationToHeadersMS: int64PtrValue(7),
		ResponseHeaders:     []HeaderPair{{Name: "Content-Type", Value: "application/json"}},
		ResponseBody:        &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(`{"error":"bad","token":"t"}`)},
	}
	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyFull, Events: []RequestEvent{response}}); err != nil {
		t.Fatalf("Ingest response: %v", err)
	}
	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyFull, Events: []RequestEvent{completedEvent("req-1", 3, 400)}}); err != nil {
		t.Fatalf("Ingest completion: %v", err)
	}

	exchange, err := store.Get(ctx, "run-1", "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if exchange.Phase != PhaseCompleted || !exchange.Complete {
		t.Errorf("phase/complete = %q/%v, want completed/true", exchange.Phase, exchange.Complete)
	}
	if exchange.StatusCode == nil || *exchange.StatusCode != 400 {
		t.Errorf("status = %v, want 400", exchange.StatusCode)
	}
	if exchange.Method != "POST" {
		t.Errorf("method = %q, want POST (merged from the start event)", exchange.Method)
	}
	if exchange.DurationToHeadersMS == nil || *exchange.DurationToHeadersMS != 7 {
		t.Errorf("time to headers = %v, want 7", exchange.DurationToHeadersMS)
	}
	if exchange.IntegrationID != "int-1" {
		t.Errorf("integration = %q, want the run's integration, not a client value", exchange.IntegrationID)
	}
	if exchange.Payloads != "full" {
		t.Errorf("payloads = %q, want full", exchange.Payloads)
	}
	// Redaction must have survived the round trip.
	if got := string(exchange.RequestBody.JSON); got != `{"password":"REDACTED","name":"ok"}` {
		t.Errorf("request body = %s, want the password redacted", got)
	}
	if got := string(exchange.ResponseBody.JSON); got != `{"error":"bad","token":"REDACTED"}` {
		t.Errorf("response body = %s, want the token redacted", got)
	}
	if exchange.RequestHeaders[0].Value != RedactedPlaceholder {
		t.Errorf("authorization header = %q, want redacted", exchange.RequestHeaders[0].Value)
	}
	if exchange.URL != "https://api.example.com/v1/items?access_token=REDACTED" {
		t.Errorf("url = %q, want the token redacted", exchange.URL)
	}

	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.RequestCount != 1 || capture.CompletedCount != 1 || capture.IncompleteCount != 0 {
		t.Errorf("counts = %d/%d/%d, want 1 request, 1 completed, 0 incomplete",
			capture.RequestCount, capture.CompletedCount, capture.IncompleteCount)
	}
	if capture.RedactionCount < 3 {
		t.Errorf("redaction count = %d, want at least 3", capture.RedactionCount)
	}
	if capture.LastSequence != 3 {
		t.Errorf("last sequence = %d, want 3", capture.LastSequence)
	}
	if capture.StoredBytes <= 0 {
		t.Errorf("stored bytes = %d, want a positive count", capture.StoredBytes)
	}
}

func TestIngestIsIdempotentAndRejectsStaleUpdates(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	batch := EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{startedEvent("req-1", 5)}}
	if _, err := store.Ingest(ctx, "run-1", batch); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// Re-delivering the same batch must not duplicate the record.
	result, err := store.Ingest(ctx, "run-1", batch)
	if err != nil {
		t.Fatalf("Ingest duplicate: %v", err)
	}
	if result.Duplicates != 1 || result.Accepted != 0 {
		t.Errorf("duplicate result = %+v, want 1 duplicate and 0 accepted", result)
	}

	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{completedEvent("req-1", 6, 200)}}); err != nil {
		t.Fatalf("Ingest completion: %v", err)
	}

	// A late start event must not rewrite the finalized exchange.
	stale := startedEvent("req-1", 4)
	stale.Method = "GET"
	result, err = store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{stale}})
	if err != nil {
		t.Fatalf("Ingest stale: %v", err)
	}
	if result.Stale != 1 {
		t.Errorf("stale = %d, want 1", result.Stale)
	}
	exchange, err := store.Get(ctx, "run-1", "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if exchange.Method != "POST" {
		t.Errorf("method = %q, want the finalized POST to be immutable", exchange.Method)
	}

	// Even a newer terminal event cannot change a finalized exchange.
	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{completedEvent("req-1", 7, 500)}}); err != nil {
		t.Fatalf("Ingest after finalize: %v", err)
	}
	exchange, err = store.Get(ctx, "run-1", "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if exchange.StatusCode == nil || *exchange.StatusCode != 200 {
		t.Errorf("status = %v, want the finalized 200 to be immutable", exchange.StatusCode)
	}
}

func TestIngestRejectsPolicyMismatchAndUnconfiguredRuns(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	_, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyFull,
		Events: []RequestEvent{startedEvent("req-1", 1)}})
	if err == nil {
		t.Fatal("a batch claiming a different policy must be rejected")
	}
	_, err = store.Ingest(ctx, "run-2", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{startedEvent("req-1", 1)}})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Ingest on an unconfigured run = %v, want ErrNotConfigured", err)
	}
}

func TestQuotasDropCaptureWithoutFailing(t *testing.T) {
	ctx := context.Background()
	limits := DefaultLimits()
	limits.MaxRunRequests = 1
	limits.MaxRunBytes = 1 << 20
	store := newTestStore(t, limits)
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{startedEvent("req-1", 1)}}); err != nil {
		t.Fatalf("Ingest first: %v", err)
	}
	result, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{startedEvent("req-2", 2)}})
	if err != nil {
		t.Fatalf("a quota rejection must not be an error: %v", err)
	}
	if result.QuotaRejected != 1 || result.Accepted != 0 {
		t.Errorf("quota result = %+v, want 1 rejected and 0 accepted", result)
	}
	if _, err := store.Get(ctx, "run-1", "req-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("over-quota exchange was stored anyway: %v", err)
	}
	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.RequestCount != 1 {
		t.Errorf("request count = %d, want 1", capture.RequestCount)
	}
}

func TestListPaginatesWithoutPayloads(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	for i, id := range []string{"req-a", "req-b", "req-c"} {
		if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
			Events: []RequestEvent{completedEvent(id, int64(i+1), 200)}}); err != nil {
			t.Fatalf("Ingest %s: %v", id, err)
		}
	}

	first, err := store.List(ctx, "run-1", 0, 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first page = %d rows, want 2", len(first))
	}
	if first[0].RequestID != "req-a" || first[1].RequestID != "req-b" {
		t.Errorf("first page order = %s,%s, want req-a,req-b", first[0].RequestID, first[1].RequestID)
	}
	if first[0].Completeness() != "metadata" {
		t.Errorf("completeness = %q, want metadata for a metadata run", first[0].Completeness())
	}

	second, err := store.List(ctx, "run-1", first[len(first)-1].ID, 2)
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(second) != 1 || second[0].RequestID != "req-c" {
		t.Errorf("second page = %+v, want only req-c", second)
	}
}

func TestFinalizeAndStateSemantics(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{startedEvent("req-1", 1)}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// An SDK that lost capture says so; the summary must reflect the loss.
	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		DroppedEvents: 2, DroppedBytes: 300}); err != nil {
		t.Fatalf("Ingest summary: %v", err)
	}
	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.DroppedEvents != 2 || capture.DroppedBytes != 300 {
		t.Errorf("dropped = %d/%d, want 2/300", capture.DroppedEvents, capture.DroppedBytes)
	}

	if err := store.Finalize(ctx, "run-1", FinalizationComplete, ""); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	capture, err = store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.Finalization != FinalizationComplete {
		t.Errorf("finalization = %q, want complete", capture.Finalization)
	}
	if capture.IncompleteCount != 1 {
		t.Errorf("incomplete count = %d, want 1 for the started-only exchange", capture.IncompleteCount)
	}
	// Known loss outranks a later clean claim, and an in-flight request means the
	// state is incomplete rather than complete.
	if capture.State != CaptureIncomplete {
		t.Errorf("state = %q, want incomplete", capture.State)
	}
}

func TestFinalizationNeverDowngradesKnownLoss(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	if err := store.Finalize(ctx, "run-1", FinalizationAbrupt, "killed by signal"); err != nil {
		t.Fatalf("Finalize abrupt: %v", err)
	}
	if err := store.Finalize(ctx, "run-1", FinalizationComplete, ""); err != nil {
		t.Fatalf("Finalize complete: %v", err)
	}
	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.Finalization != FinalizationAbrupt {
		t.Errorf("finalization = %q, want abrupt to survive a later clean claim", capture.Finalization)
	}
	if capture.Note != "killed by signal" {
		t.Errorf("note = %q, want the original note", capture.Note)
	}
}

func TestRetentionExpiresPayloadsButKeepsTheSummary(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)
	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{completedEvent("req-1", 1, 200)}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	runs, deleted, err := store.ExpireOlderThan(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ExpireOlderThan: %v", err)
	}
	if runs != 1 || deleted != 1 {
		t.Fatalf("expired runs/deleted = %d/%d, want 1/1", runs, deleted)
	}

	if _, err := store.Get(ctx, "run-1", "req-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired payload is still readable: %v", err)
	}
	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !capture.PayloadsExpired || capture.ExpiredAt == nil {
		t.Errorf("summary does not record expiry: %+v", capture)
	}
	if capture.State != CaptureExpired {
		t.Errorf("state = %q, want expired", capture.State)
	}
	// The count survives, so an expired recording is not mistaken for an empty one.
	if capture.RequestCount != 1 {
		t.Errorf("request count = %d, want 1 to survive retention", capture.RequestCount)
	}

	// A second pass must not re-report the same run.
	runs, _, err = store.ExpireOlderThan(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ExpireOlderThan second pass: %v", err)
	}
	if runs != 0 {
		t.Errorf("second pass expired %d runs, want 0", runs)
	}
}

func TestDeleteForRunAndIntegration(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})

	beginTestCapture(t, store, "run-1", PolicyMetadata)
	beginTestCapture(t, store, "run-2", PolicyMetadata)
	for _, run := range []string{"run-1", "run-2"} {
		if _, err := store.Ingest(ctx, run, EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
			Events: []RequestEvent{completedEvent("req-"+run, 1, 200)}}); err != nil {
			t.Fatalf("Ingest %s: %v", run, err)
		}
	}

	if _, err := store.DeleteForRun(ctx, "run-1"); err != nil {
		t.Fatalf("DeleteForRun: %v", err)
	}
	if _, err := store.Capture(ctx, "run-1"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("run-1 summary survived DeleteForRun: %v", err)
	}

	if _, err := store.DeleteForIntegration(ctx, "int-1"); err != nil {
		t.Fatalf("DeleteForIntegration: %v", err)
	}
	if _, err := store.Capture(ctx, "run-2"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("run-2 summary survived DeleteForIntegration: %v", err)
	}
}

func TestMarkStalePendingMarksAbrupt(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)
	if _, err := store.Ingest(ctx, "run-1", EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
		Events: []RequestEvent{startedEvent("req-1", 1)}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	affected, err := store.MarkStalePending(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("MarkStalePending: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}
	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.Finalization != FinalizationAbrupt {
		t.Errorf("finalization = %q, want abrupt", capture.Finalization)
	}
	if capture.IncompleteCount != 1 {
		t.Errorf("incomplete count = %d, want 1", capture.IncompleteCount)
	}
}

// TestMigrationIsRepeatable proves the new migration applies cleanly and that a
// second Migrate is a no-op, which is what an upgrade of an existing database
// does.
func TestMigrationIsRepeatable(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate must be a no-op: %v", err)
	}

	var applied int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied < 4 {
		t.Errorf("applied migrations = %d, want at least 4", applied)
	}
	for _, table := range []string{"run_capture", "http_exchanges"} {
		var name string
		err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s is missing after migration: %v", table, err)
		}
	}
}

func TestFindByRequestIDSpansRunsAndReturnsEveryMatch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})

	for _, runID := range []string{"run-a", "run-b"} {
		beginTestCapture(t, store, runID, PolicyMetadata)
		if _, err := store.Ingest(ctx, runID, EventBatch{
			SchemaVersion: SchemaVersion, Policy: PolicyMetadata,
			Events: []RequestEvent{startedEvent("req-shared", 1)},
		}); err != nil {
			t.Fatalf("Ingest %s: %v", runID, err)
		}
	}

	// The same request id in two runs is two rows, because uniqueness is only
	// enforced per run. That is exactly why the lookup returns all matches
	// instead of choosing one.
	matches, err := store.FindByRequestID(ctx, "req-shared")
	if err != nil {
		t.Fatalf("FindByRequestID: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want 2 (one per run)", len(matches))
	}
	seen := map[string]bool{}
	for _, match := range matches {
		seen[match.RunID] = true
		if match.RequestID != "req-shared" {
			t.Errorf("match = %+v, want the shared request id", match)
		}
	}
	if !seen["run-a"] || !seen["run-b"] {
		t.Errorf("matches did not cover both runs: %+v", seen)
	}

	// The run-scoped read still resolves exactly one row.
	one, err := store.Get(ctx, "run-a", "req-shared")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if one.RunID != "run-a" {
		t.Errorf("Get returned run %q, want run-a", one.RunID)
	}

	// An unknown id is an empty result, not an error.
	none, err := store.FindByRequestID(ctx, "req-absent")
	if err != nil {
		t.Fatalf("FindByRequestID(absent): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown id matched %d rows, want none", len(none))
	}
}
