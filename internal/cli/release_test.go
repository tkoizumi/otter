package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/release"
)

// releaseWorkspace lays out a workspace with one integration and returns the
// workspace root and the integration directory.
func releaseWorkspace(t *testing.T, name string) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntegrationFixture(t, dir, name, "print('ok')\n")
	return root, dir
}

// writeIntegrationFixture writes a minimal valid external-Python integration.
func writeIntegrationFixture(t *testing.T, dir, name, python string) {
	t.Helper()
	manifest := "version: 1\nname: " + name + "\nentrypoint: main.py\n"
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(python), 0o644); err != nil {
		t.Fatal(err)
	}
}

// otterIn runs one CLI invocation with the working directory set to dir.
func otterIn(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	inWorkspace(t, dir, func() {
		var out, errOut bytes.Buffer
		app := New("test", &out, &errOut)
		code = app.Run(context.Background(), args)
		stdout, stderr = out.String(), errOut.String()
	})
	return stdout, stderr, code
}

// `cd counter && otter release` releases counter: identity follows the working
// directory, the way `otter run` already does.
func TestReleaseFromInsideTheIntegrationDirectory(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")

	stdout, stderr, code := otterIn(t, dir, "release")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "counter: activated") {
		t.Errorf("output does not report the activation:\n%s", stdout)
	}
	// It landed in the workspace's own data directory, not a cwd-relative one
	// that no daemon reads.
	if _, err := os.Stat(filepath.Join(root, stateDirName, "data", release.DirName, "counter")); err != nil {
		t.Errorf("the release did not land in the workspace data directory: %v", err)
	}

	// And --list agrees that something is active.
	stdout, stderr, code = otterIn(t, dir, "release", "--list")
	if code != 0 {
		t.Fatalf("list exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "active release:") {
		t.Errorf("list does not report an active release:\n%s", stdout)
	}
}

// A relative path is read from disk, so `otter release ./counter` works from
// the workspace root as well as the "." form does from inside.
func TestReleaseAcceptsAPathReference(t *testing.T) {
	root, _ := releaseWorkspace(t, "counter")

	stdout, stderr, code := otterIn(t, root, "release", "counter")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "counter: activated") {
		t.Errorf("output does not report the activation:\n%s", stdout)
	}
}

// A bare name works from anywhere in the workspace, and --all covers every
// integration in it.
func TestReleaseByNameAndAll(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	if err := os.MkdirAll(filepath.Join(root, "group", "two"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntegrationFixture(t, filepath.Join(root, "group", "two"), "two", "print('two')\n")

	if stdout, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
		t.Fatalf("release one exited %d: %s", code, stderr)
	} else if !strings.Contains(stdout, "one: activated") {
		t.Errorf("output does not report the activation:\n%s", stdout)
	}

	stdout, stderr, code := otterIn(t, root, "release", "--all")
	if code != 0 {
		t.Fatalf("release --all exited %d: %s", code, stderr)
	}
	for _, want := range []string{"one: activated", "two: activated"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--all did not release everything (%q missing):\n%s", want, stdout)
		}
	}
}

