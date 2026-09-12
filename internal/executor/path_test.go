package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
