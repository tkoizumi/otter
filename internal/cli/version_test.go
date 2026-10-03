package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
)

// `otter version` is the local build, with no daemon and no network.
func TestVersionCommandPrintsTheLocalBuild(t *testing.T) {
	var out bytes.Buffer
	app := New("9.9.9", &out, &out)
	if code := app.cmdVersion(context.Background(), globals{}, nil); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if strings.TrimSpace(out.String()) != "9.9.9" {
		t.Errorf("output = %q, want the build version", out.String())
	}
}

// `otter --json version` reads the daemon's contract document, which is the
// machine-readable half of the interface freeze.
func TestVersionCommandJSONReadsTheDaemonDocument(t *testing.T) {
	doc := api.VersionDocumentFor("9.9.9")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" {
			t.Errorf("request path = %q, want /v1/version", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	var out bytes.Buffer
	app := New("9.9.9", &out, &out)
	if code := app.cmdVersion(context.Background(), globals{api: srv.URL, jsonOut: true}, nil); code != 0 {
		t.Fatalf("exit code = %d: %s", code, out.String())
	}
	var got api.VersionDocument
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode output %q: %v", out.String(), err)
	}
	if got.SchemaVersion != api.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, api.SchemaVersion)
	}
	if got.ProductVersion != "9.9.9" {
		t.Errorf("product_version = %q, want 9.9.9", got.ProductVersion)
	}
	if len(got.SupportedPlatforms) == 0 {
		t.Error("the document carries no supported platforms")
	}
}
