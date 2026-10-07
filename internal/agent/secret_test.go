package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The secret path must present `bootstrap_secret` in the body and NOTHING that
// looks like a signed assertion. A stray signature header would make the control
// plane's SigV4 branch eligible, and a stray `assertion` field would make a body
// log misleading about which proof was used.
func TestExchangeRuntimeSecretSendsTheSecretAndNoAssertion(t *testing.T) {
	now := time.Now()
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("the secret path must not send an Authorization header, got %q", got)
		}
		if got := r.Header.Get("X-Amz-Date"); got != "" {
			t.Errorf("the secret path must not send SigV4 headers, got X-Amz-Date %q", got)
		}
		if got := r.Header.Get("X-Otter-Bootstrap-Provider"); got != "runtime-secret" {
			t.Errorf("provider header = %q, want runtime-secret", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Credential{
			Token: "short-lived", RuntimeID: "rt-1",
			ExpiresAt: now.Add(10 * time.Minute), Generation: 3,
		})
	}))
	defer srv.Close()

	e := &Exchanger{BaseURL: srv.URL, Now: func() time.Time { return now }}
	cred, err := e.Exchange(context.Background(), &RuntimeSecret{Secret: "per-runtime-secret"}, "rt-1", "0.1.0")
	if err != nil {
		t.Fatalf("a secret bootstrap must succeed: %v", err)
	}
	if cred.Token != "short-lived" {
		t.Errorf("unexpected credential %+v", cred)
	}
	if raw["bootstrap_secret"] != "per-runtime-secret" {
		t.Errorf("bootstrap_secret = %v, want the provider's secret", raw["bootstrap_secret"])
	}
	if _, present := raw["assertion"]; present {
		t.Error("the secret body must not carry an assertion")
	}
	if raw["runtime_id"] != "rt-1" {
		t.Errorf("runtime_id = %v, want rt-1", raw["runtime_id"])
	}
}

// A provider with no secret must fail before the network, not send an empty
// proof the control plane would reject with a less useful message.
func TestRuntimeSecretRefusesAnEmptySecret(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	e := &Exchanger{BaseURL: srv.URL, Now: time.Now}
	if _, err := e.Exchange(context.Background(), &RuntimeSecret{}, "rt-1", "0"); err == nil {
		t.Fatal("an empty runtime secret must be refused")
	}
	if called {
		t.Error("an empty secret must not reach the network")
	}
}

func TestRuntimeSecretIdentityCarriesTheSecretAndNoClaims(t *testing.T) {
	id, err := (&RuntimeSecret{Secret: "abc"}).Identity(context.Background())
	if err != nil {
		t.Fatalf("a configured secret must produce an identity: %v", err)
	}
	if id.BootstrapSecret != "abc" {
		t.Errorf("BootstrapSecret = %q, want abc", id.BootstrapSecret)
	}
	if id.Provider != "runtime-secret" {
		t.Errorf("Provider = %q, want runtime-secret", id.Provider)
	}
	if len(id.Claims) != 0 {
		t.Errorf("the secret path makes no claims to bind, got %v", id.Claims)
	}
}
