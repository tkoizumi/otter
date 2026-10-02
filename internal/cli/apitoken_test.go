package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTokenCommandLifecycle drives the CLI's token surfaces against a
// stand-in daemon, asserting both the requests it makes and what it prints.
// The token itself is printed once, so that output is part of the contract.
func TestTokenCommandLifecycle(t *testing.T) {
	root := t.TempDir()
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"tok-1","name":"cloud-gateway","scope":"control",`+
				`"created_at":"2026-10-02T12:00:00Z","token":"otter_ctl_abc123"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tokens":
			fmt.Fprint(w, `{"tokens":[`+
				`{"id":"tok-1","name":"cloud-gateway","scope":"control","created_at":"2026-10-02T12:00:00Z"},`+
				`{"id":"tok-0","name":"old","scope":"read","created_at":"2026-10-01T09:00:00Z",`+
				`"revoked_at":"2026-10-02T10:00:00Z"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/tokens/tok-1":
			fmt.Fprint(w, `{"id":"tok-1","revoked":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"no route for `+r.URL.Path+`"}}`)
		}
	}))
	defer srv.Close()
	recordLiveDaemon(t, root, srv.URL)

	stdout, stderr, code := otterIn(t, root, "token", "create", "--name", "cloud-gateway", "--scope", "control")
	if code != 0 {
		t.Fatalf("token create exited %d: %s", code, stderr)
	}
	for _, want := range []string{"otter_ctl_abc123", "cloud-gateway", "control", "shown once"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("token create output is missing %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, code = otterIn(t, root, "token", "list")
	if code != 0 {
		t.Fatalf("token list exited %d: %s", code, stderr)
	}
	for _, want := range []string{"tok-1", "tok-0", "active", "revoked"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("token list output is missing %q:\n%s", want, stdout)
		}
	}
	// A listing must never print a token, only what identifies one.
	if strings.Contains(stdout, "otter_ctl_") || strings.Contains(stdout, "otter_ro_") {
		t.Errorf("token list printed a credential:\n%s", stdout)
	}

	stdout, stderr, code = otterIn(t, root, "token", "revoke", "tok-1")
	if code != 0 {
		t.Fatalf("token revoke exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "tok-1") {
		t.Errorf("token revoke did not name the token:\n%s", stdout)
	}

	// The client probes /health before each call, so only the token calls are
	// asserted, and in order.
	var tokenCalls []string
	for _, s := range seen {
		if strings.Contains(s, "/v1/tokens") {
			tokenCalls = append(tokenCalls, s)
		}
	}
	want := []string{"POST /v1/tokens", "GET /v1/tokens", "DELETE /v1/tokens/tok-1"}
	if strings.Join(tokenCalls, ", ") != strings.Join(want, ", ") {
		t.Errorf("token calls = %v, want %v", tokenCalls, want)
	}
}

// A daemon that refuses the call must not be reported as success.
func TestTokenCreateSurfacesRefusal(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"invalid_request","message":"scope must be \"read\" or \"control\""}}`)
	}))
	defer srv.Close()
	recordLiveDaemon(t, root, srv.URL)

	_, stderr, code := otterIn(t, root, "token", "create", "--name", "x", "--scope", "admin")
	if code == 0 {
		t.Fatal("token create reported success for a refused scope")
	}
	if !strings.Contains(stderr, "control") {
		t.Errorf("the refusal is not surfaced:\n%s", stderr)
	}
}

func TestTokenUsageErrors(t *testing.T) {
	root := t.TempDir()
	cases := [][]string{
		{"token"},
		{"token", "nonsense"},
		{"token", "revoke"},
		{"token", "list", "extra"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, code := otterIn(t, root, args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 for %v", code, args)
			}
		})
	}
}
