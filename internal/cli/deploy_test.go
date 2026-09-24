package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/deploy"
	"github.com/tkoizumi/otter/internal/release"
)

// tempRepo creates a directory that looks enough like an Otter project for
// findProjectRoot and LoadConfig to accept it.
func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/otter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "integrations", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "integrations", "counter", "otter.yaml"), []byte("version: 1\nname: counter\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "integrations", "counter", "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// tempProject is a workspace with no Go source: the shape most deploy targets
// actually have, and the one that used to be impossible to deploy.
func tempProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".otter"), 0o755); err != nil {
		t.Fatal(err)
	}
	integ := filepath.Join(dir, "shopify_integrations", "customer_sync")
	if err := os.MkdirAll(integ, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "version: 1\nname: customer_sync\nentrypoint: main.py\npython:\n  mode: managed\n  path:\n    - ../lib/python\n"
	if err := os.WriteFile(filepath.Join(integ, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(integ, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "shopify_integrations", "lib", "python"), 0o755); err != nil {
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
	if !strings.Contains(stderr, "no otter.yaml found") && !strings.Contains(stderr, "nothing to deploy") {
		t.Errorf("stderr does not explain the empty project:\n%s", stderr)
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

// Where the executables come from is the difference between "deploy from this
// project" and "deploy from the runtime checkout". It must be automatic, and
// each override must be honoured.
func TestBinarySourceSelection(t *testing.T) {
	base := t.TempDir()

	pythonProject := filepath.Join(base, "python")
	if err := os.MkdirAll(filepath.Join(pythonProject, ".otter"), 0o755); err != nil {
		t.Fatal(err)
	}

	goCheckout := filepath.Join(base, "checkout")
	for _, pkg := range []string{"cmd/otterd", "cmd/otter"} {
		if err := os.MkdirAll(filepath.Join(goCheckout, filepath.FromSlash(pkg)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a python project fetches the release", func(t *testing.T) {
		src, err := binarySourceFor(pythonProject, &deploy.Flags{}, "0.1.13", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		release, ok := src.(deploy.ReleaseBinaries)
		if !ok {
			t.Fatalf("source = %T, want ReleaseBinaries", src)
		}
		if release.Version != "0.1.13" {
			t.Errorf("version = %q, want 0.1.13", release.Version)
		}
	})

	t.Run("a go checkout compiles from itself", func(t *testing.T) {
		src, err := binarySourceFor(goCheckout, &deploy.Flags{}, "dev", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		compile, ok := src.(deploy.CompileSource)
		if !ok {
			t.Fatalf("source = %T, want CompileSource", src)
		}
		if compile.Dir != goCheckout {
			t.Errorf("dir = %q, want the project %q", compile.Dir, goCheckout)
		}
	})

	// The case that blocked a Python workspace deployed by a development
	// build: the project has no Go source, and the build has no release to
	// fetch, but the checkout it was built from is still on disk.
	t.Run("--source names another checkout", func(t *testing.T) {
		src, err := binarySourceFor(pythonProject, &deploy.Flags{Source: goCheckout}, "v0.1.13-dirty", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		compile, ok := src.(deploy.CompileSource)
		if !ok {
			t.Fatalf("source = %T, want CompileSource", src)
		}
		if compile.Dir != goCheckout {
			t.Errorf("dir = %q, want %q", compile.Dir, goCheckout)
		}
	})

	t.Run("a dirty build compiles from its own checkout", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(goCheckout, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(goCheckout, "bin", "otter")
		if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		restore := executablePath
		executablePath = func() (string, error) { return exe, nil }
		t.Cleanup(func() { executablePath = restore })

		src, err := binarySourceFor(pythonProject, &deploy.Flags{}, "v0.1.13-dirty", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		compile, ok := src.(deploy.CompileSource)
		if !ok {
			t.Fatalf("source = %T, want CompileSource from the checkout the binary lives in", src)
		}
		if compile.Dir != goCheckout {
			t.Errorf("dir = %q, want %q", compile.Dir, goCheckout)
		}

		// A released build must still prefer the published archive, even when
		// a checkout happens to sit next to the binary.
		src, err = binarySourceFor(pythonProject, &deploy.Flags{}, "0.1.13", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := src.(deploy.ReleaseBinaries); !ok {
			t.Errorf("a released build chose %T, want the published release", src)
		}
	})

	t.Run("--binaries wins", func(t *testing.T) {
		src, err := binarySourceFor(goCheckout, &deploy.Flags{Binaries: base}, "dev", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		dir, ok := src.(deploy.DirBinaries)
		if !ok {
			t.Fatalf("source = %T, want DirBinaries", src)
		}
		want, _ := filepath.Abs(base)
		if dir.Dir != want {
			t.Errorf("dir = %q, want %q", dir.Dir, want)
		}
	})

	t.Run("--build without source explains the way out", func(t *testing.T) {
		_, err := binarySourceFor(pythonProject, &deploy.Flags{Build: true}, "dev", io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--source") {
			t.Errorf("err = %v, want it to name --source", err)
		}
	})

	t.Run("--source must be a checkout", func(t *testing.T) {
		_, err := binarySourceFor(pythonProject, &deploy.Flags{Source: pythonProject}, "dev", io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not an Otter checkout") {
			t.Errorf("err = %v, want it to reject a non-checkout", err)
		}
	})
}

// findProjectRoot must accept a project that is not a Go module, and must walk
// up from a subdirectory to the directory carrying the marker.
func TestFindProjectRootWalksUpToAProject(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "shopify_integrations", "customer_sync")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".otter"), 0o755); err != nil {
		t.Fatal(err)
	}

	chdir(t, nested)
	got, err := findProjectRoot()
	if err != nil {
		t.Fatal(err)
	}
	// macOS t.TempDir may return a /private symlink; compare resolved paths.
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Errorf("project root = %q, want %q", resolved, want)
	}
}
