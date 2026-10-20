package secretbroker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tkoizumi/otter/internal/awscreds"
	"github.com/tkoizumi/otter/internal/awssecrets"
	"github.com/tkoizumi/otter/internal/secrets"
)

func newAWSStore(t *testing.T) (*AWSStore, *fakeSecretsManager) {
	t.Helper()
	backend := newFakeSecretsManager()
	aws := httptest.NewServer(backend)
	t.Cleanup(aws.Close)

	client := awssecrets.New("eu-west-1",
		awscreds.Static{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"},
		func(c *awssecrets.Client) { c.Endpoint = aws.URL })
	return &AWSStore{Client: client}, backend
}

// The document is exactly this job's secrets. A sibling job's and a sibling
// runtime's values must not appear: the namespace prefix is the boundary, and
// the trailing separator is what keeps job_1 from matching job_10.
func TestAWSStoreAssemblesOnlyTheJobsDocument(t *testing.T) {
	store, backend := newAWSStore(t)
	backend.rotate("otter/rt_a/job_1/SHOPIFY_CLIENT_ID", "id-1")
	backend.rotate("otter/rt_a/job_1/CLICKHOUSE_USER", "reader")
	backend.rotate("otter/rt_a/job_10/LEAK", "sibling-job")
	backend.rotate("otter/rt_b/job_1/LEAK", "sibling-runtime")
	backend.rotate("otter/rt_a/job_1/../X", "traversal")

	document, err := store.Document(context.Background(), "otter/rt_a/job_1")
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	if len(document) != 2 {
		t.Fatalf("document = %v, want exactly job_1's two secrets", document)
	}
	if document["SHOPIFY_CLIENT_ID"] != "id-1" || document["CLICKHOUSE_USER"] != "reader" {
		t.Errorf("document = %v", document)
	}
	for _, leaked := range []string{"LEAK", "../X", "sibling-job", "sibling-runtime", "traversal"} {
		if _, ok := document[leaked]; ok {
			t.Errorf("%q reached the document: %v", leaked, document)
		}
	}
}

// A job with nothing written is a clear answer, not an empty environment.
func TestAWSStoreWithoutSecretsIsNotFound(t *testing.T) {
	store, _ := newAWSStore(t)

	_, err := store.Document(context.Background(), "otter/rt_a/job_1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want the broker's ErrNotFound", err)
	}
}

// The regression this move fixed: the adapter used to return a DIFFERENT
// package's not-found error, so the broker answered 500 for a job with no
// secrets -- an operator would have seen an internal error for a configuration
// state.
func TestAWSStoreNotFoundIsRecognisedByTheBroker(t *testing.T) {
	store, _ := newAWSStore(t)
	broker := New(MapCredentials{"rt_a": runtimeKey}, store, Options{})

	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"})
	rec := exchange(t, broker, "rt_a", token)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d (%s), want 404", rec.Code, rec.Body.String())
	}
}

// The policy in `otter-platform/docs/secrets-broker-iam.md` grants exactly TWO
// actions: `ListSecrets` and `BatchGetSecretValue`. A policy is only correct if
// the code needs no more than it grants and no less, so the actions a document
// fetch actually presents are recorded and pinned here.
//
// Without this, a call added to the read path later would need a policy change
// that nothing would remind anyone to make: on a real host it would fail with
// AccessDenied on the first exchange, and every test in this package would still
// pass, because they all run against a fake that grants everything.
func TestFetchingADocumentPresentsExactlyTheActionsThePolicyGrants(t *testing.T) {
	backend := newFakeSecretsManager()
	backend.rotate("otter/rt_a/job_1/SHOPIFY_CLIENT_ID", "id-1")

	var mu sync.Mutex
	presented := map[string]bool{}
	aws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		presented[r.Header.Get("X-Amz-Target")] = true
		mu.Unlock()
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(aws.Close)

	client := awssecrets.New("eu-west-1",
		awscreds.Static{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"},
		func(c *awssecrets.Client) { c.Endpoint = aws.URL })
	store := &AWSStore{Client: client}

	_, docErr := store.Document(context.Background(), "otter/rt_a/job_1")

	mu.Lock()
	snapshot := map[string]bool{}
	for action := range presented {
		snapshot[action] = true
	}
	mu.Unlock()

	// Checked even when the fetch failed. A read path that presents an action the
	// policy does not grant IS the finding; a fake refusing that action is only
	// how it becomes visible, and its complaint would otherwise be the whole
	// message.
	want := map[string]bool{
		"secretsmanager.ListSecrets":         true,
		"secretsmanager.BatchGetSecretValue": true,
	}
	for action := range want {
		if !snapshot[action] {
			t.Errorf("the read path did not present %s, so the policy grants an action the code does not use", action)
		}
	}
	for action := range snapshot {
		if !want[action] {
			t.Errorf("the read path presented %s, which otter-platform/docs/secrets-broker-iam.md does NOT grant; "+
				"on a real host that is AccessDenied on the first exchange, so either the policy or the code has to change", action)
		}
	}

	// The code never performs a single-value read, and that is worth pinning --
	// but note what it does NOT pin. The policy must still GRANT GetSecretValue,
	// because Secrets Manager refuses BatchGetSecretValue without it ("you must
	// have secretsmanager:GetSecretValue for each secret"). So the broker's policy
	// is deliberately larger than the actions its code presents, unlike the
	// writer's, which is pinned to match. A policy-versus-code equality check here
	// would have rejected the only configuration that works.
	if snapshot["secretsmanager.GetSecretValue"] {
		t.Error("the read path presented GetSecretValue; the document is assembled by listing and batch-getting")
	}

	if docErr != nil {
		t.Fatalf("document: %v", docErr)
	}
}
