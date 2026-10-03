package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
)

// listRecorder serves the two list endpoints as the daemon does: an object with
// schema_version and an endpoint-specific key.
type listRecorder struct {
	jobs api.JobList
	runs api.RunList
}

func (l *listRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/jobs":
			_ = json.NewEncoder(w).Encode(l.jobs)
		case "/v1/runs":
			_ = json.NewEncoder(w).Encode(l.runs)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// decodeEnvelope asserts the printed document is an object carrying
// schema_version and the expected key, not a bare array.
func decodeEnvelope(t *testing.T, out string, key string) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, out)
	}
	var version int
	if payload, ok := raw["schema_version"]; !ok {
		t.Fatalf("output has no schema_version: %s", out)
	} else if err := json.Unmarshal(payload, &version); err != nil || version != api.SchemaVersion {
		t.Fatalf("schema_version = %s, want %d", payload, api.SchemaVersion)
	}
	if _, ok := raw[key]; !ok {
		t.Fatalf("output has no %q key: %s", key, out)
	}
	return raw
}

func TestRunsJSONIsAVersionedEnvelope(t *testing.T) {
	rec := &listRecorder{runs: api.RunList{
		SchemaVersion: api.SchemaVersion,
		Runs:          []*runs.Run{{ID: "run-1", JobID: "job-A", Status: runs.StatusSucceeded}},
	}}
	srv := rec.server(t)
	defer srv.Close()

	var out bytes.Buffer
	app := New("test", &out, &out)
	code := app.cmdRuns(context.Background(), globals{api: srv.URL, jsonOut: true}, []string{"job-A"})
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	decodeEnvelope(t, out.String(), "runs")
}

func TestJobsJSONIsAVersionedEnvelope(t *testing.T) {
	rec := &listRecorder{jobs: api.JobList{
		SchemaVersion: api.SchemaVersion,
		Jobs: []api.JobView{{
			ID: "job-A", Name: "job-A", Path: "/jobs/job-A", Valid: true, Status: "active",
		}},
	}}
	srv := rec.server(t)
	defer srv.Close()

	var out bytes.Buffer
	app := New("test", &out, &out)
	// apiExplicit keeps the listing to the named daemon and off this host's
	// registry, which is what a tunnel does.
	code := app.cmdJobs(context.Background(), globals{api: srv.URL, apiExplicit: true, jsonOut: true}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	decodeEnvelope(t, out.String(), "jobs")
}
