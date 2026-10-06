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

// stubBootstrap is a provider that returns whatever the test wants, so the
// exchange can be tested without IMDS.
type stubBootstrap struct {
	a   *Assertion
	err error
}

func (s *stubBootstrap) Provider() string { return "stub" }
func (s *stubBootstrap) Assert(context.Context) (*Assertion, error) {
	return s.a, s.err
}

func goodAssertion(now time.Time) *Assertion {
	return &Assertion{
		Provider: "stub",
		Claims:   map[string]string{"account_id": "426714791664", "role": "otter-agent", "instance_id": "i-abc"},
		SignedAt: now,
		Headers:  map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=stub", "X-Amz-Date": "20261006T120000Z"},
	}
}

func TestExchangeReturnsABoundCredential(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/bootstrap" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		// The signed values must travel AS HEADERS: a verifier re-derives the
		// signature from the headers it receives, so sending the Authorization
		// header alone would leave it without the date and body hash it needs.
		if r.Header.Get("Authorization") == "" {
			t.Error("the signed Authorization header was not sent")
		}
		if r.Header.Get("X-Amz-Date") == "" {
			t.Error("x-amz-date was not sent; the verifier cannot reproduce the signature without it")
		}
		if r.Header.Get("X-Otter-Bootstrap-Provider") != "stub" {
			t.Errorf("provider header = %q, want stub", r.Header.Get("X-Otter-Bootstrap-Provider"))
		}
		// The JSON body must not carry the signed material: it is a header so a
		// body log cannot leak it.
		var req bootstrapRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Credential{
			Token: "short-lived", RuntimeID: "rt-1",
			ExpiresAt: now.Add(10 * time.Minute), Generation: 41,
		})
	}))
	defer srv.Close()

	e := &Exchanger{BaseURL: srv.URL, Now: func() time.Time { return now }}
	cred, err := e.Exchange(context.Background(), &stubBootstrap{a: goodAssertion(now)}, "rt-1", "0.1.0")
	if err != nil {
		t.Fatalf("a valid exchange must succeed: %v", err)
	}
	if cred.Token != "short-lived" || cred.Generation != 41 {
		t.Errorf("unexpected credential %+v", cred)
	}
}

// A credential for a different runtime must be refused. Accepting it would mean
// this agent managing another tenant's runtime, which is the cross-tenant
// failure the whole binding exists to prevent.
func TestExchangeRefusesACredentialBoundToAnotherRuntime(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Credential{Token: "t", RuntimeID: "rt-OTHER", ExpiresAt: now.Add(time.Hour)})
	}))
	defer srv.Close()

	e := &Exchanger{BaseURL: srv.URL, Now: func() time.Time { return now }}
	_, err := e.Exchange(context.Background(), &stubBootstrap{a: goodAssertion(now)}, "rt-mine", "0.1.0")
	if !errors.Is(err, ErrBootstrapRefused) {
		t.Fatalf("want ErrBootstrapRefused, got %v", err)
	}
	if !strings.Contains(err.Error(), "rt-OTHER") {
		t.Errorf("the refusal should name both runtimes: %v", err)
	}
}

// Refused and unavailable must be distinguishable, because one is retried and
// the other must not be. Conflating them either hammers a control plane that has
// already said no, or gives up on a transient failure.
func TestExchangeDistinguishesRefusedFromUnavailable(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"401 is refused", http.StatusUnauthorized, ErrBootstrapRefused},
		{"403 is refused", http.StatusForbidden, ErrBootstrapRefused},
		{"400 is refused", http.StatusBadRequest, ErrBootstrapRefused},
		{"500 is unavailable", http.StatusInternalServerError, ErrBootstrapUnavailable},
		{"503 is unavailable", http.StatusServiceUnavailable, ErrBootstrapUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			e := &Exchanger{BaseURL: srv.URL, Now: func() time.Time { return now }}
			_, err := e.Exchange(context.Background(), &stubBootstrap{a: goodAssertion(now)}, "rt-1", "0")
			if !errors.Is(err, tc.want) {
				t.Errorf("status %d: want %v, got %v", tc.status, tc.want, err)
			}
		})
	}
}

// A response that is not usable as a credential must not become one. Each case
// is a way a control plane could hand back something that looks like a success.
func TestExchangeRefusesUnusableCredentials(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		cred Credential
	}{
		{"no token", Credential{RuntimeID: "rt-1", ExpiresAt: now.Add(time.Hour)}},
		{"not bound to a runtime", Credential{Token: "t", ExpiresAt: now.Add(time.Hour)}},
		{"already expired", Credential{Token: "t", RuntimeID: "rt-1", ExpiresAt: now.Add(-time.Minute)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(c.cred)
			}))
			defer srv.Close()
			e := &Exchanger{BaseURL: srv.URL, Now: func() time.Time { return now }}
			_, err := e.Exchange(context.Background(), &stubBootstrap{a: goodAssertion(now)}, "rt-1", "0")
			if !errors.Is(err, ErrBootstrapRefused) {
				t.Errorf("want a refusal, got %v", err)
			}
		})
	}
}

// A malformed response is a control-plane problem, not a refusal: retrying may
// work once the deployment is fixed, and treating it as refused would take the
// host out of service permanently.
func TestExchangeTreatsAMalformedResponseAsUnavailable(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	e := &Exchanger{BaseURL: srv.URL, Now: func() time.Time { return now }}
	_, err := e.Exchange(context.Background(), &stubBootstrap{a: goodAssertion(now)}, "rt-1", "0")
	if !errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("want ErrBootstrapUnavailable, got %v", err)
	}
}

// The exchanger re-validates the assertion, so a provider that builds a
// malformed one cannot put it on the wire.
func TestExchangeRejectsAMalformedAssertionBeforeSending(t *testing.T) {
	now := time.Now()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	e := &Exchanger{BaseURL: srv.URL, Now: func() time.Time { return now }}
	bad := goodAssertion(now)
	bad.Headers = nil
	if _, err := e.Exchange(context.Background(), &stubBootstrap{a: bad}, "rt-1", "0"); err == nil {
		t.Fatal("expected a refusal")
	}
	if called {
		t.Error("a malformed assertion must not reach the network")
	}
}

// The credential store is in-memory on purpose: persisting a short-lived
// credential would recreate the long-lived-secret problem the instance-role
// bootstrap exists to avoid. The margin is what stops a request starting with a
// credential that expires in flight.
func TestCredentialStoreExpiresWithAMargin(t *testing.T) {
	now := time.Now()
	m := &MemoryCredentials{}
	if !m.Expired(now) {
		t.Error("an empty store must report expired")
	}
	m.Put(&Credential{Token: "t", RuntimeID: "rt", ExpiresAt: now.Add(10 * time.Minute)})
	if m.Expired(now) {
		t.Error("a healthy credential must not report expired")
	}
	// Inside the margin but not yet expired: still treated as expired, so the
	// agent refreshes rather than starting a request it cannot finish.
	if !m.Expired(now.Add(10*time.Minute - 10*time.Second)) {
		t.Error("a credential inside the expiry margin must be treated as expired")
	}
	if _, ok := m.Get(); !ok {
		t.Error("Get must return the stored credential")
	}
}
