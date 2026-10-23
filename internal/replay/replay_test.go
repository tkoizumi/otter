package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/secrets"
)

func testStore(t *testing.T) *inspection.Store {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}
	return inspection.NewStore(db.DB, inspection.DefaultRedactor(), inspection.DefaultLimits()).
		WithRuntimeID("rt-test")
}

func exchange(url string) *inspection.Exchange {
	return &inspection.Exchange{
		RunID:         "run-a",
		RequestID:     "req-1",
		JobID:         "job-a",
		OccurredAt:    time.Now().UTC(),
		Complete:      true,
		Method:        "GET",
		URL:           url,
		QueryRedacted: false,
		Payloads:      "full",
		RequestHeaders: []inspection.HeaderPair{
			{Name: "Authorization", Value: "{{secret:TOKEN}}"},
			{Name: "Accept", Value: "application/json"},
		},
	}
}

func TestCheckRefusesWhatCannotBeReplayedExactly(t *testing.T) {
	base := "https://api.example.com/v1/items"
	tests := []struct {
		name   string
		mutate func(*inspection.Exchange)
		want   string
	}{
		{"POST is not in v1", func(e *inspection.Exchange) { e.Method = "POST" }, "GET-only"},
		{"a redacted query is gone", func(e *inspection.Exchange) { e.QueryRedacted = true }, "URL was redacted"},
		{"metadata has no body", func(e *inspection.Exchange) { e.Payloads = "metadata" }, "not captured"},
		{"an incomplete exchange is not a template", func(e *inspection.Exchange) { e.Complete = false }, "not captured whole"},
		{"http is refused", func(e *inspection.Exchange) { e.URL = "http://api.example.com/x" }, "HTTPS-only"},
		{"userinfo is refused", func(e *inspection.Exchange) { e.URL = "https://user:pass@api.example.com/x" }, "embeds credentials"},
		{"a redacted header cannot be reconstructed", func(e *inspection.Exchange) {
			e.RequestHeaders = []inspection.HeaderPair{{Name: "Authorization", Value: inspection.RedactedPlaceholder}}
		}, "was redacted"},
		{"sigv4 is request-bound", func(e *inspection.Exchange) {
			e.RequestHeaders = []inspection.HeaderPair{{Name: "Authorization", Value: "AWS4-HMAC-SHA256 Credential=x"}}
		}, "bound to the original request"},
		{"an omitted request body with bytes is a gap", func(e *inspection.Exchange) {
			e.RequestBody = &inspection.BodyDescriptor{State: inspection.BodyOmitted, Reason: inspection.ReasonOversized, BytesObserved: 12}
		}, "not captured"},
		{"a redacted body cannot be reconstructed", func(e *inspection.Exchange) {
			e.RequestBody = &inspection.BodyDescriptor{State: inspection.BodyCaptured, JSON: json.RawMessage(`{"token":"REDACTED"}`), Redacted: true}
		}, "body was redacted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := exchange(base)
			tc.mutate(item)
			err := Check(item)
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("Check() = %v, want a refusal", err)
			}
			if !strings.Contains(refusal.Reason, tc.want) {
				t.Fatalf("refusal %q does not mention %q", refusal.Reason, tc.want)
			}
		})
	}
}

