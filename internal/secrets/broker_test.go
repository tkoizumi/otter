package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBroker stands in for the host-side broker. It performs the checks the real
// one must perform — verify the token, confirm the token's runtime matches the
// runtime the caller claims — so the tests exercise the contract rather than
// only the client.
type fakeBroker struct {
	key     string
	secrets map[string]string

	mu       sync.Mutex
	requests int
	lastID   WorkloadIdentity
}

func (f *fakeBroker) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()

		var req exchangeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id, err := VerifyWorkloadToken(f.key, req.Token, time.Now())
		if err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		// The token's runtime must match the runtime the caller named: the
		// broker must not trust either field on its own.
		if id.RuntimeID != req.RuntimeID {
			http.Error(w, "audience mismatch", http.StatusForbidden)
			return
		}
		f.mu.Lock()
		f.lastID = id
		secrets := make(map[string]string, len(f.secrets))
		for k, v := range f.secrets {
			secrets[k] = v
		}
		f.mu.Unlock()

		_ = json.NewEncoder(w).Encode(exchangeResponse{Secrets: secrets})
	}
}

func (f *fakeBroker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func newFakeBroker(t *testing.T) (*fakeBroker, *httptest.Server) {
	t.Helper()
	broker := &fakeBroker{
		key:     "per-runtime-secret",
		secrets: map[string]string{"SHOPIFY_CLIENT_ID": "id-1", "CLICKHOUSE_USER": "reader"},
	}
	server := httptest.NewServer(broker.handler())
	t.Cleanup(server.Close)
	return broker, server
}

func TestBrokerProviderExchangesOnceForTheWholeDocument(t *testing.T) {
	broker, server := newFakeBroker(t)
	provider := NewBrokerProvider(server.URL, "per-runtime-secret",
		WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}, BrokerOptions{})
	ctx := context.Background()

	first, err := provider.Get(ctx, "SHOPIFY_CLIENT_ID")
	if err != nil {
		t.Fatalf("get SHOPIFY_CLIENT_ID: %v", err)
	}
	if first != "id-1" {
		t.Errorf("SHOPIFY_CLIENT_ID = %q, want id-1", first)
	}
	second, err := provider.Get(ctx, "CLICKHOUSE_USER")
	if err != nil {
		t.Fatalf("get CLICKHOUSE_USER: %v", err)
	}
	if second != "reader" {
		t.Errorf("CLICKHOUSE_USER = %q, want reader", second)
	}

	// One exchange serves every key the job declares.
	if got := broker.count(); got != 1 {
		t.Errorf("broker requests = %d, want 1", got)
	}
}

func TestBrokerProviderPresentsTheRunIdentity(t *testing.T) {
	broker, server := newFakeBroker(t)
	provider := NewBrokerProvider(server.URL, "per-runtime-secret",
		WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_7"}, BrokerOptions{})

	if _, err := provider.Get(context.Background(), "SHOPIFY_CLIENT_ID"); err != nil {
		t.Fatalf("get: %v", err)
	}

	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.lastID.RuntimeID != "rt_1" || broker.lastID.JobID != "job_7" {
		t.Errorf("broker saw identity %+v, want {rt_1 job_7}", broker.lastID)
	}
}

// A provider belongs to one run. Rebuilding it per run is what makes a rotated
// secret visible to the next run without a release or a daemon restart.
func TestBrokerProviderSeesARotatedValueOnTheNextRun(t *testing.T) {
	broker, server := newFakeBroker(t)
	identity := WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}
	ctx := context.Background()

	firstRun := NewBrokerProvider(server.URL, "per-runtime-secret", identity, BrokerOptions{})
	before, err := firstRun.Get(ctx, "CLICKHOUSE_USER")
	if err != nil {
		t.Fatalf("first run get: %v", err)
	}
	if before != "reader" {
		t.Fatalf("first run value = %q, want reader", before)
	}

	broker.mu.Lock()
	broker.secrets["CLICKHOUSE_USER"] = "rotated"
	broker.mu.Unlock()

	secondRun := NewBrokerProvider(server.URL, "per-runtime-secret", identity, BrokerOptions{})
	after, err := secondRun.Get(ctx, "CLICKHOUSE_USER")
	if err != nil {
		t.Fatalf("second run get: %v", err)
	}
	if after != "rotated" {
		t.Errorf("second run value = %q, want rotated", after)
	}

	// The first run keeps what it resolved: a run must not observe a value
	// changing underneath it halfway through.
	if again, err := firstRun.Get(ctx, "CLICKHOUSE_USER"); err != nil || again != "reader" {
		t.Errorf("first run re-read = %q, %v; want reader, nil", again, err)
	}
}

func TestBrokerProviderMissingKeyIsNotFound(t *testing.T) {
	_, server := newFakeBroker(t)
	provider := NewBrokerProvider(server.URL, "per-runtime-secret",
		WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}, BrokerOptions{})

	if _, err := provider.Get(context.Background(), "NOT_DECLARED"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key error = %v, want ErrNotFound", err)
	}
}

// A blank value is a misconfiguration, not a usable secret.
func TestBrokerProviderTreatsBlankAsMissing(t *testing.T) {
	broker, server := newFakeBroker(t)
	broker.mu.Lock()
	broker.secrets["BLANK"] = "   "
	broker.mu.Unlock()

	provider := NewBrokerProvider(server.URL, "per-runtime-secret",
		WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}, BrokerOptions{})
	if _, err := provider.Get(context.Background(), "BLANK"); !errors.Is(err, ErrNotFound) {
		t.Errorf("blank value error = %v, want ErrNotFound", err)
	}
}

// A refused exchange must fail the run loudly rather than looking like a job
// that forgot to declare a variable.
func TestBrokerProviderRefusedExchangeIsNotNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "audience mismatch", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	provider := NewBrokerProvider(server.URL, "wrong-key",
		WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}, BrokerOptions{})

	_, err := provider.Get(context.Background(), "ANY")
	if err == nil {
		t.Fatal("want an error for a refused exchange")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("refused exchange reported as ErrNotFound: %v", err)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error %q does not say the broker refused", err)
	}
}

func TestBrokerProviderWithoutABrokerURLFails(t *testing.T) {
	provider := NewBrokerProvider("", "per-runtime-secret",
		WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}, BrokerOptions{})

	if _, err := provider.Get(context.Background(), "ANY"); err == nil {
		t.Fatal("want an error when no broker is configured")
	}
}

// The whole point of the audience: a token minted for one runtime is refused by
// a broker holding a different runtime's key.
func TestBrokerProviderRejectsAForeignRuntimeKey(t *testing.T) {
	broker, server := newFakeBroker(t) // broker holds "per-runtime-secret"
	provider := NewBrokerProvider(server.URL, "some-other-runtimes-secret",
		WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}, BrokerOptions{})

	if _, err := provider.Get(context.Background(), "SHOPIFY_CLIENT_ID"); err == nil {
		t.Fatal("want an error when the broker does not share the key")
	}
	if broker.count() != 1 {
		t.Errorf("broker requests = %d, want 1", broker.count())
	}
}
