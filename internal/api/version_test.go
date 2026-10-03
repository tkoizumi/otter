package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/sdk"
)

// The version document is the machine-readable half of the interface freeze:
// the one place a script or a control plane learns the schema version, the
// contract version, the manifest schema, the SDK version and the supported
// platforms. It is public, and its shape is itself frozen.
func TestVersionEndpointIsPublicAndPinned(t *testing.T) {
	b := newFakeBackend()
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	res := do(t, http.MethodGet, srv.URL+"/v1/version", nil, nil)
	wantStatus(t, res, http.StatusOK)

	var doc VersionDocument
	res.decode(t, &doc)
	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if doc.ContractVersion != ContractVersion {
		t.Errorf("contract_version = %d, want %d", doc.ContractVersion, ContractVersion)
	}
	if doc.ProductVersion != b.version {
		t.Errorf("product_version = %q, want %q", doc.ProductVersion, b.version)
	}
	if doc.ManifestSchema != config.SupportedVersion {
		t.Errorf("manifest_schema = %d, but the parser accepts %d", doc.ManifestSchema, config.SupportedVersion)
	}
	if doc.SDKVersion != sdk.Version {
		t.Errorf("sdk_version = %q, want the embedded %q", doc.SDKVersion, sdk.Version)
	}
	if strings.Join(doc.SupportedPlatforms, ",") != strings.Join(SupportedPlatforms, ",") {
		t.Errorf("supported_platforms = %v, want %v", doc.SupportedPlatforms, SupportedPlatforms)
	}
}

// The object-shaped machine documents carry the schema version in the body, so
// a consumer that reads one of them does not need a second request.
func TestMachineDocumentsCarryTheSchemaVersion(t *testing.T) {
	b := newFakeBackend()
	b.addJob("job-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	health := do(t, http.MethodGet, srv.URL+"/health", nil, nil)
	wantStatus(t, health, http.StatusOK)
	var h HealthResponse
	health.decode(t, &h)
	if h.SchemaVersion != SchemaVersion {
		t.Errorf("health schema_version = %d, want %d", h.SchemaVersion, SchemaVersion)
	}

	missing := do(t, http.MethodGet, srv.URL+"/v1/jobs/ghost", nil, adminHeaders())
	wantStatus(t, missing, http.StatusNotFound)
	var e ErrorResponse
	missing.decode(t, &e)
	if e.SchemaVersion != SchemaVersion {
		t.Errorf("error schema_version = %d, want %d", e.SchemaVersion, SchemaVersion)
	}
	if e.Error.Code != CodeNotFound {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeNotFound)
	}

	list := do(t, http.MethodGet, srv.URL+"/v1/jobs/job-A/schedules", nil, adminHeaders())
	wantStatus(t, list, http.StatusOK)
	var sl ScheduleList
	list.decode(t, &sl)
	if sl.SchemaVersion != SchemaVersion {
		t.Errorf("schedule list schema_version = %d, want %d", sl.SchemaVersion, SchemaVersion)
	}
}

// The supported-platform contract is published in two places -- this package
// and runtime-contract.md §6 -- so the test reads the document and fails if the
// code and the prose drift apart.
func TestSupportedPlatformsMatchTheContractDocument(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "runtime-contract.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(body)

	if !strings.Contains(doc, "Supported platforms") {
		t.Fatalf("%s has no supported-platforms row", path)
	}
	for _, platform := range SupportedPlatforms {
		if !strings.Contains(doc, "`"+platform+"`") {
			t.Errorf("runtime-contract.md does not name the supported platform %s", platform)
		}
	}
	// Everything off the list must be declared unsupported, not left implied.
	if !strings.Contains(doc, "no guarantee") && !strings.Contains(doc, "unsupported") {
		t.Errorf("runtime-contract.md does not state what lies outside the platform contract")
	}
}
