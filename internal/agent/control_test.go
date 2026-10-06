package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A control plane that behaves like the routes: it checks the bearer token, and
// it refuses a request that does not name a runtime.
func fakeCloud(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body map[string]any)) (*httptest.Server, *int) {
	t.Helper()
	refusals := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["runtime_id"] == nil || body["runtime_id"] == "" {
			// Mirrors the real routes, which refuse rather than default.
			refusals++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"request does not name a runtime"}}`))
			return
		}
		handler(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &refusals
}

func liveCreds() *MemoryCredentials {
	c := &MemoryCredentials{}
	c.Put(&Credential{Token: "good-token", RuntimeID: "rt-1", ExpiresAt: time.Now().Add(time.Hour)})
	return c
}

// The agent must authenticate every call. An agent that called without its
// credential would work against a permissive control plane and fail in
// production.
func TestControlPlaneSendsItsCredential(t *testing.T) {
	seen := 0
	srv, _ := fakeCloud(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		seen++
		_ = json.NewEncoder(w).Encode(Desired{Generation: 1, Release: Release{Digest: "sha256:a", URL: "u"}})
	})
	c := &HTTPControlPlane{BaseURL: srv.URL, Creds: liveCreds(), RuntimeID: "rt-1"}
	if _, err := c.Desired(context.Background(), Observed{}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("the control plane did not accept the credential (%d accepted calls)", seen)
	}
}

// Every request must name its runtime: the routes refuse one that does not, and
// an omission would make every call fail while local tests passed.
func TestEveryControlPlaneCallNamesItsRuntime(t *testing.T) {
	srv, refusals := fakeCloud(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		switch r.URL.Path {
		case "/agent/v1/desired":
			_ = json.NewEncoder(w).Encode(Desired{Generation: 1, Release: Release{Digest: "sha256:a", URL: "u"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"recorded": true, "lease_seconds": 60})
		}
	})
	c := &HTTPControlPlane{BaseURL: srv.URL, Creds: liveCreds(), RuntimeID: "rt-1"}
	if _, err := c.Desired(context.Background(), Observed{}); err != nil {
		t.Errorf("desired: %v", err)
	}
	if err := c.Report(context.Background(), Reported{Generation: 1, Outcome: OutcomeApplied}); err != nil {
		t.Errorf("report: %v", err)
	}
	if err := c.Lease(context.Background()); err != nil {
		t.Errorf("lease: %v", err)
	}
	if *refusals != 0 {
		t.Errorf("%d call(s) did not name a runtime", *refusals)
	}
}

// 204 means "nothing to deploy", which is not a failure: the agent keeps serving
// what it runs and asks again.
func TestNoDesiredWorkIsNotTreatedAsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := &HTTPControlPlane{BaseURL: srv.URL, Creds: liveCreds(), RuntimeID: "rt-1"}
	_, err := c.Desired(context.Background(), Observed{})
	if !errors.Is(err, errNoDesiredWork) {
		t.Fatalf("want errNoDesiredWork, got %v", err)
	}
}

// A rejected credential must be retryable, because re-asserting is the path that
// exists -- and it must not stop the runtime continuing to serve.
func TestARejectedCredentialIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &HTTPControlPlane{BaseURL: srv.URL, Creds: liveCreds(), RuntimeID: "rt-1"}
	_, err := c.Desired(context.Background(), Observed{})
	if !errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("a rejected credential should send the agent back to bootstrap, got %v", err)
	}
}

// With no credential, every call fails the same way rather than sending an
// unauthenticated request.
func TestNoCredentialFailsBeforeTheNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	c := &HTTPControlPlane{BaseURL: srv.URL, Creds: &MemoryCredentials{}, RuntimeID: "rt-1"}
	if err := c.Lease(context.Background()); !errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("want ErrBootstrapUnavailable, got %v", err)
	}
	if called {
		t.Error("an unauthenticated request must not reach the control plane")
	}
}

// RunOnce performs exactly one cycle, sharing step() with Run so a diagnostic
// cannot take a different path from the real loop.
func TestRunOncePerformsOneCycle(t *testing.T) {
	rt := &fakeRuntime{}
	srv, _ := fakeCloud(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		switch r.URL.Path {
		case "/agent/v1/desired":
			_ = json.NewEncoder(w).Encode(Desired{Generation: 1, Release: Release{Digest: "sha256:a", URL: "u"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"recorded": true, "lease_seconds": 60})
		}
	})
	l := &Loop{
		Control: &HTTPControlPlane{BaseURL: srv.URL, Creds: liveCreds(), RuntimeID: "rt-1"},
		Runtime: rt, Creds: liveCreds(), RuntimeID: "rt-1", Drain: Drain{Timeout: time.Minute},
		backoffBase: time.Millisecond, jitter: func(d time.Duration) time.Duration { return d },
	}
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// One cycle means the runtime was driven through the apply sequence exactly
	// once: fetched, gated, promoted, validated, and released back to serving.
	joined := ""
	for _, c := range rt.calls {
		joined += c + " "
	}
	for _, want := range []string{"fetch:", "enter:", "promote:", "validate:", "exit"} {
		found := false
		for _, c := range rt.calls {
			if len(c) >= len(want) && c[:len(want)] == want {
				found = true
			}
		}
		if !found {
			t.Errorf("one cycle did not include %q; calls were: %s", want, joined)
		}
	}
}
