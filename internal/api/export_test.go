package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/tkoizumi/otter/internal/inspection"
)

func (f *fakeBackend) PullCaptureExports(_ context.Context, after int64, _ int) (*inspection.ExportBatch, error) {
	if f.exportErr != nil {
		return nil, f.exportErr
	}
	if f.exportBatch != nil {
		return f.exportBatch, nil
	}
	return &inspection.ExportBatch{Documents: []inspection.ExportDocument{}, Cursor: after}, nil
}

func (f *fakeBackend) AckCaptureExports(_ context.Context, refs []inspection.ExportRef, state, message string) error {
	if f.ackErr != nil {
		return f.ackErr
	}
	f.ackRefs = refs
	f.ackState = state
	f.ackMessage = message
	return nil
}

func TestCaptureExportPullAndAck(t *testing.T) {
	backend := newFakeBackend()
	backend.exportBatch = &inspection.ExportBatch{
		Documents: []inspection.ExportDocument{{
			RowID:     7,
			RunID:     "run-1",
			Kind:      inspection.ExportExchange,
			RequestID: "req-1",
			Document:  json.RawMessage(`{"schema_version":1,"kind":"capture"}`),
		}},
		Cursor: 7,
	}
	server := newTestServer(t, ServerConfig{APIToken: "admin"}, backend)
	defer server.Close()
	auth := map[string]string{"Authorization": "Bearer admin"}

	result := do(t, "GET", server.URL+"/v1/capture/exports?after=0&limit=10", nil, auth)
	if result.status != 200 {
		t.Fatalf("pull status = %d: %s", result.status, result.body)
	}
	var batch inspection.ExportBatch
	if err := json.Unmarshal(result.body, &batch); err != nil {
		t.Fatalf("decode pull: %v", err)
	}
	if len(batch.Documents) != 1 || batch.Cursor != 7 {
		t.Fatalf("pull = %+v", batch)
	}
	if string(batch.Documents[0].Document) != `{"schema_version":1,"kind":"capture"}` {
		t.Fatalf("document = %s", batch.Documents[0].Document)
	}

	unauthorized := do(t, "GET", server.URL+"/v1/capture/exports", nil, nil)
	if unauthorized.status != 401 {
		t.Fatalf("unauthenticated pull = %d, want 401", unauthorized.status)
	}

	ack := []byte(`{"documents":[{"run_id":"run-1","kind":"exchange","request_id":"req-1"}],"state":"sent"}`)
	result = do(t, "POST", server.URL+"/v1/capture/exports/ack", ack, auth)
	if result.status != 204 {
		t.Fatalf("ack status = %d: %s", result.status, result.body)
	}
	if backend.ackState != "sent" || len(backend.ackRefs) != 1 || backend.ackRefs[0].RequestID != "req-1" {
		t.Fatalf("ack was not forwarded: state=%q refs=%+v", backend.ackState, backend.ackRefs)
	}

	empty := do(t, "POST", server.URL+"/v1/capture/exports/ack", []byte(`{"documents":[],"state":"sent"}`), auth)
	if empty.status != 400 {
		t.Fatalf("empty ack = %d, want 400", empty.status)
	}
}

func TestCaptureExportIsLoopbackOnly(t *testing.T) {
	backend := newFakeBackend()
	handler := NewServer(ServerConfig{APIToken: "admin"}, backend, testLogger()).Handler()

	req := httptest.NewRequest("GET", "/v1/capture/exports", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != 403 {
		t.Fatalf("remote pull = %d, want 403", recorder.Code)
	}

	req = httptest.NewRequest("GET", "/v1/capture/exports", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer admin")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != 200 {
		t.Fatalf("loopback pull = %d, want 200", recorder.Code)
	}
}