// Rollback: an older staged digest can be activated without staging anything,
// which is the point of keeping releases content-addressed.
func TestReleaseActivateRollsBackToAStagedDigest(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}

	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}
	first, ok, err := manager.Active("counter")
	if err != nil || !ok {
		t.Fatalf("no active release after releasing: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("second release exited %d: %s", code, stderr)
	}
	second, _, err := manager.Active("counter")
	if err != nil {
		t.Fatalf("read active release: %v", err)
	}
	if second.Digest == first.Digest {
		t.Fatal("editing the source did not produce a new release")
	}

	// A prefix is enough, matching how --list prints digests.
	stdout, stderr, code := otterIn(t, dir, "release", "--activate", first.Digest[:12])
	if code != 0 {
		t.Fatalf("--activate exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, first.Digest[:12]) {
		t.Errorf("output does not name the activated release:\n%s", stdout)
	}
	back, _, err := manager.Active("counter")
	if err != nil {
		t.Fatalf("read active release: %v", err)
	}
	if back.Digest != first.Digest {
		t.Errorf("active release = %s, want the rolled-back %s", back.Digest[:12], first.Digest[:12])
	}
}

// An unknown digest is refused, and the command that shows what is available is
// named rather than leaving the developer to guess.
func TestReleaseActivateRefusesAnUnknownDigest(t *testing.T) {
	_, dir := releaseWorkspace(t, "counter")
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	_, stderr, code := otterIn(t, dir, "release", "--activate", "deadbeefdead")
	if code == 0 {
		t.Fatal("activating an unknown digest succeeded")
	}
	if !strings.Contains(stderr, "--list") {
		t.Errorf("refusal does not name the list command:\n%s", stderr)
	}
}

// Outside a workspace a release has nowhere to go, and says so instead of
// scanning a relative ./integrations that is not there.
func TestReleaseRefusesOutsideAWorkspace(t *testing.T) {
	dir := t.TempDir()

	_, stderr, code := otterIn(t, dir, "release")
	if code == 0 {
		t.Fatal("release outside a workspace succeeded")
	}
	if !strings.Contains(stderr, "no workspace here") {
		t.Errorf("refusal does not explain itself:\n%s", stderr)
	}
	if strings.Contains(stderr, "is otterd running") {
		t.Errorf("a local path failure blamed the daemon:\n%s", stderr)
	}
}

// Naming one integration and also asking for all of them is a contradiction,
// not a silently ignored flag.
func TestReleaseRejectsAllWithAName(t *testing.T) {
	_, dir := releaseWorkspace(t, "counter")

	_, stderr, code := otterIn(t, dir, "release", "--all", "counter")
	if code == 0 {
		t.Fatal("--all with a name succeeded")
	}
	if !strings.Contains(stderr, "--all") {
		t.Errorf("refusal does not explain the conflict:\n%s", stderr)
	}
}

// --- layout coverage --------------------------------------------------------

// writeSharedIntegration lays a minimal external integration with python.path
// relative to its own directory, plus the shared tree it names.
func writeSharedIntegration(t *testing.T, dir, name, pythonPath string, sharedRel string) string {
	t.Helper()
	manifest := "version: 1\nname: " + name + "\nentrypoint: main.py\npython:\n  mode: external\n  path:\n    - " + pythonPath + "\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("from greet import hi\nprint(hi())\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(dir, filepath.FromSlash(sharedRel))
	if err := os.MkdirAll(filepath.Join(shared, "greet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "greet", "__init__.py"), []byte("def hi():\n    return \"hi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return shared
}

// activeSource resolves the code directory of an integration's active release.
func activeSource(t *testing.T, manager release.Manager, id string) (release.Metadata, string) {
	t.Helper()
	meta, ok, err := manager.Active(id)
	if err != nil || !ok {
		t.Fatalf("no active release for %s (ok=%v err=%v)", id, ok, err)
	}
	src, err := manager.SourceDir(meta)
	if err != nil {
		t.Fatalf("resolve active source: %v", err)
	}
	return meta, src
}

// assertManifestResolves validates the snapshot's own manifest, which is the
// check that proves the shared tree was captured at the depth the manifest
// names rather than left on the live tree.
func assertManifestResolves(t *testing.T, sourceDir string) {
	t.Helper()
	if _, err := config.LoadAndValidate(filepath.Join(sourceDir, config.ManifestFileName)); err != nil {
		t.Fatalf("the snapshot manifest does not resolve inside the release: %v", err)
	}
}

// The flat workspace: the integration sits at the workspace root and its shared
// tree beside it. This is the shape the old hardcoded integrations/<name>
// placement broke.
func TestReleaseFlatWorkspaceImportsSharedCode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "demo")
	writeSharedIntegration(t, dir, "demo", "../lib/python", "../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, "demo")
	if meta.IntegrationPath != "demo" {
		t.Errorf("IntegrationPath = %q, want demo", meta.IntegrationPath)
	}
	assertManifestResolves(t, src)
	if _, err := os.Stat(filepath.Join(filepath.Dir(src), "lib", "python", "greet", "__init__.py")); err != nil {
		t.Errorf("the shared tree was not captured beside the integration: %v", err)
	}
}

// The canonical checkout layout still works unchanged.
func TestReleaseCanonicalLayoutImportsSharedCode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "integrations", "demo")
	writeSharedIntegration(t, dir, "demo", "../../lib/python", "../../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, "demo")
	if meta.IntegrationPath != "integrations/demo" {
		t.Errorf("IntegrationPath = %q, want integrations/demo", meta.IntegrationPath)
	}
	assertManifestResolves(t, src)
}

// A grouped integration: the tree is placed relative to the same base as the
// integration, so ../lib/python resolves from group/<name>.
func TestReleaseGroupedLayoutImportsSharedCode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "group", "demo")
	writeSharedIntegration(t, dir, "demo", "../lib/python", "../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, "demo")
	if meta.IntegrationPath != "group/demo" {
		t.Errorf("IntegrationPath = %q, want group/demo", meta.IntegrationPath)
	}
	assertManifestResolves(t, src)
}

