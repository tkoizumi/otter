package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// The tests in this file are the evidence for CL-21: a runtime can issue a
// credential limited to the gateway's command surface, that credential is
// refused everywhere else, and revocation takes effect on the next request.

const adminToken = "admin-secret"

// mintToken creates a token through the API, as an operator would, rather than
// reaching into the store. It returns the one response that carries the token.
func mintToken(t *testing.T, srv *httptest.Server, name string, scope Scope) APITokenCreated {
	t.Helper()
	body, err := json.Marshal(CreateAPITokenRequest{Name: name, Scope: scope})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	res := do(t, http.MethodPost, srv.URL+"/v1/tokens", body, bearer(adminToken))
	if res.status != http.StatusCreated {
		t.Fatalf("create %s token: status %d, body %s", scope, res.status, res.body)
	}
	var created APITokenCreated
	if err := json.Unmarshal(res.body, &created); err != nil {
		t.Fatalf("decode created token: %v", err)
	}
	if created.Token == "" {
		t.Fatal("created token carries no token")
	}
	if created.ID == "" {
		t.Fatal("created token carries no id")
	}
	return created
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// scopedFixture builds a server with one job, one run and one token of each
// scope, plus the raw tokens.
func scopedFixture(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	b := newFakeBackend()
	b.addJob("job-A", true, "webhook-secret-for-job-A")
	now := time.Now().UTC()
	b.addRun(&runs.Run{ID: "run-A", JobID: "job-A", Status: runs.StatusSucceeded, Attempt: 1, CreatedAt: now})
	b.timelinePage = &timeline.Page{Context: timeline.Context{RunID: "run-A", JobID: "job-A"}}

	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	t.Cleanup(srv.Close)

	control := mintToken(t, srv, "cloud-gateway", ScopeControl)
	read := mintToken(t, srv, "castor-backend", ScopeRead)
	return srv, control.Token, read.Token
}

// TestControlTokenReachesTheGatewayAndNothingElse is the central CL-21 claim.
// Every probe names the route and which side of the boundary it falls on, so a
// route that drifts onto the wrong gate fails here rather than in production.
func TestControlTokenReachesTheGatewayAndNothingElse(t *testing.T) {
	srv, controlToken, _ := scopedFixture(t)
	auth := bearer(controlToken)

	gateway := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"list jobs", http.MethodGet, "/v1/jobs", nil},
		{"read a job", http.MethodGet, "/v1/jobs/job-A", nil},
		{"list runs", http.MethodGet, "/v1/runs", nil},
		{"read a run", http.MethodGet, "/v1/runs/run-A", nil},
		{"read run output", http.MethodGet, "/v1/runs/run-A/logs", nil},
		{"read the timeline", http.MethodGet, "/v1/runs/run-A/timeline", nil},
		{"read capture metadata", http.MethodGet, "/v1/runs/run-A/requests", nil},
		{"submit a run", http.MethodPost, "/v1/jobs/job-A/runs", []byte(`{}`)},
		{"cancel a run", http.MethodPost, "/v1/runs/run-A/cancel", nil},
		{"pause a job", http.MethodPost, "/v1/jobs/job-A/pause", nil},
		{"resume a job", http.MethodPost, "/v1/jobs/job-A/resume", nil},
		{"set a schedule", http.MethodPut, "/v1/jobs/job-A/schedule", []byte(`{"cron":"*/5 * * * *"}`)},
		{"clear a schedule", http.MethodDelete, "/v1/jobs/job-A/schedule", nil},
	}

	for _, tc := range gateway {
		t.Run("gateway/"+tc.name, func(t *testing.T) {
			res := do(t, tc.method, srv.URL+tc.path, tc.body, auth)
			if res.status == http.StatusForbidden || res.status == http.StatusUnauthorized {
				t.Fatalf("%s %s: control token refused with %d; it is on the gateway surface\n%s",
					tc.method, tc.path, res.status, res.body)
			}
		})
	}

	// Everything a gateway has no business doing. Each must be 403, not 404:
	// the route exists and the caller is authenticated, but not authorized.
	forbidden := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"register a job", http.MethodPost, "/v1/jobs", []byte(`{"path":"/jobs/new"}`)},
		{"resolve a job reference", http.MethodGet, "/v1/jobs/resolve?ref=job-A", nil},
		{"reset a job", http.MethodPost, "/v1/jobs/job-A/reset", nil},
		{"move a job", http.MethodPost, "/v1/jobs/job-A/move", []byte(`{"destination":"/jobs/moved"}`)},
		{"delete a job", http.MethodDelete, "/v1/jobs/job-A", nil},
		{"reload the daemon", http.MethodPost, "/v1/reload", nil},
		{"read job state", http.MethodGet, "/v1/jobs/job-A/state", nil},
		{"write job state", http.MethodPut, "/v1/jobs/job-A/state/k", []byte(`"v"`)},
		{"delete job state", http.MethodDelete, "/v1/jobs/job-A/state/k", nil},
		{"read a capture payload", http.MethodGet, "/v1/runs/run-A/requests/req-1", nil},
		{"read a capture payload by id", http.MethodGet, "/v1/requests/req-1", nil},
		{"list tokens", http.MethodGet, "/v1/tokens", nil},
		{"mint a token", http.MethodPost, "/v1/tokens", []byte(`{"name":"x","scope":"control"}`)},
		{"revoke a token", http.MethodDelete, "/v1/tokens/tok-1", nil},
	}

	for _, tc := range forbidden {
		t.Run("forbidden/"+tc.name, func(t *testing.T) {
			res := do(t, tc.method, srv.URL+tc.path, tc.body, auth)
			if res.status != http.StatusForbidden {
				t.Fatalf("%s %s: control token got %d, want %d\n%s",
					tc.method, tc.path, res.status, http.StatusForbidden, res.body)
			}
		})
	}
}

