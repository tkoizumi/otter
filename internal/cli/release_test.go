package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
)

// releaseWorkspace lays out a workspace with one job and returns the
// workspace root and the job directory.
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
	writeJobFixture(t, dir, name, "print('ok')\n")
	return root, dir
}

// writeJobFixture writes a minimal valid external-Python job.
func writeJobFixture(t *testing.T, dir, name, python string) {
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
func TestReleaseFromInsideTheJobDirectory(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(root, stateDirName, "data", release.DirName, idFor(t, dir))); err != nil {
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
// job in it.
func TestReleaseByNameAndAll(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	if err := os.MkdirAll(filepath.Join(root, "group", "two"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJobFixture(t, filepath.Join(root, "group", "two"), "two", "print('two')\n")

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

// `otter release --package` publishes the canonical release digest alongside the
// archive, with the archive hash as a separate transport value. The two must not
// be the same, or a deploy would point at a tarball hash the runtime never
// computed.
func TestReleasePackagePublishesTheCanonicalDigest(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	pkg := filepath.Join(t.TempDir(), "counter.tar.gz")

	stdout, stderr, code := otterIn(t, root, "release", "--package", pkg, "counter")
	if code != 0 {
		t.Fatalf("release --package exited %d: %s", code, stderr)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		for _, f := range strings.Fields(line) {
			if k, v, ok := strings.Cut(f, "="); ok {
				fields[k] = v
			}
		}
	}
	releaseDigest := fields["release_digest"]
	artifact := fields["artifact_sha256"]
	if releaseDigest == "" || artifact == "" {
		t.Fatalf("output does not publish both values:\n%s", stdout)
	}

	data, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatalf("the package was not written: %v", err)
	}
	sum := sha256.Sum256(data)
	if want := "sha256:" + hex.EncodeToString(sum[:]); artifact != want {
		t.Errorf("artifact_sha256 = %s, want the hash of the file %s", artifact, want)
	}
	if artifact == "sha256:"+releaseDigest {
		t.Error("the artifact hash and the release digest are the same value; they are different things")
	}

	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, ok, err := manager.Active(idFor(t, dir))
	if err != nil || !ok {
		t.Fatalf("no active release after packaging: ok=%v err=%v", ok, err)
	}
	if meta.Digest != releaseDigest {
		t.Errorf("published release_digest %s, active release %s", releaseDigest, meta.Digest)
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
	first, ok, err := manager.Active(idFor(t, dir))
	if err != nil || !ok {
		t.Fatalf("no active release after releasing: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("second release exited %d: %s", code, stderr)
	}
	second, _, err := manager.Active(idFor(t, dir))
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
	back, _, err := manager.Active(idFor(t, dir))
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
// scanning a relative ./jobs that is not there.
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

// Naming one job and also asking for all of them is a contradiction,
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

// writeSharedJob lays a minimal external job with python.path
// relative to its own directory, plus the shared tree it names.
func writeSharedJob(t *testing.T, dir, name, pythonPath string, sharedRel string) string {
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

// idFor reads the durable identity the runtime assigned to a job
// directory. Releases and environments are keyed by it, not by the manifest
// label, so a test that looks one up must ask the marker.
func idFor(t *testing.T, dir string) string {
	t.Helper()
	id, err := identity.ReadMarker(dir)
	if err != nil {
		t.Fatalf("read identity marker in %s: %v", dir, err)
	}
	return id.String()
}

// activeSource resolves the code directory of a job's active release.
func activeSource(t *testing.T, manager release.Manager, dir string) (release.Metadata, string) {
	t.Helper()
	id := idFor(t, dir)
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

// The flat workspace: the job sits at the workspace root and its shared
// tree beside it. This is the shape the old hardcoded jobs/<name>
// placement broke.
func TestReleaseFlatWorkspaceImportsSharedCode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "demo")
	writeSharedJob(t, dir, "demo", "../lib/python", "../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, dir)
	if meta.JobPath != "demo" {
		t.Errorf("JobPath = %q, want demo", meta.JobPath)
	}
	assertManifestResolves(t, src)
	if _, err := os.Stat(filepath.Join(filepath.Dir(src), "lib", "python", "greet", "__init__.py")); err != nil {
		t.Errorf("the shared tree was not captured beside the job: %v", err)
	}
}

// The canonical checkout layout still works unchanged.
func TestReleaseCanonicalLayoutImportsSharedCode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "jobs", "demo")
	writeSharedJob(t, dir, "demo", "../../lib/python", "../../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, dir)
	if meta.JobPath != "jobs/demo" {
		t.Errorf("JobPath = %q, want jobs/demo", meta.JobPath)
	}
	assertManifestResolves(t, src)
}

// A grouped job: the tree is placed relative to the same base as the
// job, so ../lib/python resolves from group/<name>.
func TestReleaseGroupedLayoutImportsSharedCode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "group", "demo")
	writeSharedJob(t, dir, "demo", "../lib/python", "../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, dir)
	if meta.JobPath != "group/demo" {
		t.Errorf("JobPath = %q, want group/demo", meta.JobPath)
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
	dir := filepath.Join(root, "jobs", "dir-name")
	writeSharedJob(t, dir, "other-name", "../../lib", "../../lib")

	if _, stderr, code := otterIn(t, root, "release", filepath.Join("jobs", "dir-name")); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, dir)
	if meta.JobPath != "jobs/dir-name" {
		t.Errorf("JobPath = %q, want jobs/dir-name", meta.JobPath)
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
	writeSharedJob(t, dir, "demo", "../../lib/python", "../../lib/python")

	if _, stderr, code := otterIn(t, root, "release", "--source", "other/demo"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	meta, src := activeSource(t, manager, dir)
	if meta.JobPath != "other/demo" {
		t.Errorf("JobPath = %q, want other/demo", meta.JobPath)
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
	first, _, err := manager.Active(idFor(t, dir))
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
	assertActive(t, manager, idFor(t, dir), first.Digest)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	// (2) Validation fails: an absolute python.path passes on this machine but
	// cannot be reproduced in a release.
	absolute := t.TempDir()
	writeJobFixture(t, dir, "counter", "print('ok')\n")
	manifest := "version: 1\nname: counter\nentrypoint: main.py\npython:\n  mode: external\n  path:\n    - " + absolute + "\n"
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code == 0 {
		t.Fatal("a release accepted an absolute python.path")
	} else if !strings.Contains(stderr, "absolute") {
		t.Errorf("validation failure does not explain itself:\n%s", stderr)
	}
	assertActive(t, manager, idFor(t, dir), first.Digest)

	// (3) Activation fails: a release whose recorded placement escapes the
	// release root is refused.
	digest := strings.Repeat("d", 64)
	corrupt := filepath.Join(root, stateDirName, "data", release.DirName, idFor(t, dir), digest)
	if err := os.MkdirAll(corrupt, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(release.Metadata{
		Job:     idFor(t, dir),
		Digest:  digest,
		JobPath: "../escape",
		Source:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, release.ManifestFileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release", "--activate", digest[:12]); code == 0 {
		t.Fatal("activated a release whose job path escapes it")
	} else if !strings.Contains(stderr, "escapes the release root") {
		t.Errorf("activation failure does not explain itself:\n%s", stderr)
	}
	assertActive(t, manager, idFor(t, dir), first.Digest)
}

// A rollback to a release whose snapshot manifest no longer resolves fails
// cleanly, and the previously active release keeps serving.
func TestFailedActivatePreservesTheActiveRelease(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}

	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}
	first, _, err := manager.Active(idFor(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("second release exited %d: %s", code, stderr)
	}
	second, _, err := manager.Active(idFor(t, dir))
	if err != nil {
		t.Fatal(err)
	}

	// Rollback works and is a first-class activation.
	if stdout, stderr, code := otterIn(t, dir, "release", "--activate", first.Digest[:12]); code != 0 {
		t.Fatalf("rollback exited %d: %s", code, stderr)
	} else if !strings.Contains(stdout, first.Digest[:12]) {
		t.Errorf("rollback output does not name the release:\n%s", stdout)
	}
	assertActive(t, manager, idFor(t, dir), first.Digest)

	// Break the newer snapshot's manifest, then try to activate it.
	_, secondSource := activeSourceFor(t, manager, idFor(t, dir), second.Digest)
	if err := os.Remove(filepath.Join(secondSource, config.ManifestFileName)); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release", "--activate", second.Digest[:12]); code == 0 {
		t.Fatal("activated a release whose manifest no longer resolves")
	} else if !strings.Contains(stderr, "invalid") {
		t.Errorf("refusal does not explain itself:\n%s", stderr)
	}
	assertActive(t, manager, idFor(t, dir), first.Digest)
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
	first, _, err := manager.Active(idFor(t, dir))
	if err != nil {
		t.Fatal(err)
	}

	// Hand-build a managed snapshot with no prepared environment.
	digest := strings.Repeat("e", 64)
	source := filepath.Join(dataDir, release.DirName, idFor(t, dir), digest, "jobs", "counter")
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
		Job:     idFor(t, dir),
		Digest:  digest,
		JobPath: "jobs/counter",
		Source:  dir,
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
	assertActive(t, manager, idFor(t, dir), first.Digest)
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

// Retention must not remove a snapshot a non-terminal run is bound to: the
// attempt would fail because its own release was collected underneath it.
func TestPinnedReleasesIncludesOnlyNonTerminalRuns(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := database.FormatTime(time.Now().UTC())
	insert := func(id, status, digest string) {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO runs (id, job_id, trigger_type, status, attempt, created_at, release_digest)
			 VALUES (?, ?, 'manual', ?, 1, ?, ?)`,
			id, "int-1", status, now, digest); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("run-queued", "queued", strings.Repeat("a", 64))
	insert("run-running", "running", strings.Repeat("b", 64))
	insert("run-done", "succeeded", strings.Repeat("c", 64))

	app := New("test", io.Discard, io.Discard)
	pins, err := app.pinnedReleases(ctx, db, "int-1")
	if err != nil {
		t.Fatalf("pinnedReleases: %v", err)
	}
	if !pins[strings.Repeat("a", 64)] || !pins[strings.Repeat("b", 64)] {
		t.Fatalf("non-terminal runs are not pinned: %+v", pins)
	}
	if pins[strings.Repeat("c", 64)] {
		t.Fatalf("a finished run pinned its release: %+v", pins)
	}

	// A data directory that has never held a runtime has no runs to pin. That
	// is an honest empty set, not an unreadable registry, so it is not an error.
	fresh, err := database.Open(ctx, filepath.Join(t.TempDir(), "fresh"))
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	if _, err := database.Migrate(ctx, fresh); err != nil {
		t.Fatalf("migrate fresh: %v", err)
	}
	pins, err = app.pinnedReleases(ctx, fresh, "int-1")
	if err != nil {
		t.Fatalf("a fresh data directory was treated as unreadable: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("a fresh data directory produced pins: %+v", pins)
	}
}

// The pin set is the gate a prune passes through, so an unreadable registry
// must be an error rather than an empty set: empty means "nothing is pinned",
// which is exactly the wrong thing to assume when the truth is unknown.
func TestPinnedReleasesRefusesAnUnreadableDatabase(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, database.FileName),
		[]byte("this is not a SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := New("test", io.Discard, io.Discard)
	manager := release.Manager{DataDir: dataDir}
	removed, err := app.retainReleases(ctx, manager, "int-1", 1)
	if err == nil {
		t.Fatalf("an unreadable registry yielded a prune: %+v", removed)
	}
}

// seedNonTerminalRun records a queued run bound to a release digest, which is
// what retention must protect. The run is written with the durable job id, the
// key the pin query filters on.
func seedNonTerminalRun(t *testing.T, dataDir, jobID, digest string) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate registry: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, job_id, trigger_type, status, attempt, created_at, release_digest)
		 VALUES (?, ?, 'manual', 'queued', 1, ?, ?)`,
		"run-queued", jobID, database.FormatTime(time.Now().UTC()), digest); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// The prune gate: when the registry cannot be read, retention refuses rather
// than removing releases against an assumed-empty pin set. This is the rule
// `OT-002` records and every prune path shares.
func TestReleaseRetentionRefusesWhenTheRegistryCannotBeRead(t *testing.T) {
	ctx := context.Background()
	root, dir := releaseWorkspace(t, "one")
	dataDir := filepath.Join(root, stateDirName, "data")
	manager := release.Manager{DataDir: dataDir}

	// Three releases, so a keep=1 window would have something to remove.
	for i := 0; i < 3; i++ {
		writeJobFixture(t, dir, "one", fmt.Sprintf("print(%d)\n", i))
		if _, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
			t.Fatalf("release %d exited %d: %s", i, code, stderr)
		}
	}
	id := idFor(t, dir)
	before, err := manager.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 3 {
		t.Fatalf("staged %d releases, want at least 3", len(before))
	}

	// Break the registry after the releases exist. Identity resolution is no
	// longer needed: this exercises the prune gate directly, the way releaseOne
	// calls it once the release is active.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(dataDir, database.FileName+suffix))
	}
	if err := os.WriteFile(filepath.Join(dataDir, database.FileName),
		[]byte("this is not a SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := New("test", io.Discard, io.Discard)
	removed, err := app.retainReleases(ctx, manager, id, 1)
	if err == nil {
		t.Fatalf("retention ran against an unreadable registry and removed %v", removed)
	}
	if !strings.Contains(err.Error(), "no release was removed") {
		t.Errorf("refusal does not state that nothing was removed: %v", err)
	}
	after, err := manager.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused retention removed releases: %d -> %d", len(before), len(after))
	}
}

// The same refusal end to end: identity resolution still succeeds, but the run
// registry cannot be read, so `otter release --keep` must exit non-zero and
// leave every snapshot alone rather than prune against an assumed-empty pin set.
func TestReleaseKeepRefusesWhenTheRunRegistryIsUnreadable(t *testing.T) {
	ctx := context.Background()
	root, dir := releaseWorkspace(t, "one")
	dataDir := filepath.Join(root, stateDirName, "data")
	manager := release.Manager{DataDir: dataDir}

	for i := 0; i < 3; i++ {
		writeJobFixture(t, dir, "one", fmt.Sprintf("print(%d)\n", i))
		if _, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
			t.Fatalf("release %d exited %d: %s", i, code, stderr)
		}
	}
	id := idFor(t, dir)
	before, err := manager.List(id)
	if err != nil {
		t.Fatal(err)
	}

	// Keep the identity registry readable but make the run registry
	// unreadable: the pin query then fails after identity resolution has
	// already succeeded, which is the state the gate exists to catch. Dropping
	// the table rather than corrupting the file is deliberate -- Migrate sees
	// every migration applied and does not recreate it.
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE runs`); err != nil {
		t.Fatalf("drop runs: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := otterIn(t, root, "release", "--keep", "1", "one")
	if code == 0 {
		t.Fatalf("release --keep succeeded against an unreadable run registry:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no release was removed") {
		t.Errorf("refusal does not state that nothing was removed:\n%s", stderr)
	}
	after, err := manager.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused prune removed releases: %d -> %d", len(before), len(after))
	}
}

// A deploy's release step prunes through this path, so `--keep N` must remove
// exactly the releases beyond the window and spare everything still
// load-bearing: the active release, the rollback target, and every digest a
// non-terminal run is bound to.
func TestReleaseKeepPrunesToTheWindowAndProtectsPins(t *testing.T) {
	root, dir := releaseWorkspace(t, "one")
	dataDir := filepath.Join(root, stateDirName, "data")
	manager := release.Manager{DataDir: dataDir}

	// Five distinct releases. The first release also assigns the durable
	// identity the registry pins are keyed by.
	for i := 0; i < 5; i++ {
		writeJobFixture(t, dir, "one", fmt.Sprintf("print(%d)\n", i))
		if _, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
			t.Fatalf("release %d exited %d: %s", i, code, stderr)
		}
	}
	id := idFor(t, dir)
	before, err := manager.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 5 {
		t.Fatalf("staged %d releases, want 5", len(before))
	}
	active := ""
	var inactive []string
	for _, rel := range before {
		if rel.Active {
			active = rel.Digest
			continue
		}
		inactive = append(inactive, rel.Digest)
	}
	if active == "" || len(inactive) != 4 {
		t.Fatalf("active=%q inactive=%d, want one active and four inactive", active, len(inactive))
	}

	// The oldest release is far outside a one-release window, but a queued run
	// is bound to it, so retention must keep it anyway.
	pinned := inactive[len(inactive)-1]
	seedNonTerminalRun(t, dataDir, id, pinned)

	if _, stderr, code := otterIn(t, root, "release", "--keep", "1", "one"); code != 0 {
		t.Fatalf("release --keep 1 exited %d: %s", code, stderr)
	}

	for _, digest := range []string{active, inactive[0], pinned} {
		if _, err := manager.Metadata(id, digest); err != nil {
			t.Errorf("retention removed a protected release %s: %v", digest[:12], err)
		}
	}
	after, err := manager.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 {
		t.Fatalf("kept %d releases, want the active, the rollback target and the pinned one", len(after))
	}
	for _, rel := range after {
		if rel.Digest == inactive[1] || rel.Digest == inactive[2] {
			t.Errorf("retention kept %s, which is outside the window and unpinned", rel.Digest[:12])
		}
	}
}

// The cross-job view answers "what has a release, and what is missing
// one?", which is the question behind a submission that refuses because an
// job was never released.
func TestReleaseListAllReportsReleasesAndGaps(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	two := filepath.Join(root, "two")
	if err := os.MkdirAll(two, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJobFixture(t, two, "two", "print('two')\n")

	// Release only one of them; registering "two" happens as a side effect of
	// the reconcile the release performs.
	if _, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
		t.Fatalf("release one exited %d: %s", code, stderr)
	}

	stdout, stderr, code := otterIn(t, root, "release", "--list", "--all")
	if code != 0 {
		t.Fatalf("release --list --all exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "JOB") || !strings.Contains(stdout, "ACTIVE") {
		t.Fatalf("no table header:\n%s", stdout)
	}

	// "one" has an active release; "two" is registered with none, and says so.
	var oneLine, twoLine string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "one ") {
			oneLine = line
		}
		if strings.HasPrefix(line, "two ") {
			twoLine = line
		}
	}
	if !strings.Contains(oneLine, "active") || strings.Contains(oneLine, " -       0") {
		t.Fatalf("one does not show an active release: %q", oneLine)
	}
	if !strings.Contains(twoLine, "active") {
		t.Fatalf("two is not shown as a registered job: %q", twoLine)
	}
	if !strings.Contains(twoLine, " 0 ") {
		t.Fatalf("two is not shown as having no release: %q", twoLine)
	}
}

// The machine-readable form is what a script or a deploy can consume.
func TestReleaseListAllJSON(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	if _, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	stdout, stderr, code := otterIn(t, root, "--json", "release", "--list", "--all")
	if code != 0 {
		t.Fatalf("release --list --all --json exited %d: %s", code, stderr)
	}
	var rows []struct {
		Name     string `json:"name"`
		ID       string `json:"id"`
		Active   string `json:"active"`
		Releases int    `json:"releases"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("decode rows: %v\n%s", err, stdout)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	// The label is what a human reads; the id is the durable identity that
	// releases are actually keyed by.
	if rows[0].Name != "one" || rows[0].ID == "" || rows[0].ID == "one" ||
		rows[0].Active == "" || rows[0].Releases != 1 {
		t.Fatalf("row = %+v", rows[0])
	}
}

// Orphan releases are the leftovers of a job removed outside the
// registry. Pruning them is explicit, previewed by default, and refuses to act
// until the registry is bootstrapped, because with no registry every release
// would look like an orphan.
func TestReleasePruneRemovesOnlyUnregisteredReleases(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	dataDir := filepath.Join(root, stateDirName, "data")

	// Bootstrap the registry, then release the one registered job.
	if _, stderr, code := otterIn(t, root, "identity", "migrate", "--apply"); code != 0 {
		t.Fatalf("migrate exited %d: %s", code, stderr)
	}
	if _, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	// A release directory with no identity behind it.
	ghost := filepath.Join(dataDir, release.DirName, "ghost", strings.Repeat("f", 64))
	if err := os.MkdirAll(ghost, 0o755); err != nil {
		t.Fatal(err)
	}

	// The default is a plan.
	stdout, stderr, code := otterIn(t, root, "release", "--list", "--all", "--prune")
	if code != 0 {
		t.Fatalf("prune plan exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "would remove ghost") {
		t.Fatalf("plan does not name the orphan:\n%s", stdout)
	}
	if _, err := os.Stat(ghost); err != nil {
		t.Fatalf("a dry run removed the release: %v", err)
	}

	// Applying removes the orphan and nothing else.
	stdout, stderr, code = otterIn(t, root, "release", "--list", "--all", "--prune", "--apply")
	if code != 0 {
		t.Fatalf("prune exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "removed ghost") {
		t.Fatalf("apply did not report the removal:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dataDir, release.DirName, "ghost")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the orphan release survived: %v", err)
	}
	// The registered job is untouched. Its releases are keyed by its
	// durable identity, not by its directory name.
	keptID := idFor(t, filepath.Join(root, "one"))
	if _, err := os.Stat(filepath.Join(dataDir, release.DirName, keptID)); err != nil {
		t.Fatalf("prune removed a registered job's releases: %v", err)
	}
}

func TestReleasePruneRefusesBeforeBootstrap(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	dataDir := filepath.Join(root, stateDirName, "data")
	ghost := filepath.Join(dataDir, release.DirName, "ghost", strings.Repeat("f", 64))
	if err := os.MkdirAll(ghost, 0o755); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := otterIn(t, root, "release", "--list", "--all", "--prune", "--apply")
	if code == 0 {
		t.Fatalf("prune acted on an unbootstrapped registry")
	}
	if !strings.Contains(stderr, "identity migrate") {
		t.Fatalf("refusal does not explain what to run:\n%s", stderr)
	}
	if _, err := os.Stat(ghost); err != nil {
		t.Fatalf("a refused prune removed the release: %v", err)
	}
}

// A deleted identity with no releases left is a tombstone. The registry keeps
// it so the id is never reused, but it is not a release and does not belong in
// the release view; `otter jobs --all` is where it is shown.
func TestReleaseListAllHidesTombstonesWithoutReleases(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	dataDir := filepath.Join(root, stateDirName, "data")
	ctx := context.Background()

	// Seed a deleted identity with nothing left of its own.
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	if err := identity.NewStore(db.DB).CreateInstance(ctx, identity.Instance{
		ID:               identity.MustParse("ghost-id"),
		Name:             "ghost",
		Status:           identity.StatusDeleted,
		Generation:       2,
		CreatedAt:        now,
		RetiredAt:        &now,
		RetirementReason: "deleted by operator",
	}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rows, err := collectReleases(ctx, release.Manager{DataDir: dataDir})
	if err != nil {
		t.Fatalf("collectReleases: %v", err)
	}
	for _, row := range rows {
		if row.ID == "ghost-id" {
			t.Fatalf("a tombstone is listed as a release: %+v", row)
		}
	}

	// It is still recorded, and --all is how you see it.
	stdout, stderr, code := otterIn(t, root, "jobs", "--all")
	if code != 0 {
		t.Fatalf("jobs --all exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "ghost-id") || !strings.Contains(stdout, "deleted") {
		t.Fatalf("the tombstone is not reported by jobs --all:\n%s", stdout)
	}
}

// OT-029: a retired job whose source is still in the repository is discovered by
// a deploy sweep like any other, but the runtime will never register its path
// again. Releasing it must skip and report, not fail the whole sweep -- the real
// host reproduced exactly this, and a bare `otter deploy` died on it.
func TestReleaseSkipsASuppressedPath(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	ctx := context.Background()

	// The first release registers the identity and stages a release.
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}

	// Suppress the path the way `otter delete` does, leaving the source in
	// place: the state the live host was in.
	dataDir := filepath.Join(root, stateDirName, "data")
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := identity.NewStore(db.DB)
	canonical, err := identity.Canonical(dir)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	rec, found, err := store.PathRecord(ctx, canonical)
	if err != nil || !found {
		t.Fatalf("path record: found=%v err=%v", found, err)
	}
	if err := store.SuppressPath(ctx, rec.CanonicalPath, rec.OwnerID, "deleted by operator"); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stdout, stderr, code := otterIn(t, root, "release", dir)
	if code != 0 {
		t.Fatalf("releasing a suppressed path exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "skip:") || !strings.Contains(stdout, "suppressed") {
		t.Fatalf("the suppressed path was not reported as a skip:\n%s", stdout)
	}
}

// OT-010: the pin query and the plan that moves releases aside run in one
// immediate transaction, and the deletion happens after it commits. A queued run
// bound to a release outside the keep window must survive, everything else in
// that window must go, and the trash must be empty once the command returns.
func TestRetainReleasesPinsAQueuedRunAndClearsTheTrash(t *testing.T) {
	ctx := context.Background()
	root, dir := releaseWorkspace(t, "one")
	dataDir := filepath.Join(root, stateDirName, "data")
	manager := release.Manager{DataDir: dataDir}

	// Four releases so a keep=1 window has more than one candidate.
	for i := 0; i < 4; i++ {
		writeJobFixture(t, dir, "one", fmt.Sprintf("print(%d)\n", i))
		if _, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
			t.Fatalf("release %d exited %d: %s", i, code, stderr)
		}
	}
	id := idFor(t, dir)
	before, err := manager.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 4 {
		t.Fatalf("staged %d releases, want at least 4", len(before))
	}
	// List is newest first, so the oldest is the one a keep=1 window would
	// remove first.
	oldest := before[len(before)-1]
	seedNonTerminalRun(t, dataDir, id, oldest.Digest)

	app := New("test", io.Discard, io.Discard)
	removed, err := app.retainReleases(ctx, manager, id, 1)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("retention removed nothing, so it proves nothing")
	}
	for _, digest := range removed {
		if digest == oldest.Digest {
			t.Fatalf("retention removed the release a queued run is bound to")
		}
	}
	if _, err := manager.Metadata(id, oldest.Digest); err != nil {
		t.Errorf("a pinned release is no longer resolvable: %v", err)
	}

	// The slow half ran before the command returned: nothing is left in trash.
	releasesRoot, err := manager.Root()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(releasesRoot, ".trash"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("retention left %d directories in the trash", len(entries))
	}
}
