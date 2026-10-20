package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeExportSource struct {
	documents  []ExportDocument
	cursor     int64
	pulls      []int64
	ackStates  []string
	ackReasons []string
	ackRefs    [][]ExportRef
	pullErr    error
	ackErr     error
}

func (f *fakeExportSource) PullExports(_ context.Context, after int64, _ int) ([]ExportDocument, int64, error) {
	f.pulls = append(f.pulls, after)
	if f.pullErr != nil {
		return nil, after, f.pullErr
	}
	documents := f.documents
	f.documents = nil
	return documents, f.cursor, nil
}

func (f *fakeExportSource) AckExports(_ context.Context, refs []ExportRef, state, message string) error {
	if f.ackErr != nil {
		return f.ackErr
	}
	f.ackStates = append(f.ackStates, state)
	f.ackReasons = append(f.ackReasons, message)
	f.ackRefs = append(f.ackRefs, refs)
	return nil
}

type fakeUploader struct {
	calls     int
	documents []ExportDocument
	err       error
}

func (f *fakeUploader) UploadCapture(_ context.Context, documents []ExportDocument) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.documents = documents
	return nil
}

func exportDocumentFixture() ExportDocument {
	return ExportDocument{
		RowID:     7,
		RunID:     "run-1",
		Kind:      "exchange",
		RequestID: "req-1",
		Document:  json.RawMessage(`{"schema_version":1,"kind":"capture"}`),
	}
}

func TestExportLoopUploadsThenAcksSent(t *testing.T) {
	source := &fakeExportSource{documents: []ExportDocument{exportDocumentFixture()}, cursor: 7}
	uploader := &fakeUploader{}
	loop := &ExportLoop{Source: source, Uploader: uploader}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if uploader.calls != 1 || len(uploader.documents) != 1 {
		t.Fatalf("upload calls=%d documents=%d", uploader.calls, len(uploader.documents))
	}
	if len(source.ackStates) != 1 || source.ackStates[0] != ExportAckSent {
		t.Fatalf("acks = %+v, want sent", source.ackStates)
	}
	if len(source.ackRefs[0]) != 1 || source.ackRefs[0][0] != (ExportRef{RunID: "run-1", Kind: "exchange", RequestID: "req-1"}) {
		t.Fatalf("ack refs = %+v", source.ackRefs)
	}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("second Once: %v", err)
	}
	if uploader.calls != 1 {
		t.Fatalf("second Once uploaded an empty batch")
	}
	if len(source.pulls) != 2 || source.pulls[1] != 7 {
		t.Fatalf("second pull cursor = %v, want 7", source.pulls)
	}
}

func TestExportLoopFailureAcksRetryAndKeepsTheRows(t *testing.T) {
	source := &fakeExportSource{documents: []ExportDocument{exportDocumentFixture()}, cursor: 7}
	uploader := &fakeUploader{err: errors.New("cloud is down")}
	loop := &ExportLoop{Source: source, Uploader: uploader}

	err := loop.Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cloud is down") {
		t.Fatalf("Once error = %v", err)
	}
	if len(source.ackStates) != 1 || source.ackStates[0] != ExportAckRetry {
		t.Fatalf("acks = %+v, want retry", source.ackStates)
	}
	if !strings.Contains(source.ackReasons[0], "cloud is down") {
		t.Fatalf("retry reason = %q", source.ackReasons[0])
	}
	if uploader.calls != 1 {
		t.Fatalf("upload calls = %d", uploader.calls)
	}
}

func TestExportLoopIdleDoesNothing(t *testing.T) {
	source := &fakeExportSource{}
	uploader := &fakeUploader{}
	loop := &ExportLoop{Source: source, Uploader: uploader}
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if uploader.calls != 0 || len(source.ackStates) != 0 {
		t.Fatalf("idle cycle uploaded=%d acks=%v", uploader.calls, source.ackStates)
	}
}

func TestHTTPControlPlaneUploadsCapture(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/capture" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if gotAuth := r.Header.Get("Authorization"); gotAuth != "Bearer tok" {
			t.Errorf("authorization = %q", gotAuth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	creds := &MemoryCredentials{}
	creds.Put(&Credential{Token: "tok", RuntimeID: "rt-1", ExpiresAt: time.Now().Add(time.Hour)})
	control := &HTTPControlPlane{BaseURL: server.URL, Creds: creds, RuntimeID: "rt-1"}
	if err := control.UploadCapture(context.Background(), []ExportDocument{exportDocumentFixture()}); err != nil {
		t.Fatalf("UploadCapture: %v", err)
	}
	if got["runtime_id"] != "rt-1" {
		t.Fatalf("body runtime_id = %v", got["runtime_id"])
	}
	documents, ok := got["documents"].([]any)
	if !ok || len(documents) != 1 {
		t.Fatalf("body documents = %v", got["documents"])
	}
}

func TestRuntimeHTTPPullsAndAcksExports(t *testing.T) {
	var acked map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/capture/exports":
			if r.URL.Query().Get("after") != "3" || r.URL.Query().Get("limit") != "10" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": []ExportDocument{exportDocumentFixture()},
				"cursor":    7,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/capture/exports/ack":
			if err := json.NewDecoder(r.Body).Decode(&acked); err != nil {
				t.Errorf("decode ack: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runtime := &RuntimeHTTP{BaseURL: server.URL, Token: "tok"}
	documents, cursor, err := runtime.PullExports(context.Background(), 3, 10)
	if err != nil {
		t.Fatalf("PullExports: %v", err)
	}
	if len(documents) != 1 || cursor != 7 {
		t.Fatalf("pull = %+v cursor=%d", documents, cursor)
	}
	refs := []ExportRef{{RunID: "run-1", Kind: "exchange", RequestID: "req-1"}}
	if err := runtime.AckExports(context.Background(), refs, ExportAckSent, ""); err != nil {
		t.Fatalf("AckExports: %v", err)
	}
	if acked["state"] != "sent" {
		t.Fatalf("acked = %v", acked)
	}
}