// The directory name determines relative paths, not the manifest's name, so a
// directory whose basename differs from `name` still lands at its own path.
func TestReleasePlacementUsesTheDirectoryNotTheName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "integrations", "dir-name")
	writeSharedIntegration(t, dir, "other-name", "../../lib", "../../lib")

	if _, stderr, code := otterIn(t, root, "release", filepath.Join("integrations", "dir-name")); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, "other-name")
	if meta.IntegrationPath != "integrations/dir-name" {
		t.Errorf("IntegrationPath = %q, want integrations/dir-name", meta.IntegrationPath)
	}
	assertManifestResolves(t, src)
}

// --source names a tree outside the discovery root; the same ancestor rule
// still places it and its shared code relative to one base.
func TestReleaseSourceOutsideTheDiscoveryRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "other", "demo")
	writeSharedIntegration(t, dir, "demo", "../../lib/python", "../../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "--source", "other/demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, "demo")
	if meta.IntegrationPath != "other/demo" {
		t.Errorf("IntegrationPath = %q, want other/demo", meta.IntegrationPath)
	}
	assertManifestResolves(t, src)
}

// --- preserve the active release on failure ---------------------------------

func TestFailedReleasePreservesTheActiveRelease(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}
	first, _, err := manager.Active("counter")
	if err != nil {
		t.Fatal(err)
	}

	// (1) Staging fails: an escaping symlink cannot be captured.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "live.py"), []byte("LIVE = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape.py")
	if err := os.Symlink(filepath.Join(outside, "live.py"), link); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code == 0 {
		t.Fatal("a release captured a symlink that escapes it")
	} else if !strings.Contains(stderr, "outside the release") {
		t.Errorf("staging failure does not explain itself:\n%s", stderr)
	}
	assertActive(t, manager, "counter", first.Digest)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	// (2) Validation fails: an absolute python.path passes on this machine but
	// cannot be reproduced in a release.
	absolute := t.TempDir()
	writeIntegrationFixture(t, dir, "counter", "print('ok')\n")
	manifest := "version: 1\nname: counter\nentrypoint: main.py\npython:\n  mode: external\n  path:\n    - " + absolute + "\n"
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code == 0 {
		t.Fatal("a release accepted an absolute python.path")
	} else if !strings.Contains(stderr, "absolute") {
		t.Errorf("validation failure does not explain itself:\n%s", stderr)
	}
	assertActive(t, manager, "counter", first.Digest)

	// (3) Activation fails: a release whose recorded placement escapes the
	// release root is refused.
	digest := strings.Repeat("d", 64)
	corrupt := filepath.Join(root, stateDirName, "data", release.DirName, "counter", digest)
	if err := os.MkdirAll(corrupt, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(release.Metadata{
		Integration:     "counter",
		Digest:          digest,
		IntegrationPath: "../escape",
		Source:          dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, release.ManifestFileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release", "--activate", digest[:12]); code == 0 {
		t.Fatal("activated a release whose integration path escapes it")
	} else if !strings.Contains(stderr, "escapes the release root") {
		t.Errorf("activation failure does not explain itself:\n%s", stderr)
	}
	assertActive(t, manager, "counter", first.Digest)
}

// A rollback to a release whose snapshot manifest no longer resolves fails
// cleanly, and the previously active release keeps serving.
func TestFailedActivatePreservesTheActiveRelease(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}

	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}
	first, _, err := manager.Active("counter")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("second release exited %d: %s", code, stderr)
	}
	second, _, err := manager.Active("counter")
	if err != nil {
		t.Fatal(err)
	}

	// Rollback works and is a first-class activation.
	if stdout, stderr, code := otterIn(t, dir, "release", "--activate", first.Digest[:12]); code != 0 {
		t.Fatalf("rollback exited %d: %s", code, stderr)
	} else if !strings.Contains(stdout, first.Digest[:12]) {
		t.Errorf("rollback output does not name the release:\n%s", stdout)
	}
	assertActive(t, manager, "counter", first.Digest)

	// Break the newer snapshot's manifest, then try to activate it.
	_, secondSource := activeSourceFor(t, manager, "counter", second.Digest)
	if err := os.Remove(filepath.Join(secondSource, config.ManifestFileName)); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release", "--activate", second.Digest[:12]); code == 0 {
		t.Fatal("activated a release whose manifest no longer resolves")
	} else if !strings.Contains(stderr, "invalid") {
		t.Errorf("refusal does not explain itself:\n%s", stderr)
	}
	assertActive(t, manager, "counter", first.Digest)
}

