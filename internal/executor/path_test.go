package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otter-runtime/otter/internal/config"
)

// PYTHONPATH must be ordered: the runtime SDK first (so `import otter` always
// resolves to the daemon's own copy), then shared code the manifest declares,
// then anything the operator already had.
func TestPythonPathOrderPutsTheSDKFirstThenSharedCode(t *testing.T) {
	m, dir := writeScript(t, "pass\n")

	shared := filepath.Join(dir, "shared")
	other := filepath.Join(dir, "other")
	for _, d := range []string{shared, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m.Python.Path = []string{"shared", "other"}

	inherited := "/from/the/operator"
	t.Setenv("PYTHONPATH", inherited)

	env, err := New(testLogger(), "/runtime/sdk").buildEnv(&Request{
		Manifest: m, RunID: "r", TriggerType: "manual",
	})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}

	var got string
	for _, kv := range env {
		if key, value, _ := strings.Cut(kv, "="); key == "PYTHONPATH" {
			got = value
		}
	}

	want := strings.Join([]string{"/runtime/sdk", shared, other, inherited}, string(os.PathListSeparator))
	if got != want {
		t.Errorf("PYTHONPATH =\n  %s\nwant\n  %s", got, want)
	}
}

func TestPythonPathOmittedWhenThereIsNothingToAdd(t *testing.T) {
	m, _ := writeScript(t, "pass\n")

	// No SDK, no declared paths, nothing inherited.
	t.Setenv("PYTHONPATH", "")
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PYTHONPATH=") {
			os.Unsetenv("PYTHONPATH")
		}
	}

	env, err := New(testLogger(), "").buildEnv(&Request{Manifest: m, RunID: "r", TriggerType: "manual"})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PYTHONPATH=") && kv != "PYTHONPATH=" {
			t.Errorf("unexpected %q", kv)
		}
	}
}

// python.path entries that are absolute are passed through untouched.
func TestPythonPathAcceptsAbsoluteEntries(t *testing.T) {
	m, _ := writeScript(t, "pass\n")
	abs := t.TempDir()
	m.Python.Path = []string{abs}

	env, err := New(testLogger(), "").buildEnv(&Request{Manifest: m, RunID: "r", TriggerType: "manual"})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	var got string
	for _, kv := range env {
		if key, value, _ := strings.Cut(kv, "="); key == "PYTHONPATH" {
			got = value
		}
	}
	if !strings.HasPrefix(got, abs) {
		t.Errorf("PYTHONPATH = %q, want it to start with %q", got, abs)
	}
}

// A managed environment must not pass the daemon's world to the child, but it
// must pass the knobs an operator sets per deployment. Swallowing DRY_RUN makes
// a dry run perform real writes, silently.
func TestManagedHostVariables(t *testing.T) {
	allowed := []string{
		// Operational knobs documented for integrations.
		"DRY_RUN", "PAGE_SIZE", "MAX_PAGES_PER_RUN", "RUN_BUDGET_SECONDS",
		"OVERLAP_SECONDS", "SALESFORCE_BATCH_SIZE", "SYNC_ADDRESS",
		"SHOPIFY_SORT_KEY",
		// Process basics a child legitimately needs.
		"HOME", "LANG", "TZ", "TMPDIR", "HTTP_PROXY", "HTTPS_PROXY",
		"SSL_CERT_FILE",
	}
	for _, key := range allowed {
		if !managedHostVariable(key) {
			t.Errorf("managedHostVariable(%q) = false, want true", key)
		}
	}

	// The point of the narrow environment: anything else stays out, above all
	// the daemon's own credential and other integrations' secrets.
	denied := []string{
		"OTTER_API_TOKEN", "OTTER_STATE_TOKEN",
		"SHOPIFY_CLIENT_SECRET", "SALESFORCE_CLIENT_SECRET",
		"PATH", "PYTHONPATH", "PYTHONHOME", "VIRTUAL_ENV",
	}
	for _, key := range denied {
		if managedHostVariable(key) {
			t.Errorf("managedHostVariable(%q) = true, want false", key)
		}
	}
}

// The regression this guards: an operator sets DRY_RUN for the daemon, and the
// managed child must see it. Before the allow-list included it, a dry run
// performed real writes with no error anywhere.
func TestManagedChildReceivesOperationalKnobs(t *testing.T) {
	t.Setenv("DRY_RUN", "1")
	t.Setenv("PAGE_SIZE", "250")
	t.Setenv("SHOPIFY_CLIENT_SECRET", "must-not-leak")
	t.Setenv("OTTER_API_TOKEN", "must-not-leak")

	e := New(testLogger(), "/tmp/sdk")
	req := &Request{
		Managed: true,
		Manifest: &config.Manifest{
			Name:       "probe",
			Version:    config.SupportedVersion,
			Entrypoint: "main.py",
			Dir:        t.TempDir(),
			Python:     config.PythonConfig{Executable: "python3"},
		},
	}

	env, err := e.buildEnv(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")

	if !strings.Contains(joined, "DRY_RUN=1") {
		t.Error("DRY_RUN did not reach a managed child; a dry run would write for real")
	}
	if !strings.Contains(joined, "PAGE_SIZE=250") {
		t.Error("PAGE_SIZE did not reach a managed child")
	}
	for _, secret := range []string{"must-not-leak"} {
		if strings.Contains(joined, secret) {
			t.Errorf("a credential leaked into a managed child: %s", secret)
		}
	}
}