// TestReadTokenIsReadOnly proves the narrower scope commands nothing.
func TestReadTokenIsReadOnly(t *testing.T) {
	srv, _, readToken := scopedFixture(t)
	auth := bearer(readToken)

	reads := []string{"/v1/jobs", "/v1/jobs/job-A", "/v1/runs", "/v1/runs/run-A", "/v1/runs/run-A/logs"}
	for _, path := range reads {
		t.Run("read "+path, func(t *testing.T) {
			res := do(t, http.MethodGet, srv.URL+path, nil, auth)
			if res.status == http.StatusForbidden || res.status == http.StatusUnauthorized {
				t.Fatalf("GET %s: read token refused with %d\n%s", path, res.status, res.body)
			}
		})
	}

	commands := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodPost, "/v1/jobs/job-A/runs", []byte(`{}`)},
		{http.MethodPost, "/v1/runs/run-A/cancel", nil},
		{http.MethodPost, "/v1/jobs/job-A/pause", nil},
		{http.MethodPost, "/v1/jobs/job-A/resume", nil},
		{http.MethodPut, "/v1/jobs/job-A/schedule", []byte(`{"cron":"*/5 * * * *"}`)},
		{http.MethodDelete, "/v1/jobs/job-A/schedule", nil},
	}
	for _, tc := range commands {
		t.Run("command "+tc.method+" "+tc.path, func(t *testing.T) {
			res := do(t, tc.method, srv.URL+tc.path, tc.body, auth)
			if res.status != http.StatusForbidden {
				t.Fatalf("%s %s: read token got %d, want %d\n%s",
					tc.method, tc.path, res.status, http.StatusForbidden, res.body)
			}
		})
	}
}

