package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalAPIURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"http://127.0.0.1:7400", "http://127.0.0.1:7400"},
		{"http://127.0.0.1:7400/", "http://127.0.0.1:7400"},
		{"127.0.0.1:7400", "http://127.0.0.1:7400"},
		{"  http://localhost:7337  ", "http://localhost:7337"},
		{"https://otter.example.com", "https://otter.example.com"},
		{"", ""},
		{"   ", ""},
		// A file that is not ours must not become a request to somewhere odd.
		{"ftp://example.com", ""},
		{"file:///etc/passwd", ""},
		{"not a url", ""},
		{"http://", ""},
	}
	for _, tc := range tests {
		if got := canonicalAPIURL(tc.raw); got != tc.want {
			t.Errorf("canonicalAPIURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// A daemon that asks for port 0 is told the port by the kernel. The recorded
// address has to be the resolved one, not the request.
func TestListenAPIURL(t *testing.T) {
	tests := []struct {
		listen string
		want   string
	}{
		{"127.0.0.1:7400", "http://127.0.0.1:7400"},
		{"", "http://127.0.0.1:7337"},
		{":7400", "http://127.0.0.1:7400"},
		{"0.0.0.0:7400", "http://127.0.0.1:7400"},
		{"[::]:7400", "http://127.0.0.1:7400"},
		{"[::1]:7400", "http://[::1]:7400"},
		{"nonsense", ""},
	}
	for _, tc := range tests {
		if got := listenAPIURL(tc.listen); got != tc.want {
			t.Errorf("listenAPIURL(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}

// record writes a listen file the way a daemon does, into a data directory.
func record(t *testing.T, dataDir, base string) {
	t.Helper()
	if err := writeListenURLFile(dataDir, base); err != nil {
		t.Fatal(err)
	}
}

// The project's data directory is the developer's choice via --data, and the
// conventional value is a directory *inside* the project. Discovery must find
// the record at any depth below .otter/, not only directly inside it: assuming
// `.otter/listen.url` meant a daemon recorded into `.otter/data/` was never
// found by the CLI that had just started it.
func TestDiscoverFindsTheRecordAtAnyDepthBelowState(t *testing.T) {
	layouts := map[string]string{
		"directly inside .otter": ".otter",
		"in .otter/data":         filepath.Join(".otter", "data"),
		"in a nested data dir":   filepath.Join(".otter", "data", "nested", "deeper"),
	}
	for name, dataRel := range layouts {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			record(t, filepath.Join(root, dataRel), "http://127.0.0.1:7400")

			got, gotRoot, ok := discoverAPIURL(root)
			if !ok {
				t.Fatalf("nothing discovered from %s (layout: %s)", root, dataRel)
			}
			if got != "http://127.0.0.1:7400" {
				t.Errorf("url = %q, want http://127.0.0.1:7400", got)
			}
			if gotRoot != root {
				t.Errorf("root = %q, want %q", gotRoot, root)
			}
		})
	}
}

func TestDiscoverWalksUpToTheProject(t *testing.T) {
	root := t.TempDir()
	record(t, filepath.Join(root, ".otter", "data"), "http://127.0.0.1:7400")

	nested := filepath.Join(root, "hello", "tests", "deeper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, filepath.Join(root, "hello"), nested} {
		got, _, ok := discoverAPIURL(dir)
		if !ok {
			t.Fatalf("no daemon discovered from %s", dir)
		}
		if got != "http://127.0.0.1:7400" {
			t.Errorf("from %s got %q, want http://127.0.0.1:7400", dir, got)
		}
	}
}

// The nearest project wins, so a developer working in a subproject is not
// silently driven to the outer one.
func TestDiscoverPrefersTheNearestProject(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	record(t, filepath.Join(outer, ".otter", "data"), "http://127.0.0.1:7400")
	record(t, filepath.Join(inner, ".otter", "data"), "http://127.0.0.1:7500")

	got, root, ok := discoverAPIURL(inner)
	if !ok || got != "http://127.0.0.1:7500" {
		t.Errorf("got (%q, %q, %v), want the inner project on 7500", got, root, ok)
	}
}

// A .otter directory with no record in it -- a stopped project, or one whose
// daemon never started -- must not stop the walk. The outer project may be the
// one that is running.
func TestDiscoverKeepsWalkingPastAProjectWithoutARecord(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(filepath.Join(inner, ".otter"), 0o755); err != nil {
		t.Fatal(err)
	}
	record(t, filepath.Join(outer, ".otter", "data"), "http://127.0.0.1:7400")

	if got, _, ok := discoverAPIURL(inner); !ok || got != "http://127.0.0.1:7400" {
		t.Errorf("got (%q, %v), want the outer project's daemon", got, ok)
	}
}

func TestDiscoverWithoutAProject(t *testing.T) {
	if got, root, ok := discoverAPIURL(t.TempDir()); ok {
		t.Errorf("discovered (%q, %q) in an empty directory, want nothing", got, root)
	}
}

func TestDiscoverIgnoresAnEmptyRecord(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".otter", "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ListenURLFileName), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := discoverAPIURL(root); ok {
		t.Errorf("discovered %q from an empty record, want nothing", got)
	}
}

// A prepared environment or extracted SDK can hold hundreds of thousands of
// files. The walk must not descend into them looking for a one-line file.
func TestDiscoverPrunesHeavyStateDirectories(t *testing.T) {
	root := t.TempDir()
	heavy := filepath.Join(root, ".otter", "python", "lib")
	if err := os.MkdirAll(heavy, 0o755); err != nil {
		t.Fatal(err)
	}
	// A record hidden inside the pruned tree must not be found, because the
	// walk never looks there.
	record(t, heavy, "http://127.0.0.1:9999")

	if got, _, ok := discoverAPIURL(root); ok {
		t.Errorf("discovered %q inside a pruned directory, want nothing", got)
	}
}

// --api and OTTER_API_URL both outrank discovery: an operator who pointed every
// command at one daemon must not be redirected by their current directory.
func TestResolveAPIPrecedence(t *testing.T) {
	dir := t.TempDir()
	record(t, filepath.Join(dir, ".otter", "data"), "http://127.0.0.1:7400")
	original := workingDirForTest
	workingDirForTest = func() (string, error) { return dir, nil }
	defer func() { workingDirForTest = original }()

	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("OTTER_API_URL", "http://127.0.0.1:7600")
		g := globals{api: "http://127.0.0.1:7700"}
		resolveAPI(&g)
		if g.api != "http://127.0.0.1:7700" {
			t.Errorf("api = %q, want the flag value", g.api)
		}
	})

	t.Run("environment beats the project", func(t *testing.T) {
		t.Setenv("OTTER_API_URL", "http://127.0.0.1:7600")
		g := globals{}
		resolveAPI(&g)
		if g.api != "http://127.0.0.1:7600" {
			t.Errorf("api = %q, want the environment value", g.api)
		}
	})

	t.Run("project beats the default", func(t *testing.T) {
		t.Setenv("OTTER_API_URL", "")
		g := globals{}
		resolveAPI(&g)
		if g.api != "http://127.0.0.1:7400" {
			t.Errorf("api = %q, want the discovered address", g.api)
		}
	})

	t.Run("empty stays default", func(t *testing.T) {
		t.Setenv("OTTER_API_URL", "")
		original := workingDirForTest
		workingDirForTest = func() (string, error) { return t.TempDir(), nil }
		defer func() { workingDirForTest = original }()
		g := globals{}
		resolveAPI(&g)
		if g.api != "" {
			t.Errorf("api = %q, want empty so the client uses its default", g.api)
		}
	})
}

func TestRemoveListenURLFile(t *testing.T) {
	dataDir := t.TempDir()
	record(t, dataDir, "http://127.0.0.1:7400")
	removeListenURLFile(dataDir)
	if _, err := os.Stat(filepath.Join(dataDir, ListenURLFileName)); !os.IsNotExist(err) {
		t.Errorf("the record survived removal: %v", err)
	}
	// Removing it twice, or when it was never written, is not an error.
	removeListenURLFile(dataDir)
	removeListenURLFile("")
}

// The help text is the only place a developer learns the precedence, so it has
// to mention discovery rather than implying --api is the only way.
func TestUsageDocumentsDiscovery(t *testing.T) {
	var out strings.Builder
	app := New("test", &out, &out)
	app.printUsage(&out)
	usage := out.String()
	if !strings.Contains(usage, "walking up from the working directory") {
		t.Errorf("usage does not explain where the address comes from:\n%s", usage)
	}
}
