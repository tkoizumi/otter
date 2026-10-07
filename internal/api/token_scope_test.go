package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
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

// scopedFixtureBuild holds one server plus a token of each scope, so a test can
// probe the same routes from every credential class.
type scopedFixtureBuild struct {
	srv     *httptest.Server
	control string
	read    string
	capture string
}

// scopedFixture builds a server with one job, one run and one token of each
// scope, plus the raw tokens.
func scopedFixture(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	built := scopedFixtureAll(t)
	return built.srv, built.control, built.read
}

// scopedFixtureAll is scopedFixture plus the capture token, for the tests that
// are about the capture boundary.
func scopedFixtureAll(t *testing.T) scopedFixtureBuild {
	t.Helper()
	b := newFakeBackend()
	b.addJob("job-A", true, "webhook-secret-for-job-A")
	now := time.Now().UTC()
	b.timelinePage = &timeline.Page{Context: timeline.Context{RunID: "run-A", JobID: "job-A"}}
	// A run with one captured exchange, so the payload routes have something to
	// return to a capture credential and something to refuse to the others.
	seedCapturedRun(t, b, "run-A", "job-A", inspection.PolicyFull)
	b.runs["run-A"].Status = runs.StatusSucceeded
	b.runs["run-A"].CreatedAt = now

	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	t.Cleanup(srv.Close)

	// Ingest one exchange through the real route, as an SDK would, so the
	// payload test reads a stored body rather than one assembled by the test.
	body := captureBatch(inspection.PolicyFull, inspection.RequestEvent{
		Kind:        inspection.EventCompleted,
		RequestID:   "req-captured",
		ProducerSeq: 1,
		OccurredAt:  now,
		Method:      "POST",
		URL:         "https://api.example.com/v1/items",
		CallSite:    "source.py:42 in push",
		RequestBody: &inspection.BodyDescriptor{
			State:         inspection.BodyCaptured,
			ContentType:   "application/json",
			BytesObserved: 24,
			JSON:          []byte(`{"cursor":"cur-42"}`),
			Redacted:      true,
			RedactedCount: 1,
		},
	})
	if res := do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", body, bearer(adminToken)); res.status != http.StatusAccepted {
		t.Fatalf("seed captured exchange: status %d, body %s", res.status, res.body)
	}

	control := mintToken(t, srv, "cloud-gateway", ScopeControl)
	read := mintToken(t, srv, "castor-backend", ScopeRead)
	capture := mintToken(t, srv, "cloud-capture", ScopeCapture)
	return scopedFixtureBuild{srv: srv, control: control.Token, read: read.Token, capture: capture.Token}
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

// TestCaptureTokenReachesPayloadsAndTheGateway is the other half of the
// boundary: a capture credential is a superset of control, it reads the stored
// bodies, and the widening reaches nothing else.
func TestCaptureTokenReachesPayloadsAndTheGateway(t *testing.T) {
	built := scopedFixtureAll(t)
	auth := bearer(built.capture)

	// The gateway and read surfaces a control token has, it has too.
	allowed := []string{
		"/v1/jobs",
		"/v1/jobs/job-A",
		"/v1/runs",
		"/v1/runs/run-A",
		"/v1/runs/run-A/logs",
		"/v1/runs/run-A/timeline",
		"/v1/runs/run-A/requests",
	}
	for _, path := range allowed {
		t.Run("allowed "+path, func(t *testing.T) {
			res := do(t, http.MethodGet, built.srv.URL+path, nil, auth)
			if res.status == http.StatusForbidden || res.status == http.StatusUnauthorized {
				t.Fatalf("GET %s: capture token refused with %d\n%s", path, res.status, res.body)
			}
		})
	}

	// Both payload routes return the sanitized body, redaction count included.
	for _, path := range []string{
		"/v1/runs/run-A/requests/req-captured",
		"/v1/requests/req-captured",
	} {
		t.Run("payload "+path, func(t *testing.T) {
			res := do(t, http.MethodGet, built.srv.URL+path, nil, auth)
			if res.status != http.StatusOK {
				t.Fatalf("GET %s: capture token got %d, want %d\n%s",
					path, res.status, http.StatusOK, res.body)
			}
			var detail CaptureRequestResponse
			if err := json.Unmarshal(res.body, &detail); err != nil {
				t.Fatalf("decode payload: %v\n%s", err, res.body)
			}
			if detail.Request == nil || detail.Request.RequestBody == nil {
				t.Fatalf("payload carries no request body: %+v", detail.Request)
			}
			if got := string(detail.Request.RequestBody.JSON); got != `{"cursor":"cur-42"}` {
				t.Errorf("request body = %s, want the sanitized JSON", got)
			}
			if !detail.Request.RequestBody.Redacted || detail.Request.RequestBody.RedactedCount != 1 {
				t.Errorf("redaction = %v/%d, want true/1",
					detail.Request.RequestBody.Redacted, detail.Request.RequestBody.RedactedCount)
			}
		})
	}

	// The widening stops where the plan says it stops: identity, state and the
	// tokens themselves are still admin-only.
	forbidden := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"register a job", http.MethodPost, "/v1/jobs", []byte(`{"path":"/jobs/new"}`)},
		{"delete a job", http.MethodDelete, "/v1/jobs/job-A", nil},
		{"reload the daemon", http.MethodPost, "/v1/reload", nil},
		{"read job state", http.MethodGet, "/v1/jobs/job-A/state", nil},
		{"list tokens", http.MethodGet, "/v1/tokens", nil},
		{"mint a token", http.MethodPost, "/v1/tokens", []byte(`{"name":"x","scope":"capture"}`)},
	}
	for _, tc := range forbidden {
		t.Run("forbidden/"+tc.name, func(t *testing.T) {
			res := do(t, tc.method, built.srv.URL+tc.path, tc.body, auth)
			if res.status != http.StatusForbidden {
				t.Fatalf("%s %s: capture token got %d, want %d\n%s",
					tc.method, tc.path, res.status, http.StatusForbidden, res.body)
			}
		})
	}
}