// A managed release whose environment was never prepared cannot be activated
// through --activate; the refusal names otter prepare.
func TestActivateRefusesAnUnpreparedManagedRelease(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	dataDir := filepath.Join(root, stateDirName, "data")
	manager := release.Manager{DataDir: dataDir}
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}
	first, _, err := manager.Active("counter")
	if err != nil {
		t.Fatal(err)
	}

	// Hand-build a managed snapshot with no prepared environment.
	digest := strings.Repeat("e", 64)
	source := filepath.Join(dataDir, release.DirName, "counter", digest, "integrations", "counter")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "version: 1\nname: counter\nentrypoint: main.py\npython:\n  mode: managed\n"
	if err := os.WriteFile(filepath.Join(source, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".python-version", "pyproject.toml", "uv.lock"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("3.13.5\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	meta := release.Metadata{
		Integration:     "counter",
		Digest:          digest,
		IntegrationPath: "integrations/counter",
		Source:          dir,
	}
	body, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(source)), release.ManifestFileName), body, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := otterIn(t, dir, "release", "--activate", digest[:12]); code == 0 {
		t.Fatal("activated a managed release with no prepared environment")
	} else if !strings.Contains(stderr, "otter prepare") {
		t.Errorf("refusal does not name otter prepare:\n%s", stderr)
	}
	assertActive(t, manager, "counter", first.Digest)
}

func assertActive(t *testing.T, manager release.Manager, id, digest string) {
	t.Helper()
	active, ok, err := manager.Active(id)
	if err != nil || !ok {
		t.Fatalf("no active release for %s (ok=%v err=%v)", id, ok, err)
	}
	if active.Digest != digest {
		t.Errorf("active release = %s, want %s", active.Digest[:12], digest[:12])
	}
}

// activeSourceFor resolves the code directory of one specific staged release.
func activeSourceFor(t *testing.T, manager release.Manager, id, digest string) (release.Metadata, string) {
	t.Helper()
	meta, err := manager.Metadata(id, digest)
	if err != nil {
		t.Fatal(err)
	}
	src, err := manager.SourceDir(meta)
	if err != nil {
		t.Fatal(err)
	}
	return meta, src
}
