package awssecrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/awscreds"
)

// fakeSecretsManager stands in for the AWS JSON API. It performs the checks the
// real service performs — the target header, the content type, a signed request
// — so a test failure here means the service would have refused the call.
type fakeSecretsManager struct {
	mu       sync.Mutex
	requests []recordedRequest
	// values maps a secret name to its value.
	values map[string]string
	// pageSize drives ListSecrets pagination; 0 means one page.
	pageSize int
}

type recordedRequest struct {
	target        string
	contentType   string
	authorization string
	body          []byte
}

func (f *fakeSecretsManager) record(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		target:        r.Header.Get("X-Amz-Target"),
		contentType:   r.Header.Get("Content-Type"),
		authorization: r.Header.Get("Authorization"),
		body:          body,
	})
	f.mu.Unlock()
	return body
}

func (f *fakeSecretsManager) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := f.record(t, r)

		// A real service refuses an unsigned or mislabelled request before it
		// looks at the payload.
		if r.Header.Get("Authorization") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			http.Error(w, `{"__type":"MissingAuthenticationTokenException","message":"no signature"}`, http.StatusBadRequest)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "content-type") ||
			!strings.Contains(r.Header.Get("Authorization"), "x-amz-target") {
			http.Error(w, `{"__type":"InvalidSignatureException","message":"content-type and x-amz-target must be signed"}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
			http.Error(w, `{"__type":"InvalidRequestException","message":"wrong content type"}`, http.StatusBadRequest)
			return
		}

		switch r.Header.Get("X-Amz-Target") {
		case "secretsmanager.GetSecretValue":
			var req struct {
				SecretID string `json:"SecretId"`
			}
			_ = json.Unmarshal(body, &req)
			f.mu.Lock()
			value, ok := f.values[req.SecretID]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"__type":"ResourceNotFoundException","message":"%s not found"}`, req.SecretID)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"SecretString": value})

		case "secretsmanager.ListSecrets":
			var req struct {
				NextToken string
			}
			_ = json.Unmarshal(body, &req)
			f.mu.Lock()
			var all []string
			for name := range f.values {
				all = append(all, name)
			}
			pageSize := f.pageSize
			f.mu.Unlock()
			sortStrings(all)

			start := 0
			if req.NextToken != "" {
				fmt.Sscanf(req.NextToken, "%d", &start)
			}
			end := len(all)
			next := ""
			if pageSize > 0 && start+pageSize < len(all) {
				end = start + pageSize
				next = fmt.Sprintf("%d", end)
			}
			list := make([]map[string]string, 0, end-start)
			for _, name := range all[start:end] {
				list = append(list, map[string]string{"Name": name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"SecretList": list, "NextToken": next})

		case "secretsmanager.BatchGetSecretValue":
			var req struct {
				SecretIDList []string `json:"SecretIdList"`
			}
			_ = json.Unmarshal(body, &req)
			if len(req.SecretIDList) > batchLimit {
				http.Error(w, `{"__type":"InvalidParameterException","message":"too many ids"}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			values := make([]map[string]string, 0, len(req.SecretIDList))
			for _, id := range req.SecretIDList {
				if value, ok := f.values[id]; ok {
					values = append(values, map[string]string{"Name": id, "SecretString": value})
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"SecretValues": values})

		default:
			http.Error(w, `{"__type":"UnknownOperationException","message":"unknown target"}`, http.StatusBadRequest)
		}
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func newTestClient(t *testing.T, fake *fakeSecretsManager) *Client {
	t.Helper()
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	return New("eu-west-1", awscreds.Static{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}, func(c *Client) {
		c.Endpoint = server.URL
		c.Now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }
	})
}

func TestGetSecretValueReturnsTheString(t *testing.T) {
	fake := &fakeSecretsManager{values: map[string]string{"otter/rt_a/job_1/TOKEN": "s3cr3t"}}
	client := newTestClient(t, fake)

	value, err := client.GetSecretValue(context.Background(), "otter/rt_a/job_1/TOKEN")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if value != "s3cr3t" {
		t.Errorf("value = %q, want s3cr3t", value)
	}

	// The request must be signed over the headers the service requires, or a
	// real endpoint refuses it.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	got := fake.requests[0]
	if got.target != "secretsmanager.GetSecretValue" {
		t.Errorf("target = %q", got.target)
	}
	if !strings.Contains(got.authorization, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-target") {
		t.Errorf("signed headers are not what the service requires:\n%s", got.authorization)
	}
}

func TestGetSecretValueMissingIsNotFound(t *testing.T) {
	client := newTestClient(t, &fakeSecretsManager{values: map[string]string{}})

	_, err := client.GetSecretValue(context.Background(), "otter/rt_a/job_1/ABSENT")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestGetSecretValueRefusesABinarySecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"SecretBinary": "AAAA"})
	}))
	t.Cleanup(server.Close)

	client := New("eu-west-1", awscreds.Static{AccessKeyID: "a", SecretAccessKey: "b"}, func(c *Client) {
		c.Endpoint = server.URL
	})
	if _, err := client.GetSecretValue(context.Background(), "otter/rt/job/BINARY"); err == nil {
		t.Fatal("a binary secret should be refused: it cannot be injected as an environment value")
	}
}

func TestListSecretNamesFollowsPagination(t *testing.T) {
	fake := &fakeSecretsManager{
		pageSize: 2,
		values: map[string]string{
			"otter/rt_a/job_1/A": "1",
			"otter/rt_a/job_1/B": "2",
			"otter/rt_a/job_1/C": "3",
			"otter/rt_a/job_1/D": "4",
			"otter/rt_a/job_1/E": "5",
		},
	}
	client := newTestClient(t, fake)

	names, err := client.ListSecretNames(context.Background(), "otter/rt_a/job_1/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 5 {
		t.Errorf("names = %v, want five", names)
	}
}

func TestBatchGetSecretValuesChunksBeyondTheAPILimit(t *testing.T) {
	values := map[string]string{}
	var ids []string
	for i := 0; i < batchLimit*2+3; i++ {
		name := fmt.Sprintf("otter/rt_a/job_1/SECRET_%02d", i)
		values[name] = fmt.Sprintf("value-%d", i)
		ids = append(ids, name)
	}
	fake := &fakeSecretsManager{values: values}
	client := newTestClient(t, fake)

	got, err := client.BatchGetSecretValues(context.Background(), ids)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(got) != len(ids) {
		t.Errorf("got %d values, want %d", len(got), len(ids))
	}
	// The fake refuses a chunk larger than the API limit, so reaching here
	// proves the chunking.
	fake.mu.Lock()
	requests := len(fake.requests)
	fake.mu.Unlock()
	if requests != 3 {
		t.Errorf("requests = %d, want 3 chunks for %d ids", requests, len(ids))
	}
}
