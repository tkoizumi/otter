package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otter-runtime/otter/internal/config"
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

// releaseRelativeName decides where a shared tree lands inside a release. The
// landing place has to reproduce the checkout's relative depth, or the
// manifest's own python.path stops resolving after activation.
func TestReleaseRelativeName(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		target   string
		want     string
		wantFail bool
	}{
		{
			name:   "sibling shared tree at the repo root",
			source: "/repo/integrations/foo",
			target: "/repo/lib",
			want:   "lib",
		},
		{
			name:   "a tree deeper inside a shared root",
			source: "/repo/integrations/foo",
			target: "/repo/lib/python/connectors",
			want:   "lib/python/connectors",
		},
		{
			name:   "nested integration directories",
			source: "/repo/integrations/team/foo",
			target: "/repo/lib",
			want:   "lib",
		},
		{
			name:     "no common ancestor with the source directory",
			source:   "/repo/integrations/foo",
			target:   "/elsewhere/lib",
			wantFail: true,
		},
		{
			name:     "a tree inside the integration itself needs no capture",
			source:   "/repo/integrations/foo",
			target:   "/repo/integrations/foo/vendor",
			wantFail: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := releaseRelativeName(tc.source, tc.target)
			if tc.wantFail {
				if ok {
					t.Fatalf("releaseRelativeName(%q, %q) = %q, want no name", tc.source, tc.target, got)
				}
				return
			}
			if !ok {
				t.Fatalf("releaseRelativeName(%q, %q) found no name", tc.source, tc.target)
			}
			if got != tc.want {
				t.Errorf("releaseRelativeName(%q, %q) = %q, want %q", tc.source, tc.target, got, tc.want)
			}
		})
	}
}

// sharedTreesFor must take the manifest's own declared paths as authoritative
// and skip anything already inside the integration directory.
func TestSharedTreesForUsesManifestPaths(t *testing.T) {
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

	trees := sharedTreesFor(source, manifest, "")
	if len(trees) != 1 {
		t.Fatalf("captured %d shared trees, want only the one outside the integration: %+v", len(trees), trees)
	}
	if trees[0].Name != "lib" {
		t.Errorf("shared tree landed as %q, want lib", trees[0].Name)
	}
	if !strings.HasSuffix(trees[0].Source, filepath.Join("repo", "lib")) &&
		trees[0].Source != shared {
		t.Errorf("shared tree source = %q, want %q", trees[0].Source, shared)
	}

	// An explicit --shared entry is additive and deduplicated.
	extra := filepath.Join(root, "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	trees = sharedTreesFor(source, manifest, "../../lib,"+extra)
	if len(trees) != 2 {
		t.Fatalf("captured %d shared trees with --shared, want 2: %+v", len(trees), trees)
	}
}
