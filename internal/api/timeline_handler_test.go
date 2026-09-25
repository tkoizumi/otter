package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tkoizumi/otter/internal/timeline"
)

// newTestServer is the helper name other API tests use for the two-argument
// case; the timeline tests need the fake back end's seams, so they build both
// explicitly and reach the server through this.
func newTimelineTestServer(t *testing.T) (*fakeBackend, string, func()) {
	t.Helper()
	b := newFakeBackend()
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	return b, srv.URL, srv.Close
}

// timelineRequests returns the requests the backend observed.
func (b *fakeBackend) timelineRequestsSeen() []timeline.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]timeline.Request, len(b.timelineRequests))
	copy(out, b.timelineRequests)
	return out
}

// TestTimelineRouteContract covers the parts of the timeline the API layer owns:
// the query contract, the authorization, and the mapping from a reader error to
// a status code. The merge itself is tested in internal/timeline.
func TestTimelineRouteContract(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		b, base, closeSrv := newTimelineTestServer(t)
		defer closeSrv()

		r := do(t, http.MethodGet, base+"/v1/runs/run-1/timeline", nil, adminHeaders())
		wantStatus(t, r, http.StatusOK)

		requests := b.timelineRequestsSeen()
		if len(requests) != 1 {
			t.Fatalf("backend saw %d timeline requests, want 1", len(requests))
		}
		got := requests[0]
		if got.RunID != "run-1" {
			t.Errorf("run id = %q, want run-1", got.RunID)
		}
		if got.Limit != timeline.DefaultLimit {
			t.Errorf("limit = %d, want %d", got.Limit, timeline.DefaultLimit)
		}
		if !got.IncludeHTTP {
			t.Errorf("HTTP events must be included by default")
		}
		if got.After != "" {
			t.Errorf("first page must not carry a cursor, got %q", got.After)
		}
	})

	t.Run("explicit query", func(t *testing.T) {
		b, base, closeSrv := newTimelineTestServer(t)
		defer closeSrv()

		r := do(t, http.MethodGet,
			base+"/v1/runs/run-1/timeline?limit=5&include_http=false&after=abc",
			nil, adminHeaders())
		wantStatus(t, r, http.StatusOK)

		got := b.timelineRequestsSeen()[0]
		if got.Limit != 5 || got.IncludeHTTP || got.After != "abc" {
			t.Errorf("request = %+v, want limit 5, http excluded, cursor abc", got)
		}
	})

	t.Run("rejects a bad limit", func(t *testing.T) {
		for _, raw := range []string{"0", "-1", "abc", "1001"} {
			b, base, closeSrv := newTimelineTestServer(t)
			r := do(t, http.MethodGet, base+"/v1/runs/run-1/timeline?limit="+raw, nil, adminHeaders())
			wantStatus(t, r, http.StatusBadRequest)
			if len(b.timelineRequestsSeen()) != 0 {
				t.Errorf("limit %q reached the backend", raw)
			}
			closeSrv()
		}
	})

	t.Run("rejects a bad include_http", func(t *testing.T) {
		b, base, closeSrv := newTimelineTestServer(t)
		defer closeSrv()

		r := do(t, http.MethodGet, base+"/v1/runs/run-1/timeline?include_http=maybe", nil, adminHeaders())
		wantStatus(t, r, http.StatusBadRequest)
		if len(b.timelineRequestsSeen()) != 0 {
			t.Errorf("invalid include_http reached the backend")
		}
	})

	t.Run("operator only", func(t *testing.T) {
		// A run token can read its own run's logs, but the merged timeline also
		// exposes captured traffic, so it follows the request-list reads and
		// stays operator-only.
		b, base, closeSrv := newTimelineTestServer(t)
		defer closeSrv()

		b.mu.Lock()
		b.runTokens["token-A"] = RunToken{RunID: "run-1", IntegrationID: "int-1"}
		b.mu.Unlock()

		r := do(t, http.MethodGet, base+"/v1/runs/run-1/timeline", nil, runTokenHeaders("token-A"))
		wantStatus(t, r, http.StatusForbidden)
		if len(b.timelineRequestsSeen()) != 0 {
			t.Errorf("a run token reached the timeline backend")
		}
	})

	t.Run("error mapping", func(t *testing.T) {
		cases := []struct {
			err    error
			status int
			code   string
		}{
			{timeline.ErrRunNotFound, http.StatusNotFound, CodeNotFound},
			{timeline.ErrRunNotTerminal, http.StatusConflict, CodeConflict},
			{timeline.ErrEvidenceChanged, http.StatusConflict, CodeConflict},
			{timeline.ErrCursorInvalid, http.StatusBadRequest, CodeInvalid},
			{timeline.ErrReadDeadline, http.StatusServiceUnavailable, CodeUnavailable},
		}
		for _, tc := range cases {
			b, base, closeSrv := newTimelineTestServer(t)
			b.mu.Lock()
			b.timelineErr = tc.err
			b.mu.Unlock()

			r := do(t, http.MethodGet, base+"/v1/runs/run-1/timeline", nil, adminHeaders())
			wantStatus(t, r, tc.status)

			var body ErrorResponse
			if err := json.Unmarshal(r.body, &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Code != tc.code {
				t.Errorf("%v mapped to code %q, want %q", tc.err, body.Error.Code, tc.code)
			}
			closeSrv()
		}
	})

	t.Run("the page is returned as stored", func(t *testing.T) {
		b, base, closeSrv := newTimelineTestServer(t)
		defer closeSrv()

		b.mu.Lock()
		b.timelinePage = &timeline.Page{
			Context: timeline.Context{
				SchemaVersion: timeline.SchemaVersion,
				RunID:         "run-1",
				Status:        "failed",
				IncludeHTTP:   true,
			},
			Events: []timeline.Event{{
				Kind:   timeline.KindHTTP,
				Source: timeline.SourceHTTPExchanges,
				ID:     7,
				RunID:  "run-1",
				HTTP:   &timeline.HTTPEvent{RequestID: "req-1", Method: "POST"},
			}},
			HasMore:    true,
			NextCursor: "next-token",
		}
		b.mu.Unlock()

		r := do(t, http.MethodGet, base+"/v1/runs/run-1/timeline", nil, adminHeaders())
		wantStatus(t, r, http.StatusOK)

		var got timeline.Page
		if err := json.Unmarshal(r.body, &got); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		if got.Context.Status != "failed" || len(got.Events) != 1 {
			t.Fatalf("page = %+v, want the stored page back", got)
		}
		if got.Events[0].HTTP == nil || got.Events[0].HTTP.RequestID != "req-1" {
			t.Errorf("event payload was not round-tripped: %+v", got.Events[0])
		}
		if !got.HasMore || got.NextCursor != "next-token" {
			t.Errorf("continuation framing was lost: has_more=%v cursor=%q", got.HasMore, got.NextCursor)
		}
	})
}
