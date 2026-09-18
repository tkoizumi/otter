package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/runs"
)

func newSeededBackend() *fakeBackend {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "wh-token")
	now := time.Now().UTC()
	b.addRun(&runs.Run{
		ID:            "run-1",
		IntegrationID: "int-A",
		TriggerType:   TriggerManual,
		Status:        runs.StatusSucceeded,
		Attempt:       1,
		CreatedAt:     now,
	})
	b.addLogs("run-1",
		runs.LogEntry{ID: 1, RunID: "run-1", Timestamp: now, Stream: runs.StreamOtter, Message: "first"},
		runs.LogEntry{ID: 2, RunID: "run-1", Timestamp: now, Stream: runs.StreamStdout, Message: "second"},
	)
	b.seedState("int-A", "keep", `{"v":1}`)
	b.seedState("int-A", "drop", `"bye"`)
	b.runTokens["run-token"] = RunToken{RunID: "run-1", IntegrationID: "int-A"}
	return b
}

func TestNewClientDefaultsAndTrimming(t *testing.T) {
	c := NewClient("", "")
	if c.BaseURL != DefaultBaseURL {
		t.Fatalf("BaseURL = %q, want %q", c.BaseURL, DefaultBaseURL)
	}
	if c.HTTP == nil {
		t.Fatalf("HTTP client must not be nil")
	}
	c = NewClient("http://example.test/", "tok")
	if c.BaseURL != "http://example.test" {
		t.Fatalf("BaseURL = %q, want trailing slash trimmed", c.BaseURL)
	}
	if c.Token != "tok" {
		t.Fatalf("Token = %q", c.Token)
	}
}