// TestPayloadRoutesRefuseControlAndRead is the "independently named authority"
// half: a credential that commands the runtime, or merely observes it, is still
// refused the client's bodies.
func TestPayloadRoutesRefuseControlAndRead(t *testing.T) {
	built := scopedFixtureAll(t)

	payloadRoutes := []string{
		"/v1/runs/run-A/requests/req-captured",
		"/v1/requests/req-captured",
	}
	for name, token := range map[string]string{"control": built.control, "read": built.read} {
		for _, path := range payloadRoutes {
			t.Run(name+" "+path, func(t *testing.T) {
				res := do(t, http.MethodGet, built.srv.URL+path, nil, bearer(token))
				if res.status != http.StatusForbidden {
					t.Fatalf("GET %s: %s token got %d, want %d\n%s",
						path, name, res.status, http.StatusForbidden, res.body)
				}
			})
		}
	}

	// The admin token still reaches them, or the routes would be unusable.
	res := do(t, http.MethodGet, built.srv.URL+"/v1/requests/req-captured", nil, bearer(adminToken))
	if res.status != http.StatusOK {
		t.Fatalf("admin token got %d on the payload route, want %d\n%s", res.status, http.StatusOK, res.body)
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
		{"capture is valid", `{"name":"z","scope":"capture"}`, http.StatusCreated},
		{"agent is valid", `{"name":"a","scope":"agent"}`, http.StatusCreated},
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
	for _, token := range []string{"", "otter_ctl_", "otter_ro_deadbeef", "otter_cap_", "otter_agt_", "nonsense"} {
		t.Run(token, func(t *testing.T) {
			res := do(t, http.MethodGet, srv.URL+"/v1/jobs", nil, bearer(token))
			if res.status != http.StatusUnauthorized {
				t.Fatalf("bearer %q got %d, want %d\n%s", token, res.status, http.StatusUnauthorized, res.body)
			}
		})
	}
}

// TestAgentTokenReachesTheAgentSurfaceAndNothingElse is the evidence for the
// runtime's own credential. The agent is the tenant's only control channel, so
// it reaches the deploy surface AND the read/control surface: gate, releases,
// jobs, runs, output, timeline, capture metadata, run/cancel/pause/schedule.
// It must not reach capture payloads, job state, job registration, configuration
// writes, reload or token management -- those stay admin, and the tenant's own
// Python cannot read the credential because the agent is a separate PID
// namespace.
func TestAgentTokenReachesTheAgentSurfaceAndNothingElse(t *testing.T) {
	built := scopedFixtureAll(t)
	agent := mintToken(t, built.srv, "runtime-agent", ScopeAgent)
	auth := bearer(agent.Token)

	// Everything the agent's allowlist covers. A 400 from a handler is fine: the
	// point is that the GATE admitted the credential (not 401/403).
	allowed := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		// Deploy.
		{"read maintenance", http.MethodGet, "/v1/runtime/maintenance", nil},
		{"enter maintenance", http.MethodPost, "/v1/runtime/maintenance", []byte(`{}`)},
		{"exit maintenance", http.MethodDelete, "/v1/runtime/maintenance", nil},
		{"read active releases", http.MethodGet, "/v1/runtime/releases/active", nil},
		{"install a package", http.MethodPost, "/v1/runtime/releases/install", nil},
		{"activate a release", http.MethodPost, "/v1/runtime/releases/activate",
			[]byte(`{"digest":"sha256:` + strings.Repeat("a", 64) + `"}`)},
		// Read.
		{"list jobs", http.MethodGet, "/v1/jobs", nil},
		{"read a job", http.MethodGet, "/v1/jobs/job-A", nil},
		{"list runs", http.MethodGet, "/v1/runs", nil},
		{"read a run", http.MethodGet, "/v1/runs/run-A", nil},
		{"read run output", http.MethodGet, "/v1/runs/run-A/logs", nil},
		{"read the timeline", http.MethodGet, "/v1/runs/run-A/timeline", nil},
		{"read capture metadata", http.MethodGet, "/v1/runs/run-A/requests", nil},
		{"read job config", http.MethodGet, "/v1/jobs/job-A/config", nil},
		// Control.
		{"submit a run", http.MethodPost, "/v1/jobs/job-A/runs", []byte(`{}`)},
		{"cancel a run", http.MethodPost, "/v1/runs/run-A/cancel", nil},
		{"pause a job", http.MethodPost, "/v1/jobs/job-A/pause", nil},
		{"resume a job", http.MethodPost, "/v1/jobs/job-A/resume", nil},
		{"set a schedule", http.MethodPut, "/v1/jobs/job-A/schedule", []byte(`{"cron":"*/5 * * * *"}`)},
		{"clear a schedule", http.MethodDelete, "/v1/jobs/job-A/schedule", nil},
	}
	for _, tc := range allowed {
		t.Run("allowed/"+tc.name, func(t *testing.T) {
			res := do(t, tc.method, built.srv.URL+tc.path, tc.body, auth)
			if res.status == http.StatusForbidden || res.status == http.StatusUnauthorized {
				t.Fatalf("%s %s: agent token refused with %d; it is on the agent's surface\n%s",
					tc.method, tc.path, res.status, res.body)
			}
		})
	}

	// The surface the agent must NOT hold, even though it is the tenant's own
	// credential: capture payloads (PII), job state, registration and identity,
	// configuration writes, reload, and token management.
	forbidden := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"read a capture payload", http.MethodGet, "/v1/runs/run-A/requests/req-1", nil},
		{"read a capture payload by id", http.MethodGet, "/v1/requests/req-1", nil},
		{"read job state", http.MethodGet, "/v1/jobs/job-A/state", nil},
		{"write job state", http.MethodPut, "/v1/jobs/job-A/state/k", []byte(`"v"`)},
		{"delete job state", http.MethodDelete, "/v1/jobs/job-A/state/k", nil},
		{"write job config", http.MethodPut, "/v1/jobs/job-A/config", []byte(`{}`)},
		{"register a job", http.MethodPost, "/v1/jobs", []byte(`{"path":"/jobs/new"}`)},
		{"resolve a job reference", http.MethodGet, "/v1/jobs/resolve?ref=job-A", nil},
		{"reset a job", http.MethodPost, "/v1/jobs/job-A/reset", nil},
		{"move a job", http.MethodPost, "/v1/jobs/job-A/move", []byte(`{"destination":"/jobs/moved"}`)},
		{"delete a job", http.MethodDelete, "/v1/jobs/job-A", nil},
		{"reload the daemon", http.MethodPost, "/v1/reload", nil},
		{"list tokens", http.MethodGet, "/v1/tokens", nil},
		{"mint a token", http.MethodPost, "/v1/tokens", []byte(`{"name":"x","scope":"agent"}`)},
	}
	for _, tc := range forbidden {
		t.Run("forbidden/"+tc.name, func(t *testing.T) {
			res := do(t, tc.method, built.srv.URL+tc.path, tc.body, auth)
			if res.status != http.StatusForbidden {
				t.Fatalf("%s %s: agent token got %d, want %d\n%s",
					tc.method, tc.path, res.status, http.StatusForbidden, res.body)
			}
		})
	}
}
