package secretbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/secrets"
)

// The AWS-backed document store must keep satisfying the seam the broker reads
// through. Without this the drift would only surface on a real host, where the
// failure is a run that cannot resolve its secrets.
var _ Store = (*AWSStore)(nil)

const runtimeKey = "runtime-a-broker-credential"

func mint(t *testing.T, key string, id secrets.WorkloadIdentity) string {
	t.Helper()
	token, err := secrets.MintWorkloadToken(key, id, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return token
}

func exchange(t *testing.T, broker *Broker, runtimeID, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"runtime_id": runtimeID, "token": token})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, PathExchange, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	broker.ServeHTTP(rec, req)
	return rec
}

func decodeSecrets(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var payload struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return payload.Secrets
}

func newTestBroker(store Store, opts Options) *Broker {
	credentials := MapCredentials{
		"rt_a": runtimeKey,
		"rt_b": "runtime-b-broker-credential",
	}
	return New(credentials, store, opts)
}

func TestExchangeReturnsTheJobsDocument(t *testing.T) {
	store := MapStore{
		"otter/rt_a/job_1": {"SHOPIFY_CLIENT_ID": "id-1", "CLICKHOUSE_USER": "reader"},
	}
	broker := newTestBroker(store, Options{})

	rec := exchange(t, broker, "rt_a", mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	got := decodeSecrets(t, rec)
	if got["SHOPIFY_CLIENT_ID"] != "id-1" || got["CLICKHOUSE_USER"] != "reader" {
		t.Errorf("secrets = %v", got)
	}
}

// A tenant holding its own credential cannot read a sibling's document: the
// claim selects the key, and it does not have the sibling's.
func TestExchangeRejectsATokenSignedByAnotherRuntime(t *testing.T) {
	store := MapStore{"otter/rt_b/job_1": {"SECRET": "sibling-value"}}
	broker := newTestBroker(store, Options{})

	// Signed with rt_a's key, presented as rt_b.
	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_b", JobID: "job_1"})
	rec := exchange(t, broker, "rt_b", token)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sibling-value") {
		t.Errorf("response leaked a value: %s", rec.Body.String())
	}
}

// Possessing a valid key does not let a runtime claim to be a different one:
// the runtime id inside the signature must match the runtime id on the request.
func TestExchangeRejectsAnAudienceMismatch(t *testing.T) {
	broker := newTestBroker(MapStore{}, Options{})

	// rt_a's key, but the signed claim names rt_b.
	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_b", JobID: "job_1"})
	rec := exchange(t, broker, "rt_a", token)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d (%s), want 403", rec.Code, rec.Body.String())
	}
}

// An unauthenticated caller learns nothing about which runtimes exist: an
// unknown runtime and a bad token are the same answer.
func TestExchangeDoesNotRevealWhichRuntimesExist(t *testing.T) {
	broker := newTestBroker(MapStore{}, Options{})

	unknown := exchange(t, broker, "rt_does_not_exist", "some-token")
	badToken := exchange(t, broker, "rt_a", "not-a-real-token")

	if unknown.Code != http.StatusUnauthorized || badToken.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401 and 401", unknown.Code, badToken.Code)
	}
	if unknown.Body.String() != badToken.Body.String() {
		t.Errorf("unknown runtime body %q differs from bad token body %q",
			unknown.Body.String(), badToken.Body.String())
	}
}

func TestExchangeRejectsATraversalJobID(t *testing.T) {
	// A store that fails the test if it is ever reached: the request must be
	// refused before any lookup.
	store := StoreFunc(func(_ context.Context, namespace string) (map[string]string, error) {
		t.Errorf("the store was queried for %q; a traversal must not reach it", namespace)
		return nil, ErrNotFound
	})
	broker := newTestBroker(store, Options{})

	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "../../rt_b/job_1"})
	rec := exchange(t, broker, "rt_a", token)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d (%s), want 403", rec.Code, rec.Body.String())
	}
}

// A runtime may not read a sibling job's secrets of its own tenant when the
// broker can answer the membership question.
func TestExchangeRejectsAJobThatIsNotThisRuntimes(t *testing.T) {
	jobs := fakeJobs{"rt_a": {"job_1": true}}
	store := MapStore{"otter/rt_a/job_2": {"SECRET": "another-jobs-value"}}
	broker := newTestBroker(store, Options{Jobs: jobs})

	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_2"})
	rec := exchange(t, broker, "rt_a", token)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d (%s), want 403", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "another-jobs-value") {
		t.Errorf("response leaked a value: %s", rec.Body.String())
	}
}

type fakeJobs map[string]map[string]bool

func (f fakeJobs) HasJob(_ context.Context, runtimeID, jobID string) (bool, error) {
	return f[runtimeID][jobID], nil
}

