package awscreds

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIMDS implements the IMDSv2 flow: a token, then a token-authenticated read.
// It enforces the token on reads, because a reader that skipped it would work
// against this fake and fail on a real instance.
type fakeIMDS struct {
	token       string
	tokenStatus int
	responses   map[string]string
	statuses    map[string]int

	mu             sync.Mutex
	tokenRequests  int
	valueRequests  int
	sawTokenHeader string
	requireTokenOn bool
}

func newFakeIMDS() *fakeIMDS {
	return &fakeIMDS{
		token:          "imds-token",
		responses:      map[string]string{},
		statuses:       map[string]int{},
		requireTokenOn: true,
	}
}

func (f *fakeIMDS) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" {
			f.mu.Lock()
			f.tokenRequests++
			f.mu.Unlock()
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				http.Error(w, "ttl header required", http.StatusBadRequest)
				return
			}
			if f.tokenStatus != 0 {
				w.WriteHeader(f.tokenStatus)
				return
			}
			_, _ = w.Write([]byte(f.token))
			return
		}

		f.mu.Lock()
		f.valueRequests++
		f.sawTokenHeader = r.Header.Get("X-aws-ec2-metadata-token")
		f.mu.Unlock()

		if f.requireTokenOn && r.Header.Get("X-aws-ec2-metadata-token") != f.token {
			http.Error(w, "token required", http.StatusUnauthorized)
			return
		}
		if status, ok := f.statuses[r.URL.Path]; ok {
			w.WriteHeader(status)
			return
		}
		body, ok := f.responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

func newTestIMDS(t *testing.T, fake *fakeIMDS) *IMDS {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	return &IMDS{Base: server.URL}
}

