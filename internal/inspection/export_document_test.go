package inspection

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPullExportsAssemblesTheFrozenDocuments(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(OutboxExporter{}).WithRuntimeID("rt_demo_01")
	beginTestCapture(t, store, "run-1", PolicyFull)

	started := startedEvent("req-1", 1)
	started.RequestHeaders = []HeaderPair{
		{Name: "authorization", Value: "{{secret:SHOPIFY_TOKEN}}"},
	}
	completed := completedEvent("req-1", 2, 200)
	completed.ResponseBody = &BodyDescriptor{
		State:       BodyCaptured,
		ContentType: "application/json",
		JSON:        json.RawMessage(`{"ok":true}`),
	}
	if _, err := store.Ingest(ctx, "run-1", EventBatch{
		SchemaVersion: SchemaVersion,
		Policy:        PolicyFull,
		Events:        []RequestEvent{started, completed},
		Finalization:  FinalizationComplete,
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	pulled, err := store.PullExports(ctx, 0, 10)
	if err != nil {
		t.Fatalf("PullExports: %v", err)
	}
	if len(pulled.Documents) != 2 {
		t.Fatalf("documents = %d, want an exchange and a manifest", len(pulled.Documents))
	}
	if pulled.Cursor == 0 {
		t.Fatal("cursor was not advanced")
	}

	var exchange map[string]any
	if err := json.Unmarshal(pulled.Documents[0].Document, &exchange); err != nil {
		t.Fatalf("exchange document is not JSON: %v", err)
	}
	if exchange["schema_version"] != float64(1) || exchange["kind"] != "capture" {
		t.Fatalf("exchange header = %v", exchange)
	}
	if exchange["runtime_id"] != "rt_demo_01" || exchange["payloads"] != "full" || exchange["complete"] != true {
		t.Fatalf("exchange identity = %v", exchange)
	}
	request := exchange["request"].(map[string]any)
	if request["query_redacted"] != true {
		t.Fatal("a URL with access_token was not marked query_redacted")
	}
	headers, ok := request["headers"].([]any)
	if !ok || len(headers) != 1 {
		t.Fatalf("request headers = %v, want the placeholder only", request["headers"])
	}
	if headers[0].(map[string]any)["value"] != "{{secret:SHOPIFY_TOKEN}}" {
		t.Fatalf("header = %v, want the placeholder", headers[0])
	}
	response := exchange["response"].(map[string]any)
	if response["status_code"] != float64(200) {
		t.Fatalf("status = %v", response["status_code"])
	}

	var manifest map[string]any
	if err := json.Unmarshal(pulled.Documents[1].Document, &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if manifest["capture_policy"] != "full" || manifest["finalized"] != true {
		t.Fatalf("manifest = %v", manifest)
	}
	requests := manifest["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("manifest requests = %v", requests)
	}
	summary := requests[0].(map[string]any)
	if summary["query_redacted"] != true || summary["request_id"] != "req-1" {
		t.Fatalf("manifest summary = %v", summary)
	}
}

func TestPullExportsMetadataDocumentHasNoHeadersOrBodies(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(OutboxExporter{}).WithRuntimeID("rt_demo_01")
	beginTestCapture(t, store, "run-1", PolicyMetadata)

	if _, err := store.Ingest(ctx, "run-1", EventBatch{
		SchemaVersion: SchemaVersion,
		Policy:        PolicyMetadata,
		Events:        []RequestEvent{startedEvent("req-1", 1), completedEvent("req-1", 2, 401)},
		Finalization:  FinalizationComplete,
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	pulled, err := store.PullExports(ctx, 0, 10)
	if err != nil {
		t.Fatalf("PullExports: %v", err)
	}
	var exchange map[string]any
	if err := json.Unmarshal(pulled.Documents[0].Document, &exchange); err != nil {
		t.Fatalf("exchange document is not JSON: %v", err)
	}
	if exchange["payloads"] != "metadata" {
		t.Fatalf("payloads = %v", exchange["payloads"])
	}
	request := exchange["request"].(map[string]any)
	if _, ok := request["headers"]; ok {
		t.Fatalf("metadata document carries headers: %v", request["headers"])
	}
	if _, ok := request["body"]; ok {
		t.Fatalf("metadata document carries a request body: %v", request["body"])
	}
	response := exchange["response"].(map[string]any)
	if _, ok := response["body"]; ok {
		t.Fatalf("metadata document carries a response body: %v", response["body"])
	}
}

func TestPullExportsRequiresARuntimeID(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{})
	if _, err := store.PullExports(ctx, 0, 10); err == nil {
		t.Fatal("PullExports worked without a runtime id")
	}
}

func TestAckExportsRetriesAndThenAbandons(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, Limits{}).WithExporter(OutboxExporter{}).WithRuntimeID("rt_demo_01")
	beginTestCapture(t, store, "run-1", PolicyFull)
	if _, err := store.Ingest(ctx, "run-1", completedBatch("req-1")); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	pulled, err := store.PullExports(ctx, 0, 10)
	if err != nil {
		t.Fatalf("PullExports: %v", err)
	}
	refs := []ExportRef{{RunID: "run-1", Kind: ExportExchange, RequestID: "req-1"}}

	if err := store.AckExports(ctx, refs, ExportAckRetry, "cloud down"); err != nil {
		t.Fatalf("AckExports retry: %v", err)
	}
	var state, lastError string
	var attempts int
	if err := store.db.QueryRow(
		`SELECT state, attempts, last_error FROM capture_exports WHERE run_id = 'run-1' AND kind = 'exchange'`).
		Scan(&state, &attempts, &lastError); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if state != "pending" || attempts != 1 || !strings.Contains(lastError, "cloud down") {
		t.Fatalf("after retry: state=%q attempts=%d last_error=%q", state, attempts, lastError)
	}
	again, err := store.PullExports(ctx, 0, 10)
	if err != nil {
		t.Fatalf("second PullExports: %v", err)
	}
	if len(again.Documents) != 0 {
		t.Fatal("a retry row was pulled before its backoff elapsed")
	}

	if _, err := store.db.Exec(
		`UPDATE capture_exports SET attempts = ? WHERE run_id = 'run-1' AND kind = 'exchange'`,
		maxExportAttempts-1); err != nil {
		t.Fatalf("seed attempts: %v", err)
	}
	if err := store.AckExports(ctx, refs, ExportAckRetry, "still down"); err != nil {
		t.Fatalf("AckExports final retry: %v", err)
	}
	if err := store.db.QueryRow(
		`SELECT state FROM capture_exports WHERE run_id = 'run-1' AND kind = 'exchange'`).
		Scan(&state); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if state != "abandoned" {
		t.Fatalf("state = %q, want abandoned at the attempt limit", state)
	}
	_ = pulled
}
