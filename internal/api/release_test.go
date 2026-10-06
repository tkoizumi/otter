package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The endpoint exists so a runtime agent promotes through the same surface an
// operator uses. These tests pin the two things that make that safe: it is
// admin-only, and it refuses while the runtime is serving.

// activate issues the request through the real server and reports the status and
// body, so the test exercises routing and auth rather than calling a handler.
func activate(t *testing.T, srv *httptest.Server, token, body string) (int, []byte) {
	t.Helper()
	hdr := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		hdr["Authorization"] = "Bearer " + token
	}
	res := do(t, http.MethodPost, srv.URL+"/v1/runtime/releases/activate", []byte(body), hdr)
	return res.status, res.body
}

func TestActivateReleaseRefusesWhileServing(t *testing.T) {
	fb := newFakeBackend()
	fb.maintenance = MaintenanceView{Mode: "serving", AcceptingWork: true}
	srv := newTestServer(t, ServerConfig{}, fb)

	code, body := activate(t, srv, "", `{"digest":"sha256:abc"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (activation while serving must be refused): %s", code, http.StatusBadRequest, body)
	}
	if len(fb.activated) != 0 {
		t.Errorf("nothing must be activated while serving, got %v", fb.activated)
	}
}

// Admin-only: activating a release decides which code a tenant runs, so a
// read-only credential must not be able to do it.
func TestActivateReleaseRequiresAnAdminCredential(t *testing.T) {
	fb := newFakeBackend()
	fb.maintenance = MaintenanceView{Mode: "maintenance"}
	// The admin token gates control endpoints; a request with no token must not
	// reach the handler when one is configured.
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, fb)

	code, body := activate(t, srv, "", `{"digest":"sha256:abc"}`)
	if code == http.StatusOK {
		t.Fatalf("an unauthenticated request activated a release: %s", body)
	}
	if len(fb.activated) != 0 {
		t.Errorf("nothing must be activated without an admin credential, got %v", fb.activated)
	}
}

func TestActivateReleaseWorksWhileGated(t *testing.T) {
	fb := newFakeBackend()
	fb.maintenance = MaintenanceView{Mode: "maintenance"}
	srv := newTestServer(t, ServerConfig{}, fb)

	code, body := activate(t, srv, "", `{"digest":"sha256:abc"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	var view ReleaseView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Digest != "sha256:abc" {
		t.Errorf("unexpected view %+v", view)
	}
	if len(fb.activated) != 1 || fb.activated[0] != "sha256:abc" {
		t.Errorf("activation not recorded: %v", fb.activated)
	}
}

// An empty digest is a caller bug, and must be a 400 rather than an activation
// of whatever the runtime happens to have.
func TestActivateReleaseRefusesAnEmptyDigest(t *testing.T) {
	fb := newFakeBackend()
	fb.maintenance = MaintenanceView{Mode: "maintenance"}
	srv := newTestServer(t, ServerConfig{}, fb)

	code, _ := activate(t, srv, "", `{"digest":""}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if len(fb.activated) != 0 {
		t.Errorf("an empty digest activated something: %v", fb.activated)
	}
}

// A malformed body must not be read as "no digest" and then proceed.
func TestActivateReleaseRefusesAMalformedBody(t *testing.T) {
	fb := newFakeBackend()
	fb.maintenance = MaintenanceView{Mode: "maintenance"}
	srv := newTestServer(t, ServerConfig{}, fb)

	code, _ := activate(t, srv, "", `not json`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if len(fb.activated) != 0 {
		t.Errorf("a malformed body activated something: %v", fb.activated)
	}
}
