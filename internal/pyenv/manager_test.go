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

// requirePython returns the interpreter the pyenv tests exercise, failing rather
// than skipping when it is missing. Every caller here symlinks a real interpreter
// into a fake uv environment and pins the version it reports, so without python3
// the case would not be weaker evidence -- it would be no evidence, in a suite
// that still reports green. CI installs python3 via actions/setup-python in
// .github/workflows/test.yml, so a missing interpreter is a broken environment.
func requirePython(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is a required test dependency and CI provides it (actions/setup-python in .github/workflows/test.yml): %v", err)
	}
	return path
}

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
	python := requirePython(t)
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
	m := Manager{DataDir: data, Probe: func(context.Context, string) error { return nil }}
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

// The readiness marker must describe the interpreter by its place inside the
// environment, not by where that environment happened to sit when it was
// prepared. Otherwise restoring the data directory elsewhere leaves every
// prepared environment pointing at a path that no longer exists.
func TestPreparedEnvironmentSurvivesADataDirectoryMove(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, "3.13.5")
	spec, err := (Manager{DataDir: data}).Resolve(dir, "one", "uv 0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	m := Manager{DataDir: data}
	envDir, err := m.envDir(spec)
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
	if err := writeReadyMarker(envDir, Ready{Spec: spec, Interpreter: interpreter, UVVersion: "uv 0.5.0"}); err != nil {
		t.Fatal(err)
	}

	// The marker records the path relative to its own environment.
	body, err := os.ReadFile(filepath.Join(envDir, "otter-ready.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored Ready
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Interpreter != "bin/python" {
		t.Fatalf("marker stores interpreter %q, want the relative path bin/python", stored.Interpreter)
	}

	// Moving the whole data directory -- what a restore or a placement change
	// does -- must not invalidate the environment.
	movedRoot := filepath.Join(t.TempDir(), "restored")
	if err := os.Rename(data, movedRoot); err != nil {
		t.Fatal(err)
	}
	ready, err := (Manager{DataDir: movedRoot}).GetReady(spec)
	if err != nil {
		t.Fatalf("an environment under a moved data directory was refused: %v", err)
	}
	want := filepath.Join(movedRoot, "environments", spec.Digest, "bin", "python")
	if ready.Interpreter != want {
		t.Errorf("resolved interpreter = %s, want the moved path %s", ready.Interpreter, want)
	}
}

// A readiness marker is readable by anything that can read the environment, so
// a stored path that climbs out of its own environment must be refused at read
// time. Accepting it would let a doctored marker run another environment's -- or
// any writable -- binary as the daemon.
func TestGetReadyRefusesAnInterpreterOutsideItsEnvironment(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, "3.13.5")
	m := Manager{DataDir: data}
	spec, err := m.Resolve(dir, "one", "uv 0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	envDir, err := m.envDir(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(data, "outside-python")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Written by hand, because the marker writer refuses to produce this.
	body, err := json.Marshal(Ready{Spec: spec, Interpreter: "../../outside-python", UVVersion: "uv 0.5.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "otter-ready.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetReady(spec); err == nil {
		t.Fatal("a marker naming an interpreter outside its environment was accepted")
	}
}

// recordedIdentityOf mirrors what the daemon stores on a run.
func recordedIdentityOf(spec Spec) Spec {
	return RecordedIdentity(spec.Job, spec.Python, spec.Digest, spec.Policy)
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
// digest. A disagreement produces two environments for one job, and
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

// Where a vendored uv lives depends on where the data directory is, and both
// sides of a deploy have to find the same binary: uv is part of the environment
// identity, so a daemon that resolved it differently from the deploy would look
// for an environment preparation never created.
func TestUVCandidateFindsBothDeployLayouts(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("uv"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("install root with a data directory", func(t *testing.T) {
		root := t.TempDir()
		uv := filepath.Join(root, "tools", "uv", "uv")
		write(t, uv)

		got, ok := Manager{DataDir: filepath.Join(root, "data")}.uvCandidate()
		if !ok || got != uv {
			t.Errorf("uvCandidate = %q, %v; want %q", got, ok, uv)
		}
	})

	t.Run("workspace with .otter/data", func(t *testing.T) {
		ws := t.TempDir()
		uv := filepath.Join(ws, "tools", "uv", "uv")
		write(t, uv)

		got, ok := Manager{DataDir: filepath.Join(ws, ".otter", "data")}.uvCandidate()
		if !ok || got != uv {
			t.Errorf("uvCandidate = %q, %v; want %q", got, ok, uv)
		}
	})

	t.Run("nothing vendored", func(t *testing.T) {
		if _, ok := (Manager{DataDir: t.TempDir()}).uvCandidate(); ok {
			t.Error("uvCandidate invented a vendored uv")
		}
	})
}

// fakeUVRecording writes a stand-in uv that records which subcommands it was
// asked to run, and builds the environment the way the real one would. It exists
// so a test can assert on the REQUESTS preparation makes -- an install that
// should not happen is invisible in the result, because a fake install succeeds.
func fakeUVRecording(t *testing.T, data, python, log string) string {
	t.Helper()
	uv := filepath.Join(data, "tools", "uv", "uv")
	if err := os.MkdirAll(filepath.Dir(uv), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo 'uv test'; exit 0; fi\n" +
		"if [ \"$1\" = python ]; then echo install >> \"" + log + "\"; exit 0; fi\n" +
		"echo sync >> \"" + log + "\"\n" +
		"mkdir -p \"$UV_PROJECT_ENVIRONMENT/bin\"\n" +
		"ln -sf " + python + " \"$UV_PROJECT_ENVIRONMENT/bin/python\"\n"
	if err := os.WriteFile(uv, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return uv
}

// hostPython stands in for the interpreter the runtime image bakes into a
// read-only directory, in uv's own on-disk layout.
func hostPython(t *testing.T, version, python string) string {
	t.Helper()
	dir := t.TempDir()
	install := filepath.Join(dir, "cpython-"+version+"-linux-x86_64-gnu", "bin")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(python, filepath.Join(install, "python3")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pythonForTest(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix test fixture")
	}
	python := requirePython(t)
	out, err := exec.Command(python, "-c", "import sys; print('.'.join(map(str,sys.version_info[:3])))").Output()
	if err != nil {
		t.Fatal(err)
	}
	return python, strings.TrimSpace(string(out))
}

// The pooled runtime image bakes the interpreter it pins into a read-only
// directory and points UV_PYTHON_INSTALL_DIR at it. Preparation used to install a
// SECOND copy under the data directory, which means fetching over the network
// what is already on disk -- so on a host without egress the environment could
// not be prepared at all, while the interpreter sat right there.
func TestPrepareReusesTheInterpreterTheHostAlreadyProvides(t *testing.T) {
	python, version := pythonForTest(t)
	shared := hostPython(t, version, python)
	t.Setenv(managedPythonDirEnv, shared)

	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, version)
	log := filepath.Join(t.TempDir(), "uv.log")
	uv := fakeUVRecording(t, data, python, log)

	m := Manager{DataDir: data, Probe: func(context.Context, string) error { return nil }}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatalf("preparing against the host's own interpreter must not need an install: %v", err)
	}

	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "install") {
		t.Errorf("uv was asked to install an interpreter the host already provides:\n%s", got)
	}
	if !strings.Contains(string(got), "sync") {
		t.Errorf("no sync happened, so nothing was prepared at all:\n%s", got)
	}
}

// And when the host does NOT provide it, preparation still installs one, so the
// reuse is a preference rather than the only path.
func TestPrepareInstallsWhenTheHostProvidesNoInterpreter(t *testing.T) {
	python, version := pythonForTest(t)
	// A directory in the right shape but for a different version: the pin must
	// match, not merely the directory existing.
	other := "3.99"
	t.Setenv(managedPythonDirEnv, hostPython(t, other, python))

	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, version)
	log := filepath.Join(t.TempDir(), "uv.log")
	uv := fakeUVRecording(t, data, python, log)

	m := Manager{DataDir: data, Probe: func(context.Context, string) error { return nil }}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "install") {
		t.Errorf("nothing was installed for a version the host does not have:\n%s", got)
	}
}

// A pin must match a version boundary. A bare prefix test would let "3.1" accept
// "3.130", and the runtime would then run a job on an interpreter it never asked
// for.
func TestManagedVersionMatchingStopsAtABoundary(t *testing.T) {
	for _, tc := range []struct {
		rest, version string
		want          bool
	}{
		{"3.13.5-linux-x86_64-gnu", "3.13.5", true},
		{"3.13.5-linux-x86_64-gnu", "3.13", true},
		{"3.13.5+freethreaded-linux-x86_64-gnu", "3.13.5", true},
		{"3.13.5-linux-x86_64-gnu", "3.1", false},
		{"3.130.0-linux-x86_64-gnu", "3.1", false},
		{"3.14.0-linux-x86_64-gnu", "3.13", false},
		{"3.13.5-linux-x86_64-gnu", "", false},
	} {
		if got := managedVersionMatches(tc.rest, tc.version); got != tc.want {
			t.Errorf("managedVersionMatches(%q, %q) = %v, want %v", tc.rest, tc.version, got, tc.want)
		}
	}
}
