package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/release"
)

// tempRepo creates a directory that looks enough like an Otter checkout for
// findRepoRoot and LoadConfig to accept it.
func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/otter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "integrations", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "integrations", "counter", "otter.yaml"), []byte("name: counter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// chdir moves into dir for the test and restores the working directory after.
// The CLI resolves its repository root from the working directory, so the
// deploy tests have to run somewhere controlled.
func chdir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})
}

func runDeploy(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := New("test", &stdout, &stderr)
	code := app.cmdDeploy(context.Background(), globals{}, args)
	return code, stdout.String(), stderr.String()
}

func TestDeployRequiresAHost(t *testing.T) {
	chdir(t, tempRepo(t))

	code, _, stderr := runDeploy(t)
	if code == 0 {
		t.Fatal("deploy succeeded without --host")
	}
	if !strings.Contains(stderr, "--host is required") {
		t.Errorf("stderr does not explain the missing host:\n%s", stderr)
	}
}

// A deployment must never be reachable from the network by accident: an API on
// a wildcard address with a static bearer token is the exact anti-pattern an
// SSH tunnel exists to avoid.
func TestDeployRefusesWildcardListen(t *testing.T) {
	chdir(t, tempRepo(t))

	code, _, stderr := runDeploy(t, "--host", "droplet", "--listen", "0.0.0.0:7337")
	if code == 0 {
		t.Fatal("deploy accepted a wildcard listen address")
	}
	if !strings.Contains(stderr, "wildcard listen address") {
		t.Errorf("stderr does not explain the refusal:\n%s", stderr)
	}
}

func TestDeployRejectsUnexpectedArguments(t *testing.T) {
	chdir(t, tempRepo(t))

	code, _, stderr := runDeploy(t, "--host", "droplet", "extra")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unexpected arguments") {
		t.Errorf("stderr does not report the stray argument:\n%s", stderr)
	}
}

func TestDeployStatusWithoutState(t *testing.T) {
	chdir(t, tempRepo(t))

	code, _, stderr := runDeploy(t, "--status")
	if code == 0 {
		t.Fatal("--status succeeded with no recorded deploy")
	}
	if !strings.Contains(stderr, "no recorded deploy") {
		t.Errorf("stderr does not explain the missing state:\n%s", stderr)
	}
}

func TestDeployWithoutIntegrationsFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/otter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	code, _, stderr := runDeploy(t, "--host", "droplet")
	if code == 0 {
		t.Fatal("deploy succeeded with no integrations to ship")
	}
	if !strings.Contains(stderr, "no integrations found") && !strings.Contains(stderr, "read integrations directory") {
		t.Errorf("stderr does not explain the empty checkout:\n%s", stderr)
	}
}

func TestUsageDocumentsDeploy(t *testing.T) {
	var stdout bytes.Buffer
	app := New("test", &stdout, &stdout)
	app.printUsage(&stdout)

	if !strings.Contains(stdout.String(), "deploy --host") {
		t.Errorf("top-level usage does not mention deploy:\n%s", stdout.String())
	}
}

// sharedSourcesFor resolves the manifest's own declared python.path entries;
// the release captures exactly what the integration imports, and --shared adds
// trees the manifest cannot express.
func TestSharedSourcesForUsesManifestPaths(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "integrations", "foo")
	shared := filepath.Join(root, "lib")
	inner := filepath.Join(source, "vendor")
	for _, dir := range []string{source, shared, inner} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := &config.Manifest{
		Dir:    source,
		Python: config.PythonConfig{Path: []string{"../../lib", "vendor"}},
	}

	sources, err := sharedSourcesFor(source, manifest, "")
	if err != nil {
		t.Fatal(err)
	}
	// The integration-local entry is handed to Plan too, which drops it because
	// the integration's own copy already carries it.
	if len(sources) != 2 {
		t.Fatalf("resolved %d shared sources, want the declared lib and vendor: %+v", len(sources), sources)
	}

	layout, err := release.Plan(root, source, []release.SharedTree{{Source: sources[0]}, {Source: sources[1]}})
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Trees) != 1 || layout.Trees[0].Name != "lib" {
		t.Fatalf("layout = %+v, want only lib captured from outside the integration", layout.Trees)
	}

	// An explicit --shared entry is additive and deduplicated.
	extra := filepath.Join(root, "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	sources, err = sharedSourcesFor(source, manifest, "../../lib,"+extra)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 {
		t.Fatalf("resolved %d shared sources with --shared, want 3: %+v", len(sources), sources)
	}
}

// A missing declared tree is a hard error, not a silent omission: a release
// that dropped it would run against live code here and fail on the host.
func TestSharedSourcesForRefusesAMissingTree(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "integrations", "foo")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &config.Manifest{
		Dir:    source,
		Python: config.PythonConfig{Path: []string{"../../gone"}},
	}
	if _, err := sharedSourcesFor(source, manifest, ""); err == nil {
		t.Fatal("a missing python.path tree was accepted")
	}
	// --shared names the same policy.
	if _, err := sharedSourcesFor(source, manifest, filepath.Join(root, "gone")); err == nil {
		t.Fatal("a missing --shared tree was accepted")
	}
}

// An absolute python.path passes the live check on this machine and is a
// missing directory on the host. The release path refuses it up front.
func TestAbsolutePythonPathIsRefusedForRelease(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "integrations", "foo")
	shared := filepath.Join(root, "lib")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &config.Manifest{
		Dir:    source,
		Python: config.PythonConfig{Path: []string{shared}},
	}
	if err := manifest.ValidatePythonPathsForRelease(); err == nil {
		t.Fatal("an absolute python.path was accepted for release")
	}
}

// Deploy releases from a distinct discovery root while the shared library sits
// beside it. The one base rule has to place the integration at
// integrations/<name> and the tree at lib/python, which is what makes the
// manifest's ../../lib/python resolve after activation.
func TestDeployLayoutUsesTheRepositoryBase(t *testing.T) {
	remote := t.TempDir()
	root := filepath.Join(remote, "integrations")
	name := "counter"
	source := filepath.Join(root, name)
	shared := filepath.Join(remote, "lib", "python")
	for _, dir := range []string{source, shared} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	layout, err := release.Plan(root, source, []release.SharedTree{{Source: shared}})
	if err != nil {
		t.Fatal(err)
	}
	if layout.IntegrationPath != "integrations/"+name {
		t.Errorf("integration placed at %q, want integrations/%s", layout.IntegrationPath, name)
	}
	if len(layout.Trees) != 1 || layout.Trees[0].Name != "lib/python" {
		t.Fatalf("shared tree placed at %+v, want lib/python", layout.Trees)
	}
}
