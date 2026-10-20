package inspection

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewExporterDefaultsToNoop(t *testing.T) {
	for _, mode := range []string{"", "none", "NONE", " none "} {
		exporter, err := NewExporter(mode)
		if err != nil {
			t.Fatalf("NewExporter(%q): %v", mode, err)
		}
		if _, ok := exporter.(NoopExporter); !ok {
			t.Fatalf("NewExporter(%q) = %T, want NoopExporter", mode, exporter)
		}
	}
}

func TestNewExporterRefusesAModeThisBuildDoesNotImplement(t *testing.T) {
	for _, mode := range []string{"sink", "everywhere"} {
		if _, err := NewExporter(mode); err == nil {
			t.Fatalf("NewExporter(%q) accepted a mode with no implementation", mode)
		}
	}
}

func TestNewExporterSelectsTheOutbox(t *testing.T) {
	exporter, err := NewExporter("outbox")
	if err != nil {
		t.Fatalf("NewExporter(outbox): %v", err)
	}
	if _, ok := exporter.(OutboxExporter); !ok {
		t.Fatalf("NewExporter(outbox) = %T, want OutboxExporter", exporter)
	}
}

func TestNoopExporterNeverTouchesTheTransaction(t *testing.T) {
	// A nil DBTX would panic if the no-op tried to use it. That is the point:
	// "none" must be a seam that changes no behaviour, not a best-effort write
	// that happens to do nothing.
	if err := (NoopExporter{}).Export(context.Background(), nil, []ExportRecord{
		{Kind: ExportExchange, RunID: "run-a", RequestID: "req-1"},
	}); err != nil {
		t.Fatalf("no-op export: %v", err)
	}
}

func TestStoreDefaultsToTheNoopExporter(t *testing.T) {
	store := NewStore(nil, nil, Limits{})
	if _, ok := store.exporter.(NoopExporter); !ok {
		t.Fatalf("default exporter = %T, want NoopExporter", store.exporter)
	}
	// A nil exporter is a no-op, never a nil pointer the ingest path could call.
	store.WithExporter(nil)
	if _, ok := store.exporter.(NoopExporter); !ok {
		t.Fatalf("WithExporter(nil) = %T, want NoopExporter", store.exporter)
	}
}

func TestParseExportModeIsExplicit(t *testing.T) {
	got, err := ParseExportMode("")
	if err != nil || got != ExportModeNone {
		t.Fatalf("ParseExportMode(\"\") = %q, %v; want none, nil", got, err)
	}
	if got, err := ParseExportMode("outbox"); err != nil || got != ExportModeOutbox {
		t.Fatalf("ParseExportMode(outbox) = %q, %v; want outbox, nil", got, err)
	}
}

type exportRow struct {
	runID     string
	kind      string
	requestID string
	state     string
	attempts  int
}

func exportRows(t *testing.T, store *Store) []exportRow {
	t.Helper()
	rows, err := store.db.Query(
		`SELECT run_id, kind, request_id, state, attempts FROM capture_exports ORDER BY kind, request_id`)
	if err != nil {
		t.Fatalf("query capture_exports: %v", err)
	}
	defer rows.Close()

	var out []exportRow
	for rows.Next() {
		var row exportRow
		if err := rows.Scan(&row.runID, &row.kind, &row.requestID, &row.state, &row.attempts); err != nil {
			t.Fatalf("scan capture_exports: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read capture_exports: %v", err)
	}
	return out
}

func completedBatch(requestIDs ...string) EventBatch {
	var events []RequestEvent
	for index, id := range requestIDs {
		events = append(events, startedEvent(id, int64(index*2+1)))
		events = append(events, completedEvent(id, int64(index*2+2), 200))
	}
	return EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyFull, Events: events}
}

