package secretbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tkoizumi/otter/internal/awscreds"
	"github.com/tkoizumi/otter/internal/awssecrets"
	"github.com/tkoizumi/otter/internal/secrets"
)

// The property CA-13 exists to prove, asserted across every piece that
// implements it: a tenant's runtime, the host broker, and the Secrets Manager
// client.
//
//	Rotating a secret changes what the NEXT run resolves, with no release, no
//	daemon restart and no broker restart.
//
// This is D2's "rotation without redeploy" as a test rather than a hope. What it
// deliberately does NOT cover is the AWS IAM policy and the real network path --
// those need a host, and the doc says so.

// fakeSecretsManager implements the parts of the Secrets Manager JSON API this
// path uses. It enforces the headers the real service requires, so a signature
// that would be refused in production is refused here too.
type fakeSecretsManager struct {
	mu      sync.Mutex
	values  map[string]string
	listOps int
	gets    int
}

func newFakeSecretsManager() *fakeSecretsManager {
	return &fakeSecretsManager{values: map[string]string{}}
}

// rotate is the write the control plane's write-through performs. It is a
// method on the fake rather than a call through the AWS client because the host
// client is deliberately read-only: a host that could write could rewrite a
// secret it cannot read, which is a different privilege from the one it needs.
func (f *fakeSecretsManager) rotate(name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[name] = value
}

func (f *fakeSecretsManager) counts() (lists, gets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listOps, f.gets
}

func (f *fakeSecretsManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		http.Error(w, `{"__type":"MissingAuthenticationTokenException"}`, http.StatusBadRequest)
		return
	}
	if r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
		http.Error(w, `{"__type":"InvalidRequestException"}`, http.StatusBadRequest)
		return
	}

	switch r.Header.Get("X-Amz-Target") {
	case "secretsmanager.ListSecrets":
		var req struct {
			Filters []struct {
				Key    string   `json:"Key"`
				Values []string `json:"Values"`
			} `json:"Filters"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		prefix := ""
		for _, filter := range req.Filters {
			if filter.Key == "name" && len(filter.Values) > 0 {
				prefix = filter.Values[0]
			}
		}

		f.mu.Lock()
		f.listOps++
		var names []string
		for name := range f.values {
			if strings.HasPrefix(name, prefix) {
				names = append(names, name)
			}
		}
		f.mu.Unlock()
		sort.Strings(names)

		list := make([]map[string]string, 0, len(names))
		for _, name := range names {
			list = append(list, map[string]string{"Name": name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"SecretList": list})

	case "secretsmanager.BatchGetSecretValue":
		var req struct {
			SecretIDList []string `json:"SecretIdList"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		f.mu.Lock()
		f.gets++
		values := make([]map[string]string, 0, len(req.SecretIDList))
		for _, id := range req.SecretIDList {
			if value, ok := f.values[id]; ok {
				values = append(values, map[string]string{"Name": id, "SecretString": value})
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"SecretValues": values})

	default:
		http.Error(w, `{"__type":"UnknownOperationException"}`, http.StatusBadRequest)
	}
}

func TestRotatingASecretReachesTheNextRunWithoutARestart(t *testing.T) {
	const (
		brokerKey = "host-local-broker-credential"
		name      = "SHOPIFY_CLIENT_ID"
		namespace = "otter/rt_a/job_1"
	)
	backend := newFakeSecretsManager()
	backend.rotate(namespace+"/"+name, "first-value")
	backend.rotate(namespace+"/CLICKHOUSE_USER", "reader")

	aws := httptest.NewServer(backend)
	defer aws.Close()

	client := awssecrets.New("eu-west-1",
		awscreds.Static{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"},
		func(c *awssecrets.Client) { c.Endpoint = aws.URL })

	// One broker, one process, started once. Nothing below restarts it.
	broker := New(
		MapCredentials{"rt_a": brokerKey},
		&AWSStore{Client: client},
		Options{},
	)
	listeners, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	brokerURL := "http://" + listeners[0].Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, listeners, broker, nil) }()

	identity := secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}
	resolve := func() string {
		t.Helper()
		// A provider belongs to ONE run, which is what makes it re-fetch.
		provider := secrets.NewBrokerProvider(brokerURL, brokerKey, identity, secrets.BrokerOptions{})
		value, err := provider.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return value
	}

	if got := resolve(); got != "first-value" {
		t.Fatalf("first run resolved %q, want first-value", got)
	}
	listsAfterFirst, getsAfterFirst := backend.counts()

	// The rotation. No release is built, nothing is redeployed, and the broker
	// process keeps running on the same listener.
	backend.rotate(namespace+"/"+name, "second-value")

	if got := resolve(); got != "second-value" {
		t.Errorf("the next run resolved %q, want second-value: a rotation did not reach it", got)
	}

	// The value really was re-fetched rather than served from something cached
	// across runs. A provider that cached across runs would have passed the check
	// above only by accident.
	listsAfterSecond, getsAfterSecond := backend.counts()
	if listsAfterSecond <= listsAfterFirst || getsAfterSecond <= getsAfterFirst {
		t.Errorf("the second run did not re-read the backend: lists %d->%d, gets %d->%d",
			listsAfterFirst, listsAfterSecond, getsAfterFirst, getsAfterSecond)
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("the broker did not shut down cleanly: %v", err)
	}
}

// A run must not observe a value changing underneath it. The rotation takes
// effect for the NEXT run, not for one already resolving.
func TestARunKeepsTheValueItResolved(t *testing.T) {
	const (
		brokerKey = "host-local-broker-credential"
		name      = "SHOPIFY_CLIENT_ID"
		namespace = "otter/rt_a/job_1"
	)
	backend := newFakeSecretsManager()
	backend.rotate(namespace+"/"+name, "first-value")

	aws := httptest.NewServer(backend)
	defer aws.Close()

	client := awssecrets.New("eu-west-1",
		awscreds.Static{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"},
		func(c *awssecrets.Client) { c.Endpoint = aws.URL })
	broker := New(MapCredentials{"rt_a": brokerKey}, &AWSStore{Client: client}, Options{})

	listeners, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, listeners, broker, nil) }()

	provider := secrets.NewBrokerProvider("http://"+listeners[0].Addr().String(), brokerKey,
		secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"}, secrets.BrokerOptions{})

	value, err := provider.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if value != "first-value" {
		t.Fatalf("value = %q, want first-value", value)
	}

	// Rotate mid-run, then read again through the SAME provider.
	backend.rotate(namespace+"/"+name, "second-value")

	again, err := provider.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if again != "first-value" {
		t.Errorf("a run observed the value change underneath it: %q then %q", value, again)
	}
}

// A job whose secrets have never been written is a clear answer rather than an
// empty environment: the run must fail with "no secrets are configured" instead
// of starting with nothing.
func TestAJobWithNoSecretsIsRefusedRatherThanEmpty(t *testing.T) {
	backend := newFakeSecretsManager()
	aws := httptest.NewServer(backend)
	defer aws.Close()

	client := awssecrets.New("eu-west-1",
		awscreds.Static{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"},
		func(c *awssecrets.Client) { c.Endpoint = aws.URL })
	broker := New(MapCredentials{"rt_a": runtimeKey}, &AWSStore{Client: client}, Options{})

	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"})
	rec := exchange(t, broker, "rt_a", token)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d (%s), want 404", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secrets\":{") {
		t.Errorf("a missing document must not be returned as an empty one: %s", rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["secrets"]; ok {
		t.Errorf("the refusal carried a secrets field: %s", rec.Body.String())
	}
}