func TestExchangeUnconfiguredJobIsNotFound(t *testing.T) {
	broker := newTestBroker(MapStore{}, Options{})
	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"})

	if rec := exchange(t, broker, "rt_a", token); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A backend error can name paths and account ids, so it is never returned.
func TestExchangeDoesNotLeakABackendError(t *testing.T) {
	store := StoreFunc(func(context.Context, string) (map[string]string, error) {
		return nil, errors.New("AccessDenied: arn:aws:secretsmanager:eu-west-1:123456789012:secret:otter/rt_a/job_1")
	})
	broker := newTestBroker(store, Options{})
	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"})

	rec := exchange(t, broker, "rt_a", token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	for _, leak := range []string{"AccessDenied", "arn:aws", "123456789012"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response leaked %q: %s", leak, rec.Body.String())
		}
	}
}

func TestExchangeRejectsBadRequests(t *testing.T) {
	broker := newTestBroker(MapStore{}, Options{})

	empty := exchange(t, broker, "", "")
	if empty.Code != http.StatusUnauthorized {
		t.Errorf("empty request status = %d, want 401", empty.Code)
	}

	malformed := httptest.NewRecorder()
	broker.ServeHTTP(malformed, httptest.NewRequest(http.MethodPost, PathExchange, strings.NewReader("{not json")))
	if malformed.Code != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", malformed.Code)
	}

	wrongMethod := httptest.NewRecorder()
	broker.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodGet, PathExchange, nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", wrongMethod.Code)
	}

	wrongPath := httptest.NewRecorder()
	broker.ServeHTTP(wrongPath, httptest.NewRequest(http.MethodPost, "/v1/secrets", strings.NewReader("{}")))
	if wrongPath.Code != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want 404", wrongPath.Code)
	}
}

func TestNamespaceValidatesBothSegments(t *testing.T) {
	if got, err := Namespace("rt_a", "job_1"); err != nil || got != "otter/rt_a/job_1" {
		t.Fatalf("Namespace = %q, %v", got, err)
	}

	for name, pair := range map[string][2]string{
		"empty runtime":       {"", "job"},
		"empty job":           {"rt", ""},
		"traversal runtime":   {"..", "job"},
		"traversal job":       {"rt", ".."},
		"slash in job":        {"rt", "a/b"},
		"backslash in job":    {"rt", `a\b`},
		"slash in runtime":    {"a/b", "job"},
		"space in job":        {"rt", "a b"},
		"nul in job":          {"rt", "a\x00b"},
		"dotted traversal":    {"rt", "a/../b"},
		"overlong runtime id": {strings.Repeat("r", 129), "job"},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := Namespace(pair[0], pair[1]); err == nil {
				t.Errorf("Namespace(%q, %q) = %q, want an error", pair[0], pair[1], got)
			}
		})
	}
}

func TestMapCredentialsKeysAreSortedAndDiscloseNoValues(t *testing.T) {
	keys := MapCredentials{"rt_b": "secret-b", "rt_a": "secret-a"}.Keys()
	if len(keys) != 2 || keys[0] != "rt_a" || keys[1] != "rt_b" {
		t.Errorf("Keys() = %v, want [rt_a rt_b]", keys)
	}
}

// The strongest check available without AWS: the runtime's real client against
// the host's real handler, over a real socket. Two fakes that agree prove the
// fakes agree; this proves the protocol does.
func TestRuntimeClientAndHostBrokerAgreeEndToEnd(t *testing.T) {
	credentials := MapCredentials{"rt_a": runtimeKey}
	store := MapStore{"otter/rt_a/job_1": {"SHOPIFY_CLIENT_ID": "id-1"}}
	server := httptest.NewServer(New(credentials, store, Options{}))
	defer server.Close()

	identity := secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}
	provider := secrets.NewBrokerProvider(server.URL, runtimeKey, identity, secrets.BrokerOptions{})

	value, err := provider.Get(context.Background(), "SHOPIFY_CLIENT_ID")
	if err != nil {
		t.Fatalf("resolve through the broker: %v", err)
	}
	if value != "id-1" {
		t.Errorf("value = %q, want id-1", value)
	}

	// A runtime the broker does not hold is refused, and the refusal must not
	// look like a job that forgot to declare a variable.
	foreign := secrets.NewBrokerProvider(server.URL, "runtime-b-broker-credential",
		secrets.WorkloadIdentity{RuntimeID: "rt_b", JobID: "job_1"}, secrets.BrokerOptions{})
	_, err = foreign.Get(context.Background(), "SHOPIFY_CLIENT_ID")
	if err == nil {
		t.Fatal("an unknown runtime resolved a document")
	}
	if errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("a refused exchange was reported as ErrNotFound: %v", err)
	}
}