func TestCheckAllowsAPlainGet(t *testing.T) {
	if err := Check(exchange("https://api.example.com/v1/items")); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

func TestReplayResolvesTheTemplateAndRecordsTheResult(t *testing.T) {
	store := testStore(t)
	var seenAuth, seenAccept, seenMethod string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAccept = r.Header.Get("Accept")
		seenMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"echo":"`+r.URL.Query().Get("q")+`"}`)
	}))
	defer server.Close()

	exchange := exchange(server.URL + "/v1/items?q=hello")
	engine := &Engine{
		Store:   store,
		Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": "t0ken"}),
		Client:  server.Client(),
	}
	result, err := engine.Replay(context.Background(), exchange, "job-a")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.ReplayID == "" || !strings.HasPrefix(result.ReplayID, "replay_") {
		t.Fatalf("ReplayID = %q", result.ReplayID)
	}
	if seenMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", seenMethod)
	}
	if seenAuth != "t0ken" {
		t.Fatalf("Authorization = %q, want the resolved secret", seenAuth)
	}
	if seenAccept != "application/json" {
		t.Fatalf("Accept = %q", seenAccept)
	}
	if result.StatusCode == nil || *result.StatusCode != 200 {
		t.Fatalf("StatusCode = %v, want 200", result.StatusCode)
	}
	if result.ResponseBody == nil || result.ResponseBody.State != inspection.BodyCaptured {
		t.Fatalf("ResponseBody = %#v, want captured", result.ResponseBody)
	}
	if !strings.Contains(string(result.ResponseBody.JSON), `"echo":"hello"`) {
		t.Fatalf("response body = %s", result.ResponseBody.JSON)
	}
	if len(result.ResolvedSHA256) != 64 {
		t.Fatalf("ResolvedSHA256 = %q, want 64 hex characters", result.ResolvedSHA256)
	}

	stored, err := store.GetReplay(context.Background(), "run-a", result.ReplayID)
	if err != nil {
		t.Fatalf("GetReplay: %v", err)
	}
	if stored.RequestID != "req-1" || stored.ResolvedSHA256 != result.ResolvedSHA256 {
		t.Fatalf("stored replay = %#v", stored)
	}
	if stored.RequestHeaders[0].Value != "{{secret:TOKEN}}" {
		t.Fatalf("stored template changed the placeholder: %q", stored.RequestHeaders[0].Value)
	}

	batch, err := store.PullExports(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("PullExports: %v", err)
	}
	if len(batch.Documents) != 1 || batch.Documents[0].Kind != inspection.ExportReplay {
		t.Fatalf("export batch = %#v", batch.Documents)
	}
	var document map[string]any
	if err := json.Unmarshal(batch.Documents[0].Document, &document); err != nil {
		t.Fatalf("replay document: %v", err)
	}
	if document["kind"] != "replay" || document["replay_of"] != "req-1" {
		t.Fatalf("replay document = %#v", document)
	}
	if document["resolved_sha256"] != result.ResolvedSHA256 {
		t.Fatalf("document digest = %v", document["resolved_sha256"])
	}
	headers := document["request"].(map[string]any)["headers"].([]any)
	if got := headers[0].(map[string]any)["value"]; got != "{{secret:TOKEN}}" {
		t.Fatalf("exported header value = %v, want the placeholder", got)
	}
}

func TestReplayResolvesAPlaceholderInsideTheJSONBody(t *testing.T) {
	store := testStore(t)
	var body []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	item := exchange(server.URL + "/v1/items")
	item.RequestHeaders = nil
	item.RequestBody = &inspection.BodyDescriptor{
		State:       inspection.BodyCaptured,
		ContentType: "application/json",
		JSON:        json.RawMessage(`{"token":"{{secret:TOKEN}}","n":1}`),
	}
	engine := &Engine{
		Store:   store,
		Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": `a"b`}),
		Client:  server.Client(),
	}
	result, err := engine.Replay(context.Background(), item, "job-a")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the resolved body is not valid JSON: %v (%s)", err, body)
	}
	if sent["token"] != `a"b` {
		t.Fatalf("sent token = %q", sent["token"])
	}
	if sent["n"] != float64(1) {
		t.Fatalf("sent n = %v", sent["n"])
	}
	if len(result.ResolvedSHA256) != 64 {
		t.Fatalf("ResolvedSHA256 = %q, want 64 hex characters", result.ResolvedSHA256)
	}
}

func TestReplayRefusesAnUnavailableSecretWithoutSending(t *testing.T) {
	store := testStore(t)
	called := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer server.Close()

	engine := &Engine{
		Store:   store,
		Secrets: secrets.NewStaticProvider(map[string]string{}),
		Client:  server.Client(),
	}
	_, err := engine.Replay(context.Background(), exchange(server.URL+"/x"), "job-a")
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Replay() = %v, want a refusal", err)
	}
	if called {
		t.Fatal("a refused replay must not send the request")
	}
}