func TestGetUsesIMDSv2(t *testing.T) {
	fake := newFakeIMDS()
	fake.responses["/latest/meta-data/instance-id"] = "i-0abc"
	imds := newTestIMDS(t, fake)

	value, err := imds.Get(context.Background(), "/latest/meta-data/instance-id")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if value != "i-0abc" {
		t.Errorf("value = %q, want i-0abc", value)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.tokenRequests != 1 {
		t.Errorf("token requests = %d, want 1", fake.tokenRequests)
	}
	if fake.sawTokenHeader != "imds-token" {
		t.Errorf("read presented token %q, want imds-token", fake.sawTokenHeader)
	}
}

// IMDSv2 disabled is a configuration refusal, not a transient fault: retrying
// cannot enable it, and the caller must not treat it as something to wait out.
func TestGetReportsIMDSv2DisabledAsRefused(t *testing.T) {
	fake := newFakeIMDS()
	fake.tokenStatus = http.StatusUnauthorized
	imds := newTestIMDS(t, fake)

	_, err := imds.Get(context.Background(), "/latest/meta-data/instance-id")
	if !errors.Is(err, ErrRefused) {
		t.Errorf("error = %v, want ErrRefused", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("a disabled IMDSv2 must not also read as unavailable: %v", err)
	}
}

// A path that is not attached to the instance is a refusal: there is nothing to
// wait for.
func TestGetReportsAMissingPathAsRefused(t *testing.T) {
	imds := newTestIMDS(t, newFakeIMDS())

	_, err := imds.Get(context.Background(), "/latest/meta-data/iam/security-credentials/")
	if !errors.Is(err, ErrRefused) {
		t.Errorf("error = %v, want ErrRefused", err)
	}
}

func TestGetReportsAServerErrorAsUnavailable(t *testing.T) {
	fake := newFakeIMDS()
	fake.statuses["/latest/meta-data/instance-id"] = http.StatusInternalServerError
	imds := newTestIMDS(t, fake)

	_, err := imds.Get(context.Background(), "/latest/meta-data/instance-id")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable", err)
	}
}

func TestRegionReadsTheIdentityDocument(t *testing.T) {
	fake := newFakeIMDS()
	fake.responses["/latest/dynamic/instance-identity/document"] = `{"region":"eu-west-1","accountId":"123"}`
	imds := newTestIMDS(t, fake)

	region, err := imds.Region(context.Background())
	if err != nil {
		t.Fatalf("region: %v", err)
	}
	if region != "eu-west-1" {
		t.Errorf("region = %q, want eu-west-1", region)
	}
}

func TestCredentialsReadsTheRoleTheInstanceReports(t *testing.T) {
	fake := newFakeIMDS()
	fake.responses["/latest/meta-data/iam/security-credentials/"] = "otter-agent\n"
	fake.responses["/latest/meta-data/iam/security-credentials/otter-agent"] =
		`{"AccessKeyId":"AKID","SecretAccessKey":"SECRET","Token":"SESSION","Expiration":"2026-10-15T18:00:00Z"}`
	imds := newTestIMDS(t, fake)

	cred, err := imds.Credentials(context.Background())
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if cred.AccessKeyID != "AKID" || cred.SecretAccessKey != "SECRET" || cred.SessionToken != "SESSION" {
		t.Errorf("credential = %+v", cred)
	}
	if cred.Expiration.IsZero() {
		t.Error("expiration was not parsed; a cache cannot refresh without it")
	}
	if !cred.Usable() {
		t.Error("a complete credential should be usable")
	}
}

func TestCredentialsRefusesWhenNoRoleIsAttached(t *testing.T) {
	fake := newFakeIMDS()
	fake.responses["/latest/meta-data/iam/security-credentials/"] = "\n"
	imds := newTestIMDS(t, fake)

	_, err := imds.Credentials(context.Background())
	if !errors.Is(err, ErrRefused) {
		t.Errorf("error = %v, want ErrRefused", err)
	}
}

func TestSourceCachesUntilExpiry(t *testing.T) {
	fake := newFakeIMDS()
	fake.responses["/latest/meta-data/iam/security-credentials/"] = "otter-agent"
	fake.responses["/latest/meta-data/iam/security-credentials/otter-agent"] =
		`{"AccessKeyId":"AKID","SecretAccessKey":"SECRET","Expiration":"2026-10-15T18:00:00Z"}`
	imds := newTestIMDS(t, fake)

	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	source := &Source{IMDS: imds, Now: func() time.Time { return now }}

	for i := 0; i < 3; i++ {
		if _, err := source.Retrieve(context.Background()); err != nil {
			t.Fatalf("retrieve %d: %v", i, err)
		}
	}
	fake.mu.Lock()
	reads := fake.valueRequests
	fake.mu.Unlock()
	// One role-name read and one credential read, then served from cache.
	if reads != 2 {
		t.Errorf("metadata reads = %d, want 2 (a cache should serve the rest)", reads)
	}

	// Inside the refresh window the source fetches again.
	now = now.Add(5*time.Hour + 56*time.Minute)
	if _, err := source.Retrieve(context.Background()); err != nil {
		t.Fatalf("retrieve after expiry window: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.valueRequests <= reads {
		t.Error("a credential inside the refresh window was not refreshed")
	}
}

// A refresh that fails must not throw away a credential that is still valid:
// a transient metadata blip should not stop the caller signing.
func TestSourceKeepsAValidCredentialWhenRefreshFails(t *testing.T) {
	fake := newFakeIMDS()
	fake.responses["/latest/meta-data/iam/security-credentials/"] = "otter-agent"
	fake.responses["/latest/meta-data/iam/security-credentials/otter-agent"] =
		`{"AccessKeyId":"AKID","SecretAccessKey":"SECRET","Expiration":"2026-10-15T18:00:00Z"}`
	imds := newTestIMDS(t, fake)

	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	source := &Source{IMDS: imds, Now: func() time.Time { return now }}

	if _, err := source.Retrieve(context.Background()); err != nil {
		t.Fatalf("first retrieve: %v", err)
	}

	// Move inside the refresh window, then break the metadata service.
	now = now.Add(5*time.Hour + 56*time.Minute)
	fake.mu.Lock()
	fake.statuses["/latest/meta-data/iam/security-credentials/"] = http.StatusInternalServerError
	fake.mu.Unlock()

	cred, err := source.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("a failed refresh should fall back to the still-valid credential: %v", err)
	}
	if cred.AccessKeyID != "AKID" {
		t.Errorf("credential = %+v, want the cached one", cred)
	}
}

func TestStaticRefusesAnIncompleteCredential(t *testing.T) {
	if _, err := (Static{AccessKeyID: "AKID"}).Retrieve(context.Background()); err == nil {
		t.Error("a credential with no secret should be refused, not returned")
	}
	if _, err := (Static{AccessKeyID: "AKID", SecretAccessKey: "S"}).Retrieve(context.Background()); err != nil {
		t.Errorf("a complete static credential should be returned: %v", err)
	}
}

func TestFirstLineHandlesBothEndings(t *testing.T) {
	for in, want := range map[string]string{
		"otter-agent\n":   "otter-agent",
		"otter-agent\r\n": "otter-agent",
		"otter-agent":     "otter-agent",
		"":                "",
		"a\nb":            "a",
	} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
	if !strings.Contains(DefaultBase, "169.254.169.254") {
		t.Errorf("DefaultBase = %q, want the link-local address", DefaultBase)
	}
}