// --- the tenant tier ---------------------------------------------------------

// A tenant's shared secrets are stored once and reach every job; a job's own
// namespace overrides them. Sharing is a default, not a ceiling: a job that needs
// a different value still gets one, which is why precedence is job-first.
func TestExchangeMergesTheTenantsSharedDocument(t *testing.T) {
	store := MapStore{
		"otter/rt_a/_tenant": {"SHOPIFY_CLIENT_ID": "shared-id", "CLICKHOUSE_USER": "shared-reader"},
		"otter/rt_a/job_1":   {"CLICKHOUSE_USER": "job-1-reader"},
	}
	broker := newTestBroker(store, Options{})

	rec := exchange(t, broker, "rt_a", mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	got := decodeSecrets(t, rec)
	if got["SHOPIFY_CLIENT_ID"] != "shared-id" {
		t.Errorf("the tenant's shared value did not reach the job: %v", got)
	}
	if got["CLICKHOUSE_USER"] != "job-1-reader" {
		t.Errorf("the job's own value did not override the shared one: %v", got)
	}
}

func TestExchangeServesSharedSecretsToAJobWithNoneOfItsOwn(t *testing.T) {
	store := MapStore{"otter/rt_a/_tenant": {"SHOPIFY_CLIENT_ID": "shared-id"}}
	broker := newTestBroker(store, Options{})

	rec := exchange(t, broker, "rt_a", mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_9"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := decodeSecrets(t, rec)["SHOPIFY_CLIENT_ID"]; got != "shared-id" {
		t.Errorf("SHOPIFY_CLIENT_ID = %q, want the shared value", got)
	}
}

// Empty in both tiers is still the "nothing configured" answer, not an empty
// document a job would silently start with.
func TestExchangeIsNotFoundWhenNeitherTierHasAnything(t *testing.T) {
	broker := newTestBroker(MapStore{}, Options{})
	rec := exchange(t, broker, "rt_a", mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// Sharing adds a tier; it does not widen the job tier. A sibling's own namespace
// stays unreadable, which is what D2's job binding buys.
func TestExchangeDoesNotReachASiblingsNamespace(t *testing.T) {
	store := MapStore{"otter/rt_a/job_2": {"SHOPIFY_CLIENT_ID": "sibling-id"}}
	broker := newTestBroker(store, Options{})

	rec := exchange(t, broker, "rt_a", mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: a sibling's namespace must not be served", rec.Code)
	}
}

// The reserved space, which is what keeps the shared tier from also being
// somebody's private one.
func TestReservedSegmentsAreRefused(t *testing.T) {
	if _, err := Namespace("rt_a", "_tenant"); err == nil {
		t.Error("a job named _tenant was accepted; it would be served the shared document as its own")
	}
	if _, err := Namespace("_tenant", "job_1"); err == nil {
		t.Error("a runtime named _tenant was accepted")
	}
	if _, err := TenantNamespace(""); err == nil {
		t.Error("an empty runtime id was accepted")
	}
	got, err := TenantNamespace("rt_a")
	if err != nil || got != "otter/rt_a/_tenant" {
		t.Errorf("TenantNamespace(rt_a) = %q, %v", got, err)
	}
}

// And the refusal is enforced at the endpoint, not only in the builder.
func TestExchangeRejectsAReservedJobID(t *testing.T) {
	store := MapStore{"otter/rt_a/_tenant": {"SHOPIFY_CLIENT_ID": "shared-id"}}
	broker := newTestBroker(store, Options{})

	rec := exchange(t, broker, "rt_a", mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "_tenant"}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// A backend failure is answered generically -- the store's error can name S3
// paths and account ids, and a tenant must not see them -- which makes the log
// the only place the cause appears. Without it, a broker that cannot reach
// Secrets Manager looks exactly like one that reached it and found nothing.
func TestABackendFailureIsLoggedButNotReturned(t *testing.T) {
	var logged []string
	broker := newTestBroker(failingStore{err: errors.New("AccessDenied: arn:aws:secretsmanager:us-east-1:426714791664:secret:otter/rt_a")},
		Options{Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }})

	rec := exchange(t, broker, "rt_a", mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "426714791664") {
		t.Errorf("the response disclosed the account id: %s", rec.Body.String())
	}
	if len(logged) == 0 {
		t.Fatal("the backend failure was not logged, so it is nowhere at all")
	}
	if !strings.Contains(strings.Join(logged, "\n"), "AccessDenied") {
		t.Errorf("the log does not carry the cause: %v", logged)
	}
}

type failingStore struct{ err error }

func (f failingStore) Document(context.Context, string) (map[string]string, error) {
	return nil, f.err
}
