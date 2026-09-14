package pyenv

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeInputs(t *testing.T, dir, version string) {
	t.Helper()
	for name, value := range map[string]string{
		".python-version": version + "\n",
		"pyproject.toml":  "[project]\nname = 'fixture'\nversion = '0.1.0'\nrequires-python = '>=3.10'\n",
		"uv.lock":         "version = 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveAndReady(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, "3.13.5")
	m := Manager{DataDir: data}
	spec, err := m.Resolve(dir, "one", "uv test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetReady(spec); err == nil {
		t.Fatal("unprepared environment was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte("version = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := m.Resolve(dir, "one", "uv test")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Digest == changed.Digest {
		t.Fatal("lock change did not create a new environment identity")
	}
	writeInputs(t, dir, "3.13")
	if _, err := m.Resolve(dir, "one", "uv test"); err == nil || !strings.Contains(err.Error(), "exact") {
		t.Fatalf("invalid pin: %v", err)
	}
}

func TestPreparePublishesOnlyAfterValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix test fixture")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	versionOut, err := exec.Command(python, "-c", "import sys; print('.'.join(map(str,sys.version_info[:3])))").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, strings.TrimSpace(string(versionOut)))
	// A fake uv exercises the publication protocol without a network download.
	// It lives at a stable path so a second preparation resolves the same
	// policy and therefore the same environment identity.
	uv := filepath.Join(data, "tools", "uv", "uv")
	if err := os.MkdirAll(filepath.Dir(uv), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'uv test'; exit 0; fi\n" +
		"if [ \"$1\" = python ]; then exit 0; fi\n" +
		"mkdir -p \"$UV_PROJECT_ENVIRONMENT/bin\"\nln -s " + python + " \"$UV_PROJECT_ENVIRONMENT/bin/python\"\n"
	if err := os.WriteFile(uv, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	m := Manager{DataDir: data}
	ready, err := m.Prepare(context.Background(), dir, "one", uv)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Interpreter == "" {
		t.Fatal("missing interpreter")
	}
	if _, err := m.GetReady(ready.Spec); err != nil {
		t.Fatal(err)
	}
	// Re-preparing with the same toolchain must reuse the environment rather
	// than rebuild it.
	if again, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatalf("ready environment should be reused: %v", err)
	} else if again.Digest != ready.Digest {
		t.Fatal("re-preparation produced a different environment identity")
	}
	if err := os.Remove(ready.Interpreter); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetReady(ready.Spec); err == nil {
		t.Fatal("deleted interpreter was accepted")
	}
}

// A different preparation policy must produce a different environment
// identity, otherwise changing the recipe or the toolchain would silently
// reuse an environment built by the previous one.
func TestResolveIdentityCoversPreparationPolicy(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, "3.13.5")
	m := Manager{DataDir: data}

	base, err := m.Resolve(dir, "one", "uv 0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	otherUV, err := m.Resolve(dir, "one", "uv 0.6.0")
	if err != nil {
		t.Fatal(err)
	}
	if base.Digest == otherUV.Digest {
		t.Error("a different uv version produced the same identity")
	}
	// The declared inputs are unchanged, so a run recorded under the old
	// policy must still be able to validate its environment.
	if base.InputsDigest != otherUV.InputsDigest {
		t.Error("the uv version leaked into the inputs digest")
	}
	if base.Policy == otherUV.Policy {
		t.Error("the uv version is missing from the recorded policy")
	}

	// A lock change moves both.
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte("version = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := m.Resolve(dir, "one", "uv 0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	if changed.InputsDigest == base.InputsDigest {
		t.Error("a lock change did not move the inputs digest")
	}
	if changed.Digest == base.Digest {
		t.Error("a lock change did not move the environment identity")
	}
}

// A run recorded before a policy change must still resolve the environment it
// was submitted against.
func TestGetReadyAcceptsARecordedIdentity(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, "3.13.5")
	m := Manager{DataDir: data}

	recorded, err := m.Resolve(dir, "one", "uv 0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	envDir, err := m.envDir(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(envDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(envDir, "bin", "python")
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ready := Ready{Spec: recorded, Interpreter: interpreter, UVVersion: "uv 0.5.0"}
	body, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "otter-ready.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	// The toolchain has moved on, but the run keeps its recorded identity.
	moved, err := m.Resolve(dir, "one", "uv 9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Digest == recorded.Digest {
		t.Fatal("fixture is wrong: the policy change did not move the digest")
	}
	if _, err := m.GetReady(recordedIdentityOf(recorded)); err != nil {
		t.Fatalf("a recorded run could not resolve its own environment: %v", err)
	}
	if _, err := m.GetReady(moved); err == nil {
		t.Error("an environment was accepted for a policy it was not prepared under")
	}
}

// recordedIdentityOf mirrors what the daemon stores on a run.
func recordedIdentityOf(spec Spec) Spec {
	return RecordedIdentity(spec.Integration, spec.Python, spec.Digest, spec.Policy)
}

// A host that has lost its preparation toolchain must still be able to run an
// environment that is already prepared. The policy records uv as "absent", so
// a later build with uv present is treated as a different identity.
func TestMissingUVDegradesToAbsentPolicy(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, "3.13.5")
	m := Manager{DataDir: data}

	// Point at a path that cannot exist so the assertion does not depend on
	// whether the machine running the tests happens to have uv installed.
	absent, err := m.ResolveCurrentAt(context.Background(), dir, "one", filepath.Join(data, "tools", "uv", "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(absent.Policy, "uv=absent") {
		t.Fatalf("policy does not record the absent toolchain: %s", absent.Policy)
	}
	// Preparation itself must fail clearly rather than silently doing nothing.
	if _, err := m.Prepare(context.Background(), dir, "one", filepath.Join(data, "tools", "uv", "missing")); err == nil {
		t.Fatal("preparation succeeded without uv")
	} else if !strings.Contains(err.Error(), "uv is required") {
		t.Fatalf("unclear preparation failure without uv: %v", err)
	}
}

// The daemon and `otter release` must resolve uv to the same binary, because
// uv is part of the preparation policy and therefore of the environment
// digest. A disagreement produces two environments for one integration, and
// the run then looks for one preparation never built.
func TestUVPathPrefersTheVendoredCopy(t *testing.T) {
	data := t.TempDir()
	m := Manager{DataDir: data}

	// Nothing vendored yet: an explicit path is honoured, otherwise PATH.
	if got := m.UVPath("/explicit/uv"); got != "/explicit/uv" {
		t.Errorf("UVPath with no vendored copy = %q, want the explicit path", got)
	}
	if got := m.UVPath(""); got != "uv" {
		t.Errorf("UVPath with nothing configured = %q, want the PATH lookup", got)
	}

	// Once a copy is vendored it wins, including over an explicit path.
	vendored := filepath.Join(data, "tools", "uv", "uv")
	if err := os.MkdirAll(filepath.Dir(vendored), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vendored, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := m.UVPath("/explicit/uv"); got != vendored {
		t.Errorf("UVPath ignored the vendored copy: got %q, want %q", got, vendored)
	}
	if got := m.UVPath(""); got != vendored {
		t.Errorf("UVPath with no explicit path = %q, want the vendored %q", got, vendored)
	}

	// Once vendored, the identity is stable no matter what the caller passes.
	dir := t.TempDir()
	writeInputs(t, dir, "3.13.5")
	withExplicit, err := m.ResolveCurrentAt(context.Background(), dir, "one", "/explicit/uv")
	if err != nil {
		t.Fatal(err)
	}
	withoutExplicit, err := m.ResolveCurrent(context.Background(), dir, "one")
	if err != nil {
		t.Fatal(err)
	}
	if withExplicit.Digest != withoutExplicit.Digest {
		t.Errorf("the same inputs produced different environments:\n  %s\n  %s",
			withExplicit.Digest, withoutExplicit.Digest)
	}
	if withExplicit.Policy != withoutExplicit.Policy {
		t.Errorf("uv resolution is not stable:\n  %s\n  %s", withExplicit.Policy, withoutExplicit.Policy)
	}
}

// A deploy puts the data directory beside the vendored toolchain, not inside
// it. Both layouts must resolve, or the daemon and `otter release` disagree
// about which uv they used and therefore about the environment identity.
func TestUVPathFindsTheDeployLayout(t *testing.T) {
	installRoot := t.TempDir()
	data := filepath.Join(installRoot, "data")
	if err := os.MkdirAll(filepath.Join(installRoot, "tools", "uv"), 0o755); err != nil {
		t.Fatal(err)
	}
	vendored := filepath.Join(installRoot, "tools", "uv", "uv")
	if err := os.WriteFile(vendored, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := Manager{DataDir: data}
	if got := m.UVPath(""); got != vendored {
		t.Errorf("UVPath = %q, want the vendored %q", got, vendored)
	}

	// The same must hold when the copy lives under the data directory.
	nested := t.TempDir()
	nestedVendored := filepath.Join(nested, "tools", "uv", "uv")
	if err := os.MkdirAll(filepath.Dir(nestedVendored), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedVendored, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := (Manager{DataDir: nested}).UVPath(""); got != nestedVendored {
		t.Errorf("UVPath = %q, want the nested vendored %q", got, nestedVendored)
	}
}
