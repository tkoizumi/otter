package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A runtime that reports itself serving and idle.
func idleRuntime(t *testing.T, extra func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if extra != nil && extra(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "v1"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/maintenance":
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "serving", "running": 0, "accepting_work": true})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

func TestObserveReportsHealthAndMaintenance(t *testing.T) {
	srv := idleRuntime(t, nil)
	defer srv.Close()
	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "t"}
	got, err := rt.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Health != "ok" || got.RuntimeVersion != "v1" || got.Maintenance != "serving" {
		t.Errorf("unexpected observation %+v", got)
	}
}

// The agent must send its credential: a runtime API that accepts anonymous
// control calls would authorise anyone on the host.
func TestTheAgentAuthenticatesToTheRuntime(t *testing.T) {
	var sawAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer runtime-token" {
			sawAuth.Store(true)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "mode": "serving", "running": 0})
	}))
	defer srv.Close()
	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "runtime-token"}
	if _, err := rt.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawAuth.Load() {
		t.Error("the runtime client did not send its bearer token")
	}
}

// A drain that never reaches zero must return ErrDrainTimeout, not hang and not
// pretend success. This is the value Apply turns into "abort the upgrade".
func TestEnterMaintenanceTimesOutWhenWorkNeverFinishes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Always two runs active.
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "draining", "running": 2})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rt := &RuntimeHTTP{BaseURL: srv.URL}
	err := rt.EnterMaintenance(context.Background(), "deploy", 700*time.Millisecond)
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("want ErrDrainTimeout, got %v", err)
	}
	// The message has to say how much work was outstanding, or an operator
	// cannot tell a stuck drain from a busy one.
	if !contains(err.Error(), "2 run(s) still active") {
		t.Errorf("timeout should report the outstanding count: %v", err)
	}
}

// A drain that clears returns as soon as it does, so a deploy is not padded by
// the deadline.
func TestEnterMaintenanceReturnsOnceTheRuntimeIsIdle(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			n := polls.Add(1)
			running := 1
			if n >= 3 {
				running = 0
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "draining", "running": running})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rt := &RuntimeHTTP{BaseURL: srv.URL}
	start := time.Now()
	if err := rt.EnterMaintenance(context.Background(), "deploy", 10*time.Second); err != nil {
		t.Fatalf("a drain that clears must succeed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %s; it should return as soon as running reaches zero", elapsed)
	}
}

// A 5xx is the runtime failing and is retryable; a 4xx is the agent asking
// wrongly and is not. Conflating them either retries an operator error forever or
// gives up on a transient fault.
func TestRuntimeErrorsDistinguishRetryableFromNot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		retryable bool
	}{
		{"503 is retryable", http.StatusServiceUnavailable, true},
		{"500 is retryable", http.StatusInternalServerError, true},
		{"403 is not", http.StatusForbidden, false},
		{"409 is not", http.StatusConflict, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			rt := &RuntimeHTTP{BaseURL: srv.URL}
			_, err := rt.Observe(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			isRetryable := errors.Is(err, ErrRuntimeUnavailable)
			if isRetryable != tc.retryable {
				t.Errorf("status %d: retryable=%v, want %v (%v)", tc.status, isRetryable, tc.retryable, err)
			}
		})
	}
}

// An unreachable runtime is retryable: the daemon may be restarting, and the
// agent must not treat that as a permanent refusal.
func TestAnUnreachableRuntimeIsRetryable(t *testing.T) {
	rt := &RuntimeHTTP{BaseURL: "http://127.0.0.1:1"}
	_, err := rt.Observe(context.Background())
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Errorf("a refused connection must be retryable, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
