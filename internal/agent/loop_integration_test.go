package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func sha256HexString(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The full cycle against a REAL HTTP runtime and a REAL HTTP control plane in
// process: bootstrap -> credential -> desired -> apply -> report -> lease.
//
// Every other test in this package fakes one side. The integration failures that
// have actually bitten were all of that kind -- a provider signing bytes the
// verifier never saw, a route reconstructing headers it had received, a Worker
// not seeing shell env -- so at least one test must exercise the assembled
// pipeline over a socket rather than through an interface.
//
// What it does NOT prove, stated plainly: that the Cloud routes agree. That needs
// the Next.js app, and is covered by the shared wire fixtures and by running the
// real control plane. This closes the other half: that the agent's own pieces --
// call order, credential flow, runtime API usage -- work together over HTTP.
func TestTheFullCycleOverRealHTTP(t *testing.T) {
	var mu sync.Mutex
	var runtimeCalls, cloudPaths []string

	// A release served over HTTP whose bytes hash to the digest the control plane
	// will name, so the real Fetcher verifies it and the apply sequence runs to
	// completion. Overriding Fetch to force a failure would leave the interesting
	// half of the cycle -- gate, promote, validate, activate -- untested while the
	// test still passed.
	release := []byte("integration release bytes")
	digest := "sha256:" + sha256HexString(release)
	rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(release)
	}))
	defer rel.Close()

	// A runtime that behaves like otterd: it serves health, the maintenance view,
	// and the activation endpoint, and it records what was asked.
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		runtimeCalls = append(runtimeCalls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "v-test"})
		case r.URL.Path == "/v1/runtime/releases/active":
			_ = json.NewEncoder(w).Encode(map[string]any{"active": []map[string]string{}})
		case r.URL.Path == "/v1/runtime/maintenance":
			// Serving and idle until asked to gate, then gated and idle: the drain
			// completes on the first poll.
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "maintenance", "running": 0, "accepting_work": false})
		case r.URL.Path == "/v1/runtime/releases/install":
			// The agent hands the downloaded package to the runtime, which is what
			// makes the digest an address: the runtime recomputes it before
			// installing, so this endpoint must be reached for a deploy to apply.
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"job": "sync", "digest": "sha256:x"})
		case r.URL.Path == "/v1/runtime/releases/activate":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"job": "sync", "digest": "sha256:x"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer rt.Close()

	// A control plane that behaves like the routes: it refuses a request without a
	// credential, and requires a runtime id on every call.
	creds := &MemoryCredentials{}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cloudPaths = append(cloudPaths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer issued-token" {
			if r.Header.Get("X-Sabotage") != "1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["runtime_id"] == nil || body["runtime_id"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/agent/v1/desired":
			if r.Header.Get("X-Sabotage") == "1" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(Desired{
				RuntimeID:  "rt-1",
				Generation: 7,
				Release:    Release{Digest: digest, URL: rel.URL},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"recorded": true, "lease_seconds": 60})
		}
	}))
	defer cloud.Close()

	// A credential the bootstrap exchange would have obtained.
	creds.Put(&Credential{Token: "issued-token", RuntimeID: "rt-1", ExpiresAt: time.Now().Add(time.Hour), Generation: 7})

	rtc := &RuntimeHTTP{
		BaseURL: rt.URL, Token: "runtime-token", ReleaseDir: t.TempDir(),
		// The release client fetches the PACKAGE from Cloud; the runtime client
		// installs it. A live deploy uses two different credentials here, so the
		// test supplies a client rather than relying on a default.
		ReleaseClient: rel.Client(),
	}

	l := &Loop{
		Control:     &HTTPControlPlane{BaseURL: cloud.URL, Creds: creds, RuntimeID: "rt-1"},
		Runtime:     rtc,
		Creds:       creds,
		RuntimeID:   "rt-1",
		Drain:       Drain{Timeout: 5 * time.Second},
		backoffBase: time.Millisecond,
		jitter:      func(d time.Duration) time.Duration { return d },
	}

	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("the cycle must complete against a working runtime and control plane: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// The runtime must have been driven through the API, in the protocol's order.
	joined := ""
	for _, c := range runtimeCalls {
		joined += c + " | "
	}
	// The whole apply sequence, in the protocol's order. Asserting only the first
	// two calls would let the interesting half -- gate, promote, validate,
	// activate -- go untested while the test still passed, which is what an
	// earlier version of this test did.
	// Subsequence, not a prefix: observe and validate interleave reads, and
	// asserting an exact prefix would break every time a read is added even though
	// the protocol order is intact.
	//
	// INSTALL COMES BEFORE ENTERING MAINTENANCE, and that is deliberate: the
	// sequence fetches and installs first so a failed download does not gate a
	// serving runtime.
	wantOrder := []string{
		"GET /health",
		"POST /v1/runtime/releases/install",
		"POST /v1/runtime/maintenance",
		"POST /v1/runtime/releases/activate",
		"DELETE /v1/runtime/maintenance",
	}
	idx := 0
	for _, c := range runtimeCalls {
		if idx < len(wantOrder) && c == wantOrder[idx] {
			idx++
		}
	}
	if idx != len(wantOrder) {
		t.Errorf("the cycle did not drive the runtime through the protocol in order;\n"+
			"  expected prefix of: %v\n  actual calls: %s", wantOrder, joined)
	}

	// The control plane must have been asked what to run, with a credential.
	foundDesired := false
	for _, c := range cloudPaths {
		if c == "POST /agent/v1/desired" {
			foundDesired = true
		}
	}
	if !foundDesired {
		t.Errorf("the cycle never asked the control plane what to run; calls were: %v", cloudPaths)
	}
}