func TestClientRoundTrip(t *testing.T) {
	b := newSeededBackend()
	srv := newTestServer(t, ServerConfig{APIToken: "admin-token"}, b)
	defer srv.Close()

	ctx := context.Background()
	c := NewClient(srv.URL, "admin-token")

	t.Run("health", func(t *testing.T) {
		health, err := c.Health(ctx)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if health.Status != "ok" || health.Version != b.version {
			t.Fatalf("health = %+v", health)
		}
	})

	t.Run("list integrations", func(t *testing.T) {
		views, err := c.ListIntegrations(ctx)
		if err != nil {
			t.Fatalf("ListIntegrations: %v", err)
		}
		if len(views) != 1 || views[0].ID != "int-A" {
			t.Fatalf("views = %+v", views)
		}
		if views[0].Triggers.WebhookToken != "" {
			t.Fatalf("list must not expose the webhook token")
		}
	})

	t.Run("get integration", func(t *testing.T) {
		view, err := c.GetIntegration(ctx, "int-A")
		if err != nil {
			t.Fatalf("GetIntegration: %v", err)
		}
		if view.ID != "int-A" || view.Triggers.WebhookToken != "wh-token" {
			t.Fatalf("view = %+v", view)
		}
	})

	t.Run("submit run", func(t *testing.T) {
		runID, err := c.SubmitRun(ctx, "int-A", json.RawMessage(`{"a":1}`))
		if err != nil {
			t.Fatalf("SubmitRun: %v", err)
		}
		if runID == "" {
			t.Fatalf("SubmitRun returned an empty run id")
		}
		view, err := c.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun(%s): %v", runID, err)
		}
		if view.ID != runID || view.IntegrationID != "int-A" {
			t.Fatalf("run = %+v", view.Run)
		}
	})

	t.Run("submit run with empty body", func(t *testing.T) {
		runID, err := c.SubmitRun(ctx, "int-A", nil)
		if err != nil {
			t.Fatalf("SubmitRun: %v", err)
		}
		if runID == "" {
			t.Fatalf("SubmitRun returned an empty run id")
		}
	})

	t.Run("cancel run", func(t *testing.T) {
		if err := c.CancelRun(ctx, "run-1"); err != nil {
			t.Fatalf("CancelRun: %v", err)
		}
	})

	t.Run("list runs", func(t *testing.T) {
		list, err := c.ListRuns(ctx, RunsQuery{IntegrationID: "int-A", Status: string(runs.StatusSucceeded), Limit: 10})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(list) != 1 || list[0].ID != "run-1" {
			t.Fatalf("runs = %+v", list)
		}
	})

	t.Run("get run", func(t *testing.T) {
		view, err := c.GetRun(ctx, "run-1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if view.ID != "run-1" || view.RootRunID != "run-1" {
			t.Fatalf("view = %+v", view)
		}
		if len(view.Attempts) != 1 {
			t.Fatalf("attempts = %+v", view.Attempts)
		}
	})

	t.Run("get logs", func(t *testing.T) {
		logs, err := c.GetLogs(ctx, "run-1", 1, 100)
		if err != nil {
			t.Fatalf("GetLogs: %v", err)
		}
		if len(logs) != 1 || logs[0].ID != 2 {
			t.Fatalf("logs = %+v", logs)
		}
	})

	t.Run("all state", func(t *testing.T) {
		state, err := c.AllState(ctx, "int-A")
		if err != nil {
			t.Fatalf("AllState: %v", err)
		}
		if string(state["keep"]) != `{"v":1}` {
			t.Fatalf("state = %+v", state)
		}
	})

	t.Run("get state raw value", func(t *testing.T) {
		raw, err := c.GetState(ctx, "int-A", "keep")
		if err != nil {
			t.Fatalf("GetState: %v", err)
		}
		if len(raw) == 0 || raw[0] != '{' {
			t.Fatalf("raw state = %q, want the bare JSON object", raw)
		}
		var decoded map[string]int
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("raw state is not valid JSON: %v", err)
		}
		if decoded["v"] != 1 {
			t.Fatalf("decoded state = %+v", decoded)
		}
	})

	t.Run("set state", func(t *testing.T) {
		out, err := c.SetState(ctx, "int-A", "new", json.RawMessage(`[1,2,3]`))
		if err != nil {
			t.Fatalf("SetState: %v", err)
		}
		if out.IntegrationID != "int-A" || out.Key != "new" || string(out.Value) != `[1,2,3]` {
			t.Fatalf("response = %+v", out)
		}
		if out.UpdatedAt.IsZero() {
			t.Fatalf("updated_at is zero")
		}
	})

	t.Run("set state rejects invalid json client-side", func(t *testing.T) {
		if _, err := c.SetState(ctx, "int-A", "bad", json.RawMessage(`{`)); err == nil {
			t.Fatalf("SetState accepted an invalid JSON value")
		}
	})

	t.Run("delete state", func(t *testing.T) {
		if err := c.DeleteState(ctx, "int-A", "drop"); err != nil {
			t.Fatalf("DeleteState: %v", err)
		}
		if err := c.DeleteState(ctx, "int-A", "drop"); err == nil {
			t.Fatalf("second DeleteState should fail")
		} else {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || !apiErr.IsNotFound() {
				t.Fatalf("second DeleteState error = %v, want not-found APIError", err)
			}
		}
	})
}

