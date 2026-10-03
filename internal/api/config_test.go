package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The configuration surface: an admin writes a version, any scoped reader may
// read it (it is job metadata, not a secret), and a control token may not write
// it -- the control scope's published surface does not include configuration.
func TestJobConfigEndpoints(t *testing.T) {
	b := newFakeBackend()
	b.addJob("job-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	// Unconfigured: an empty object and no version.
	got := do(t, http.MethodGet, srv.URL+"/v1/jobs/job-A/config", nil, adminHeaders())
	wantStatus(t, got, http.StatusOK)
	var view JobConfigView
	got.decode(t, &view)
	if view.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", view.SchemaVersion, SchemaVersion)
	}
	if view.ConfigVersion != "" || string(view.Values) != "{}" {
		t.Fatalf("unconfigured view = %+v, want an empty object", view)
	}

	anonymous := do(t, http.MethodPut, srv.URL+"/v1/jobs/job-A/config",
		[]byte(`{"values":{"dataset":1}}`), nil)
	wantStatus(t, anonymous, http.StatusUnauthorized)

	control := mintToken(t, srv, "gateway", ScopeControl)
	forbidden := do(t, http.MethodPut, srv.URL+"/v1/jobs/job-A/config",
		[]byte(`{"values":{"dataset":1}}`), bearer(control.Token))
	wantStatus(t, forbidden, http.StatusForbidden)

	put := do(t, http.MethodPut, srv.URL+"/v1/jobs/job-A/config",
		[]byte(`{"values":{"dataset":1}}`), adminHeaders())
	wantStatus(t, put, http.StatusOK)
	var written JobConfigView
	put.decode(t, &written)
	if written.ConfigVersion == "" || !written.Changed {
		t.Fatalf("written view = %+v, want a new version", written)
	}
	if string(written.Values) != `{"dataset":1}` {
		t.Errorf("written values = %s", written.Values)
	}
	if written.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", written.SchemaVersion, SchemaVersion)
	}

	// A scoped reader may read it back.
	read := mintToken(t, srv, "castor-backend", ScopeRead)
	readBack := do(t, http.MethodGet, srv.URL+"/v1/jobs/job-A/config", nil, bearer(read.Token))
	wantStatus(t, readBack, http.StatusOK)
	var readView JobConfigView
	readBack.decode(t, &readView)
	if readView.ConfigVersion != written.ConfigVersion {
		t.Errorf("read-back version = %q, want %q", readView.ConfigVersion, written.ConfigVersion)
	}

	// An absent values key clears the configuration rather than failing.
	cleared := do(t, http.MethodPut, srv.URL+"/v1/jobs/job-A/config",
		[]byte(`{}`), adminHeaders())
	wantStatus(t, cleared, http.StatusOK)
	var clearedView JobConfigView
	cleared.decode(t, &clearedView)
	if string(clearedView.Values) != "{}" {
		t.Errorf("cleared values = %s, want an empty object", clearedView.Values)
	}

	// A malformed body is a bad request and never reaches the backend.
	bad := do(t, http.MethodPut, srv.URL+"/v1/jobs/job-A/config", []byte(`{`), adminHeaders())
	wantStatus(t, bad, http.StatusBadRequest)

	missing := do(t, http.MethodGet, srv.URL+"/v1/jobs/ghost/config", nil, adminHeaders())
	wantStatus(t, missing, http.StatusNotFound)
}

// A list response is an object carrying schema_version, never a bare array, and
// each endpoint uses its own key.
func TestListEnvelopesCarrySchemaVersion(t *testing.T) {
	b := newFakeBackend()
	b.addJob("job-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	cases := []struct {
		name string
		path string
		key  string
	}{
		{"jobs", "/v1/jobs", "jobs"},
		{"runs", "/v1/runs", "runs"},
		{"logs", "/v1/runs/run-A/logs", "logs"},
		{"tokens", "/v1/tokens", "tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, http.MethodGet, srv.URL+tc.path, nil, adminHeaders())
			wantStatus(t, res, http.StatusOK)

			var raw map[string]json.RawMessage
			res.decode(t, &raw)
			if _, ok := raw["schema_version"]; !ok {
				t.Fatalf("%s is not a versioned envelope: %s", tc.path, res.body)
			}
			if _, ok := raw[tc.key]; !ok {
				t.Fatalf("%s envelope has no %q key: %s", tc.path, tc.key, res.body)
			}
			var version int
			if err := json.Unmarshal(raw["schema_version"], &version); err != nil || version != SchemaVersion {
				t.Fatalf("%s schema_version = %s, want %d", tc.path, raw["schema_version"], SchemaVersion)
			}
		})
	}
}
