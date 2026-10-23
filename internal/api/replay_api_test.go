package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/replay"
)

func replayServer(t *testing.T, b *fakeBackend) *httptest.Server {
	t.Helper()
	return newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
}

func TestReplayRouteReturnsTheRecordedResult(t *testing.T) {
	b := newFakeBackend()
	code := http.StatusOK
	b.replayResult = &ReplayResult{
		ReplayID:       "replay_1",
		RunID:          "run-a",
		RequestID:      "req-1",
		StatusCode:     &code,
		ResolvedSHA256: "abc123",
	}
	srv := replayServer(t, b)
	defer srv.Close()

	r := do(t, http.MethodPost, srv.URL+"/v1/replay",
		[]byte(`{"run_id":"run-a","request_id":"req-1"}`), adminHeaders())
	wantStatus(t, r, http.StatusOK)
	if b.replayCalls != 1 {
		t.Fatalf("replay calls = %d, want 1", b.replayCalls)
	}
	var result ReplayResult
	if err := json.Unmarshal(r.body, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ReplayID != "replay_1" || result.ResolvedSHA256 != "abc123" {
		t.Fatalf("result = %#v", result)
	}
}

func TestReplayRouteRequiresBothIds(t *testing.T) {
	b := newFakeBackend()
	srv := replayServer(t, b)
	defer srv.Close()

	for _, body := range []string{`{}`, `{"run_id":"run-a"}`, `{"request_id":"req-1"}`} {
		r := do(t, http.MethodPost, srv.URL+"/v1/replay", []byte(body), adminHeaders())
		wantStatus(t, r, http.StatusBadRequest)
	}
	if b.replayCalls != 0 {
		t.Fatalf("a malformed replay must not reach the backend (calls=%d)", b.replayCalls)
	}
}

func TestReplayRouteMapsARefusalToConflict(t *testing.T) {
	b := newFakeBackend()
	b.replayErr = &replay.Refusal{Reason: "Otter replay is GET-only in v1; this exchange used POST"}
	srv := replayServer(t, b)
	defer srv.Close()

	r := do(t, http.MethodPost, srv.URL+"/v1/replay",
		[]byte(`{"run_id":"run-a","request_id":"req-1"}`), adminHeaders())
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(string(r.body), "GET-only") {
		t.Fatalf("body = %s", r.body)
	}
}

func TestReplayRouteIsLoopbackOnly(t *testing.T) {
	b := newFakeBackend()
	handler := NewServer(ServerConfig{APIToken: "admin-secret"}, b, testLogger()).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/replay", strings.NewReader(`{"run_id":"run-a","request_id":"req-1"}`))
	req.RemoteAddr = "203.0.113.9:4000"
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "replay is loopback-only") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if b.replayCalls != 0 {
		t.Fatalf("a remote replay must not reach the backend (calls=%d)", b.replayCalls)
	}
}

func TestReplayRouteRefusesAnUnauthenticatedCaller(t *testing.T) {
	b := newFakeBackend()
	srv := replayServer(t, b)
	defer srv.Close()

	r := do(t, http.MethodPost, srv.URL+"/v1/replay",
		[]byte(`{"run_id":"run-a","request_id":"req-1"}`), nil)
	wantStatus(t, r, http.StatusUnauthorized)
	if b.replayCalls != 0 {
		t.Fatalf("an unauthenticated replay must not reach the backend (calls=%d)", b.replayCalls)
	}
}