func TestIngestQueuesPerExchangeAndManifest(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(OutboxExporter{})
	beginTestCapture(t, store, "run-1", PolicyFull)

	result, err := store.Ingest(ctx, "run-1", completedBatch("req-1"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.ExportError != "" {
		t.Fatalf("export error on a healthy outbox: %s", result.ExportError)
	}
	rows := exportRows(t, store)
	if len(rows) != 1 || rows[0].kind != string(ExportExchange) || rows[0].requestID != "req-1" {
		t.Fatalf("after the exchange: %+v", rows)
	}
	if rows[0].state != "pending" {
		t.Errorf("exchange state = %q, want pending", rows[0].state)
	}

	result, err = store.Ingest(ctx, "run-1", EventBatch{
		SchemaVersion: SchemaVersion,
		Policy:        PolicyFull,
		Finalization:  FinalizationComplete,
	})
	if err != nil {
		t.Fatalf("finalizing Ingest: %v", err)
	}
	if result.ExportError != "" {
		t.Fatalf("export error at finalization: %s", result.ExportError)
	}
	rows = exportRows(t, store)
	if len(rows) != 2 {
		t.Fatalf("after finalization: %+v, want the exchange and the manifest", rows)
	}
	var manifest *exportRow
	for index := range rows {
		if rows[index].kind == string(ExportManifest) {
			manifest = &rows[index]
		}
	}
	if manifest == nil || manifest.requestID != "" {
		t.Fatalf("manifest row = %+v", manifest)
	}
}

type failingExporter struct{ err error }

func (f failingExporter) Export(context.Context, DBTX, []ExportRecord) error { return f.err }

func TestAFailedExporterNeverFailsIngest(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(failingExporter{err: errors.New("sink down")})
	beginTestCapture(t, store, "run-1", PolicyFull)

	batch := completedBatch("req-1")
	batch.Finalization = FinalizationComplete
	result, err := store.Ingest(ctx, "run-1", batch)
	if err != nil {
		t.Fatalf("a failed exporter failed ingest: %v", err)
	}
	if !strings.Contains(result.ExportError, "sink down") {
		t.Fatalf("ExportError = %q, want the exporter's error", result.ExportError)
	}
	if len(exportRows(t, store)) != 0 {
		t.Fatal("the failing exporter still queued rows")
	}
	capture, err := store.Capture(ctx, "run-1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capture.State != CaptureComplete {
		t.Errorf("capture state = %q, want complete: the exchange itself must survive", capture.State)
	}
}

func TestOutboxIsBoundedAndIngestStillCommits(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(OutboxExporter{MaxRows: 1})
	beginTestCapture(t, store, "run-1", PolicyFull)

	batch := completedBatch("req-1", "req-2")
	batch.Finalization = FinalizationComplete
	result, err := store.Ingest(ctx, "run-1", batch)
	if err != nil {
		t.Fatalf("a full outbox failed ingest: %v", err)
	}
	if !strings.Contains(result.ExportError, "outbox is full") {
		t.Fatalf("ExportError = %q, want the outbox-full error", result.ExportError)
	}
	rows := exportRows(t, store)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want the bound of 1", len(rows))
	}
	if rows[0].requestID != "req-1" {
		t.Fatalf("the bounded outbox kept %q, want the first record", rows[0].requestID)
	}
}

func TestCaptureOffQueuesNothing(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(OutboxExporter{})
	beginTestCapture(t, store, "run-1", PolicyOff)

	result, err := store.Ingest(ctx, "run-1", EventBatch{
		SchemaVersion: SchemaVersion,
		Policy:        PolicyOff,
		Finalization:  FinalizationComplete,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.ExportError != "" {
		t.Fatalf("ExportError = %q", result.ExportError)
	}
	if len(exportRows(t, store)) != 0 {
		t.Fatal("a capture:off run queued an export")
	}
}

func TestMetadataRunQueuesItsSummaryExports(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(OutboxExporter{})
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	batch := EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata, Events: []RequestEvent{
		startedEvent("req-1", 1),
		completedEvent("req-1", 2, 401),
	}, Finalization: FinalizationComplete}
	if _, err := store.Ingest(ctx, "run-1", batch); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	rows := exportRows(t, store)
	if len(rows) != 2 {
		t.Fatalf("metadata run rows = %+v, want an exchange summary and a manifest", rows)
	}
}

func TestOutboxExporterIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	records := []ExportRecord{{Kind: ExportExchange, RunID: "run-1", RequestID: "req-1"}}
	exporter := OutboxExporter{}
	if err := exporter.Export(ctx, tx, records); err != nil {
		t.Fatalf("first Export: %v", err)
	}
	if err := exporter.Export(ctx, tx, records); err != nil {
		t.Fatalf("second Export: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if rows := exportRows(t, store); len(rows) != 1 {
		t.Fatalf("rows = %d, want one: the outbox must be idempotent", len(rows))
	}
}