func TestClientErrorMapping(t *testing.T) {
	b := newSeededBackend()
	srv := newTestServer(t, ServerConfig{APIToken: "admin-token"}, b)
	defer srv.Close()
	ctx := context.Background()

	admin := NewClient(srv.URL, "admin-token")

	t.Run("404 is not found", func(t *testing.T) {
		_, err := admin.GetIntegration(ctx, "ghost")
		assertAPIError(t, err, http.StatusNotFound)
		if !isNotFound(err) {
			t.Fatalf("GetIntegration error = %v, want IsNotFound()", err)
		}

		if _, err := admin.GetRun(ctx, "ghost"); !isNotFound(err) {
			t.Fatalf("GetRun error = %v, want not found", err)
		}
		if _, err := admin.GetState(ctx, "int-A", "ghost"); !isNotFound(err) {
			t.Fatalf("GetState error = %v, want not found", err)
		}
	})

	t.Run("401 without token", func(t *testing.T) {
		anon := NewClient(srv.URL, "")
		_, err := anon.ListIntegrations(ctx)
		assertAPIError(t, err, http.StatusUnauthorized)
		if !isUnauthorized(err) {
			t.Fatalf("ListIntegrations error = %v, want IsUnauthorized()", err)
		}
	})

	t.Run("401 with wrong token", func(t *testing.T) {
		wrong := NewClient(srv.URL, "not-the-token")
		_, err := wrong.Health(ctx) // /health is unauthenticated
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		_, err = wrong.GetIntegration(ctx, "int-A")
		assertAPIError(t, err, http.StatusUnauthorized)
	})

	t.Run("403 for run token on admin route", func(t *testing.T) {
		runScoped := NewClient(srv.URL, "run-token")
		_, err := runScoped.ListIntegrations(ctx)
		assertAPIError(t, err, http.StatusForbidden)
		if !isUnauthorized(err) {
			t.Fatalf("IsUnauthorized() should cover 403")
		}
	})

	t.Run("run token can read its own run", func(t *testing.T) {
		runScoped := NewClient(srv.URL, "run-token")
		view, err := runScoped.GetRun(ctx, "run-1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if view.ID != "run-1" {
			t.Fatalf("run = %+v", view.Run)
		}
		if _, err := runScoped.GetRun(ctx, "run-other"); !isForbidden(err) {
			t.Fatalf("GetRun on another run = %v, want 403", err)
		}
	})
}

func TestDecodeAPIErrorFallback(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		payload    string
		wantCode   string
		wantMsg    string
		wantStatus int
	}{
		{
			name:       "json envelope",
			status:     http.StatusBadRequest,
			payload:    `{"error":{"code":"invalid_request","message":"bad input"}}`,
			wantCode:   "invalid_request",
			wantMsg:    "bad input",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "plain text body",
			status:     http.StatusInternalServerError,
			payload:    "plain failure\n",
			wantCode:   "",
			wantMsg:    "plain failure",
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "empty envelope object falls back to raw body",
			status:     http.StatusBadGateway,
			payload:    `{"error":{}}`,
			wantCode:   "",
			wantMsg:    `{"error":{}}`,
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "empty body uses the status text",
			status:     http.StatusServiceUnavailable,
			payload:    "",
			wantCode:   "",
			wantMsg:    http.StatusText(http.StatusServiceUnavailable),
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := decodeAPIError(tc.status, []byte(tc.payload))
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error type = %T, want *APIError", err)
			}
			if apiErr.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", apiErr.StatusCode, tc.wantStatus)
			}
			if apiErr.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if apiErr.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", apiErr.Message, tc.wantMsg)
			}
			if apiErr.Error() == "" {
				t.Fatalf("Error() must not be empty")
			}
		})
	}
}

func TestClientSendsBearerToken(t *testing.T) {
	type captured struct {
		auth   string
		accept string
	}

	cases := []struct {
		name      string
		token     string
		wantAuth  string
		wantToken bool
	}{
		{"with token", "tok-123", "Bearer tok-123", true},
		{"without token", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan captured, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ch <- captured{auth: r.Header.Get("Authorization"), accept: r.Header.Get("Accept")}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, tc.token)
			if _, err := c.Health(context.Background()); err != nil {
				t.Fatalf("Health: %v", err)
			}

			got := <-ch
			if got.auth != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", got.auth, tc.wantAuth)
			}
			if got.accept != "application/json" {
				t.Fatalf("Accept = %q, want application/json", got.accept)
			}
		})
	}
}

// ------------------------------------------------------------------ helpers

func assertAPIError(t *testing.T, err error, wantStatus int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error with status %d, got nil", wantStatus)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T (%v), want *APIError", err, err)
	}
	if apiErr.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (err: %v)", apiErr.StatusCode, wantStatus, err)
	}
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

func isUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsUnauthorized()
}

func isForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden
}