// TestRunTokenCannotReachOperatorLists guards the boundary between the two read
// gates. These endpoints read across jobs and runs, so a per-run token must not
// reach them even though it may read its own run.
func TestRunTokenCannotReachOperatorLists(t *testing.T) {
	b := newFakeBackend()
	b.addJob("job-A", true, "")
	b.runTokens["run-token"] = RunToken{RunID: "run-A", JobID: "job-A"}
	now := time.Now().UTC()
	b.addRun(&runs.Run{ID: "run-A", JobID: "job-A", Status: runs.StatusSucceeded, Attempt: 1, CreatedAt: now})

	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	defer srv.Close()

	auth := bearer("run-token")
	for _, path := range []string{"/v1/jobs", "/v1/runs", "/v1/runs/run-A/timeline", "/v1/runs/run-A/requests"} {
		t.Run(path, func(t *testing.T) {
			res := do(t, http.MethodGet, srv.URL+path, nil, auth)
			if res.status != http.StatusForbidden {
				t.Fatalf("GET %s: run token got %d, want %d; this endpoint reads across runs\n%s",
					path, res.status, http.StatusForbidden, res.body)
			}
		})
	}

	// The narrowing reads it may keep.
	for _, path := range []string{"/v1/jobs/job-A", "/v1/runs/run-A", "/v1/runs/run-A/logs"} {
		t.Run("allowed "+path, func(t *testing.T) {
			res := do(t, http.MethodGet, srv.URL+path, nil, auth)
			if res.status == http.StatusForbidden || res.status == http.StatusUnauthorized {
				t.Fatalf("GET %s: run token refused with %d\n%s", path, res.status, res.body)
			}
		})
	}
}

// TestScopedTokenCannotSeeJobWebhookToken covers the leak that would matter
// most: the single-job view carries a credential that triggers runs.
func TestScopedTokenCannotSeeJobWebhookToken(t *testing.T) {
	srv, controlToken, readToken := scopedFixture(t)

	for name, token := range map[string]string{"control": controlToken, "read": readToken} {
		t.Run(name, func(t *testing.T) {
			res := do(t, http.MethodGet, srv.URL+"/v1/jobs/job-A", nil, bearer(token))
			var view JobView
			if err := json.Unmarshal(res.body, &view); err != nil {
				t.Fatalf("decode job view: %v\n%s", err, res.body)
			}
			if view.Triggers.WebhookToken != "" {
				t.Fatalf("%s token received the job's webhook token", name)
			}
		})
	}

	// The admin token still gets it, or the credential becomes unusable.
	res := do(t, http.MethodGet, srv.URL+"/v1/jobs/job-A", nil, bearer(adminToken))
	var view JobView
	if err := json.Unmarshal(res.body, &view); err != nil {
		t.Fatalf("decode admin job view: %v\n%s", err, res.body)
	}
	if view.Triggers.WebhookToken != "webhook-secret-for-job-A" {
		t.Fatalf("admin webhook token = %q, want it preserved", view.Triggers.WebhookToken)
	}
}

// TestScopedTokenCannotRequestHTTPCaptureOnTimeline covers the second leak: the
// merged timeline can carry captured exchanges, which are business data.
func TestScopedTokenCannotRequestHTTPCaptureOnTimeline(t *testing.T) {
	srv, controlToken, _ := scopedFixture(t)

	res := do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/timeline?include_http=true", nil, bearer(controlToken))
	if res.status != http.StatusForbidden {
		t.Fatalf("control token asking for include_http got %d, want %d\n%s",
			res.status, http.StatusForbidden, res.body)
	}

	// Without the parameter the scoped caller still gets the timeline.
	res = do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/timeline", nil, bearer(controlToken))
	if res.status == http.StatusForbidden || res.status == http.StatusUnauthorized {
		t.Fatalf("control token reading the timeline was refused with %d\n%s", res.status, res.body)
	}
}