func TestReplayRecordsATransportFailure(t *testing.T) {
	store := testStore(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL + "/x"
	client := server.Client()
	server.Close() // a dead destination, not a refusal

	engine := &Engine{
		Store:   store,
		Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": "t"}),
		Client:  client,
	}
	result, err := engine.Replay(context.Background(), exchange(url), "job-a")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.TransportError == "" || result.StatusCode != nil {
		t.Fatalf("result = %#v, want a recorded transport failure", result)
	}
	stored, err := store.GetReplay(context.Background(), "run-a", result.ReplayID)
	if err != nil {
		t.Fatalf("GetReplay: %v", err)
	}
	if stored.TransportError == "" {
		t.Fatal("the replay document does not record the transport failure")
	}
}

func TestReplayRefusesAPOST(t *testing.T) {
	store := testStore(t)
	engine := &Engine{
		Store:   store,
		Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": "t"}),
	}
	item := exchange("https://api.example.com/x")
	item.Method = "POST"
	_, err := engine.Replay(context.Background(), item, "job-a")
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Replay() = %v, want a refusal", err)
	}
	if !strings.Contains(refusal.Reason, "GET-only") {
		t.Fatalf("refusal = %q", refusal.Reason)
	}
}

func TestReplayRefusesASignedRequest(t *testing.T) {
	item := exchange("https://api.example.com/x")
	item.RequestHeaders = []inspection.HeaderPair{{Name: "X-Amz-Date", Value: "20261023T120000Z"}}
	err := Check(item)
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Check() = %v, want a refusal", err)
	}
}

func TestReplayResultBodyIsCapturedAndRedacted(t *testing.T) {
	store := testStore(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"secret":"value","ok":true}`)
	}))
	defer server.Close()
	engine := &Engine{
		Store:   store,
		Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": "t"}),
		Client:  server.Client(),
	}
	result, err := engine.Replay(context.Background(), exchange(server.URL+"/x"), "job-a")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !strings.Contains(string(result.ResponseBody.JSON), `"secret":"REDACTED"`) {
		t.Fatalf("response body = %s, want the secret field redacted", result.ResponseBody.JSON)
	}
}

// A replay that cannot be recorded still returns what the destination said; the
// error tells the caller the document is missing.
func TestReplayReportsARecordingFailure(t *testing.T) {
	store := testStore(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer server.Close()
	engine := &Engine{
		Store:   store,
		Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": "t"}),
		Client:  server.Client(),
	}
	item := exchange(server.URL + "/x")
	item.RequestID = "" // RecordReplay rejects it after the send
	_, err := engine.Replay(context.Background(), item, "job-a")
	if err == nil {
		t.Fatal("Replay() = nil, want a recording error")
	}
	if !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("error = %v", err)
	}
}

func TestReplayLimitPerRun(t *testing.T) {
	store := testStore(t)
	now := time.Now().UTC()
	for i := 0; i < inspection.MaxReplaysPerRun; i++ {
		if err := store.RecordReplay(context.Background(), &inspection.Replay{
			ReplayID:       fmt.Sprintf("replay_%d", i),
			RunID:          "run-a",
			RequestID:      "req-1",
			JobID:          "job-a",
			OccurredAt:     now,
			Complete:       true,
			Method:         "GET",
			URL:            "https://api.example.com/x",
			ResolvedSHA256: "x",
		}); err != nil {
			t.Fatalf("RecordReplay %d: %v", i, err)
		}
	}
	err := store.RecordReplay(context.Background(), &inspection.Replay{
		ReplayID: "replay_overflow", RunID: "run-a", RequestID: "req-1",
		OccurredAt: now, Complete: true, Method: "GET", URL: "https://api.example.com/x",
	})
	if !errors.Is(err, inspection.ErrReplayLimit) {
		t.Fatalf("RecordReplay = %v, want ErrReplayLimit", err)
	}
}

// The audit digest must change when the resolved request changes. A body-only
// digest would be the empty hash for every GET; this pins that the URL and
// resolved headers are covered too.
func TestReplayDigestCoversTheResolvedRequest(t *testing.T) {
	store := testStore(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer server.Close()
	item := exchange(server.URL + "/x")

	first := &Engine{Store: store, Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": "one"}), Client: server.Client()}
	second := &Engine{Store: store, Secrets: secrets.NewStaticProvider(map[string]string{"TOKEN": "two"}), Client: server.Client()}
	a, err := first.Replay(context.Background(), item, "job-a")
	if err != nil {
		t.Fatalf("first Replay: %v", err)
	}
	b, err := second.Replay(context.Background(), item, "job-a")
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if a.ResolvedSHA256 == b.ResolvedSHA256 {
		t.Fatal("two different resolved requests produced the same audit digest")
	}
}
