package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
)

// writeIntegration writes a minimal manifest into a fresh directory and
// returns the directory. The manifest name is independent of the directory
// name, which is the case a name-only CLI cannot express.
func writeIntegration(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := "version: 1\nname: " + name + "\nentrypoint: main.py\n"
	if err := os.WriteFile(filepath.Join(dir, config.ManifestFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// chdirForTest points relative-path resolution at dir without changing the
// process-wide working directory.
func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	original := workingDirForTest
	workingDirForTest = func() (string, error) { return dir, nil }
	t.Cleanup(func() { workingDirForTest = original })
}

// A plain name is the daemon's identifier and must reach it untouched: the
// whole point is that the existing spelling keeps working.
func TestResolveIntegrationRefPassesNamesThrough(t *testing.T) {
	for _, ref := range []string{"counter", "shopify-to-salesforce", "erp.sync_v2", "a", ""} {
		got, err := resolveIntegrationRef(ref)
		if err != nil {
			t.Errorf("resolveIntegrationRef(%q): %v", ref, err)
			continue
		}
		if got != ref {
			t.Errorf("resolveIntegrationRef(%q) = %q, want it unchanged", ref, got)
		}
	}
}

// The directory is not the identifier: the manifest name is. Resolving a path
// must therefore read otter.yaml rather than use the directory's own name.
func TestResolveIntegrationRefReadsTheManifestName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "some-directory")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.ManifestFileName),
		[]byte("version: 1\nname: shopify-to-erp\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	original := workingDirForTest
	t.Cleanup(func() { workingDirForTest = original })

	check := func(refs []string) {
		t.Helper()
		for _, ref := range refs {
			got, err := resolveIntegrationRef(ref)
			if err != nil {
				t.Errorf("resolveIntegrationRef(%q): %v", ref, err)
				continue
			}
			if got != "shopify-to-erp" {
				t.Errorf("resolveIntegrationRef(%q) = %q, want shopify-to-erp", ref, got)
			}
		}
	}

	// References that name the directory while standing outside it.
	workingDirForTest = func() (string, error) { return root, nil }
	check([]string{
		dir,                // absolute directory
		"./some-directory", // relative, with separator
		"some-directory/",  // trailing separator
		filepath.Join(dir, config.ManifestFileName), // the manifest itself
	})

	// References that name it while standing inside it, which is the case
	// `otter run .` exists for.
	workingDirForTest = func() (string, error) { return dir, nil }
	check([]string{".", "otter.yaml", filepath.Join(dir, config.ManifestFileName)})
}

// A path that is not an integration has to say so locally: the daemon cannot
// explain a directory, only a name it does not know.
func TestResolveIntegrationRefRejectsWhatItCannotResolve(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		prepare func(t *testing.T, dir string)
		want    string
	}{
		{
			name: "no manifest in the directory",
			ref:  ".",
			want: "is not an integration",
		},
		{
			name: "missing path",
			ref:  "./nowhere",
			want: "no such file or directory",
		},
		{
			name: "manifest without a name",
			ref:  ".",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, config.ManifestFileName),
					[]byte("version: 1\nentrypoint: main.py\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "does not declare a name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			chdirForTest(t, dir)

			_, err := resolveIntegrationRef(tc.ref)
			if err == nil {
				t.Fatalf("resolveIntegrationRef(%q) succeeded, want an error", tc.ref)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// The line between "a name" and "a path" is what keeps the new spelling from
// shadowing the old one; neither "." nor ".." can be an integration name.
func TestLooksLikePath(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{".", true},
		{"..", true},
		{"./counter", true},
		{"../counter", true},
		{"/srv/integrations/counter", true},
		{"integrations/counter", true},
		{"otter.yaml", true},
		{"", false},
		{"counter", false},
		{"shopify-to-salesforce", false},
		{"erp.sync_v2", false},
	}
	for _, tc := range tests {
		if got := looksLikePath(tc.ref); got != tc.want {
			t.Errorf("looksLikePath(%q) = %t, want %t", tc.ref, got, tc.want)
		}
	}
}

// End to end through the command: `otter run` with a directory, and with no
// argument at all, must submit the manifest's name, not the path.
func TestCmdRunResolvesTheWorkingDirectory(t *testing.T) {
	id := "986d91e8-dde4-45be-b298-c9332c220498"
	var runPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		runPath = r.URL.Path
		fmt.Fprintf(w, `{"run_id":%q,"status":"queued"}`, id)
	}))
	defer server.Close()

	dir := writeIntegration(t, "shopify-to-erp")
	chdirForTest(t, dir)

	for _, args := range [][]string{{"."}, {}} {
		name := strings.Join(args, " ")
		if name == "" {
			name = "(no argument)"
		}
		t.Run(name, func(t *testing.T) {
			runPath = ""
			var out, errOut bytes.Buffer
			app := New("test", &out, &errOut)
			code := app.cmdRun(context.Background(), globals{api: server.URL}, args)
			if code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, errOut.String())
			}
			if want := "/v1/integrations/shopify-to-erp/runs"; runPath != want {
				t.Errorf("submitted to %q, want %q", runPath, want)
			}
			if !strings.Contains(out.String(), id) {
				t.Errorf("stdout %q does not name the queued run", out.String())
			}
		})
	}
}

// Two positionals remain a usage error: the reference is one thing, not a
// command line.
func TestCmdRunRejectsMoreThanOneReference(t *testing.T) {
	var errOut bytes.Buffer
	app := New("test", &errOut, &errOut)
	if code := app.cmdRun(context.Background(), globals{api: "http://127.0.0.1:1"}, []string{"a", "b"}); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage: otter run") {
		t.Errorf("no usage line:\n%s", errOut.String())
	}
}