// TestAPITokenRevocationIsImmediate is the "independently revocable" half of
// CL-21: no restart, no cache, the very next request is refused.
func TestAPITokenRevocationIsImmediate(t *testing.T) {
	srv, controlToken, _ := scopedFixture(t)
	auth := bearer(controlToken)

	if res := do(t, http.MethodGet, srv.URL+"/v1/jobs", nil, auth); res.status != http.StatusOK {
		t.Fatalf("token does not work before revocation: %d\n%s", res.status, res.body)
	}

	list := do(t, http.MethodGet, srv.URL+"/v1/tokens", nil, bearer(adminToken))
	var tokens APITokenListResponse
	if err := json.Unmarshal(list.body, &tokens); err != nil {
		t.Fatalf("decode token list: %v\n%s", err, list.body)
	}
	var id string
	for _, tok := range tokens.Tokens {
		if tok.Scope == ScopeControl {
			id = tok.ID
		}
		if tok.Name == "cloud-gateway" && tok.Scope != ScopeControl {
			t.Fatalf("token scope = %q, want %q", tok.Scope, ScopeControl)
		}
	}
	if id == "" {
		t.Fatal("the control token is not in the list")
	}

	revoke := do(t, http.MethodDelete, srv.URL+"/v1/tokens/"+id, nil, bearer(adminToken))
	if revoke.status != http.StatusOK {
		t.Fatalf("revoke: %d\n%s", revoke.status, revoke.body)
	}

	res := do(t, http.MethodGet, srv.URL+"/v1/jobs", nil, auth)
	if res.status != http.StatusUnauthorized {
		t.Fatalf("revoked token got %d, want %d; revocation is not immediate\n%s",
			res.status, http.StatusUnauthorized, res.body)
	}

	// Revoking again is a success, not a 404: the operator asked for it to be
	// unusable and it is.
	again := do(t, http.MethodDelete, srv.URL+"/v1/tokens/"+id, nil, bearer(adminToken))
	if again.status != http.StatusOK {
		t.Fatalf("re-revoke: %d, want %d\n%s", again.status, http.StatusOK, again.body)
	}

	unknown := do(t, http.MethodDelete, srv.URL+"/v1/tokens/nope", nil, bearer(adminToken))
	if unknown.status != http.StatusNotFound {
		t.Fatalf("revoking an unknown id: %d, want %d", unknown.status, http.StatusNotFound)
	}
}

// TestAPITokenIssuanceValidation covers the input boundary.
func TestAPITokenIssuanceValidation(t *testing.T) {
	b := newFakeBackend()
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	defer srv.Close()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing name", `{"scope":"control"}`, http.StatusBadRequest},
		{"blank name", `{"name":"   ","scope":"control"}`, http.StatusBadRequest},
		{"unknown scope", `{"name":"x","scope":"admin"}`, http.StatusBadRequest},
		{"empty scope", `{"name":"x"}`, http.StatusBadRequest},
		{"not json", `nonsense`, http.StatusBadRequest},
		{"read is valid", `{"name":"x","scope":"read"}`, http.StatusCreated},
		{"control is valid", `{"name":"y","scope":"control"}`, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, http.MethodPost, srv.URL+"/v1/tokens", []byte(tc.body), bearer(adminToken))
			wantStatus(t, res, tc.want)
		})
	}
}

// TestScopedTokensAreNotAcceptedAfterRestartlessMint is a negative for the
// token prefix: an unrelated bearer string must not be treated as a scoped
// token, and must not fall through to admin.
func TestUnrelatedBearerIsRejected(t *testing.T) {
	srv, _, _ := scopedFixture(t)
	for _, token := range []string{"", "otter_ctl_", "otter_ro_deadbeef", "nonsense"} {
		t.Run(token, func(t *testing.T) {
			res := do(t, http.MethodGet, srv.URL+"/v1/jobs", nil, bearer(token))
			if res.status != http.StatusUnauthorized {
				t.Fatalf("bearer %q got %d, want %d\n%s", token, res.status, http.StatusUnauthorized, res.body)
			}
		})
	}
}
