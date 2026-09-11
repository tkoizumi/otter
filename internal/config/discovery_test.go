package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeValidIntegration writes a valid manifest plus its entrypoint under
// root/relDir and returns the integration directory.
func writeValidIntegration(t *testing.T, root, relDir, name string) string {
	t.Helper()
	dir := filepath.Join(root, relDir)
	writeManifest(t, dir, "version: 1\nname: "+name+"\nentrypoint: main.py\n")
	touch(t, filepath.Join(dir, "main.py"))
	return dir
}

func TestDiscoverRecursiveAndSortedByID(t *testing.T) {
	root := t.TempDir()
	// Directory order deliberately differs from id order.
	writeValidIntegration(t, root, "zebra", "apple")
	writeValidIntegration(t, root, "a/b/c", "banana")
	writeValidIntegration(t, root, "mango", "cherry")

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Discover() returned %d integrations, want 3", len(got))
	}
	want := []string{"apple", "banana", "cherry"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, id)
		}
		if !got[i].Valid {
			t.Errorf("got[%d] (%s) Valid = false, error = %q", i, id, got[i].Error)
		}
		if got[i].Manifest == nil {
			t.Errorf("got[%d] (%s) Manifest = nil, want parsed manifest", i, id)
		}
	}
	// Results carry the integration directory and manifest path.
	if got[0].ManifestPath != filepath.Join(got[0].Dir, ManifestFileName) {
		t.Errorf("ManifestPath = %q, want %q", got[0].ManifestPath, filepath.Join(got[0].Dir, ManifestFileName))
	}
}

func TestDiscoverUnparseableManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "broken-int")
	writeManifest(t, dir, "name: [unterminated\nversion: 1\n")

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover() returned %d integrations, want 1", len(got))
	}
	it := got[0]
	if it.Valid {
		t.Errorf("Valid = true, want false")
	}
	if it.Error == "" {
		t.Errorf("Error is empty, want a parse failure message")
	}
	if it.Manifest != nil {
		t.Errorf("Manifest = %+v, want nil for an unparseable manifest", it.Manifest)
	}
	if it.ID != "broken-int" {
		t.Errorf("ID = %q, want directory-name fallback %q", it.ID, "broken-int")
	}
}

func TestDiscoverValidationFailureDoesNotPoisonSiblings(t *testing.T) {
	root := t.TempDir()
	writeValidIntegration(t, root, "good", "good-int")

	// Parses fine but fails validation: schema version 2 is unsupported.
	badDir := filepath.Join(root, "bad")
	writeManifest(t, badDir, "version: 2\nname: bad-int\nentrypoint: main.py\n")
	touch(t, filepath.Join(badDir, "main.py"))

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Discover() returned %d integrations, want 2", len(got))
	}

	byID := map[string]*Integration{}
	for _, it := range got {
		byID[it.ID] = it
	}

	good := byID["good-int"]
	if good == nil {
		t.Fatalf("missing good-int in results: %+v", got)
	}
	if !good.Valid {
		t.Errorf("good-int Valid = false (error %q), want true", good.Error)
	}

	bad := byID["bad-int"]
	if bad == nil {
		t.Fatalf("missing bad-int in results: %+v", got)
	}
	if bad.Valid {
		t.Errorf("bad-int Valid = true, want false")
	}
	if !strings.Contains(bad.Error, "version") {
		t.Errorf("bad-int Error = %q, want it to mention version", bad.Error)
	}
	if bad.Manifest == nil {
		t.Errorf("bad-int Manifest = nil, want the parsed manifest retained for diagnostics")
	}
}

func TestDiscoverDuplicateNames(t *testing.T) {
	root := t.TempDir()
	writeValidIntegration(t, root, "aaa", "duplicated")
	writeValidIntegration(t, root, "bbb", "duplicated")

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Discover() returned %d integrations, want 2", len(got))
	}

	var valid, invalid int
	for _, it := range got {
		if it.Valid {
			valid++
			continue
		}
		invalid++
		if !strings.Contains(it.Error, "duplicate") {
			t.Errorf("duplicate entry error = %q, want it to mention %q", it.Error, "duplicate")
		}
	}
	if valid != 1 || invalid != 1 {
		t.Fatalf("valid=%d invalid=%d, want exactly one of each", valid, invalid)
	}

	// The first directory in sorted order keeps the name.
	if got[0].Dir > got[1].Dir {
		t.Fatalf("results not sorted by directory for equal ids: %q, %q", got[0].Dir, got[1].Dir)
	}
	if !got[0].Valid || got[1].Valid {
		t.Errorf("expected the lexicographically first directory to stay valid: got[0]=%v got[1]=%v",
			got[0].Valid, got[1].Valid)
	}
}

func TestDiscoverSkipsNoisyDirectories(t *testing.T) {
	root := t.TempDir()
	skipped := []string{
		"node_modules/x",
		".git/y",
		".venv/z",
		"__pycache__/w",
		"dist/v",
		"build/u",
	}
	for i, rel := range skipped {
		name := "hidden-int-" + string(rune('a'+i))
		writeValidIntegration(t, root, rel, name)
	}

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Discover() returned %d integrations, want 0: %+v", len(got), got)
	}
}

func TestDiscoverErrors(t *testing.T) {
	t.Run("non-existent path", func(t *testing.T) {
		if _, err := Discover(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatalf("Discover() = nil error, want failure for a missing directory")
		}
	})

	t.Run("file instead of directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-a-dir.txt")
		if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if _, err := Discover(path); err == nil {
			t.Fatalf("Discover() = nil error, want failure for a file path")
		}
	})

	t.Run("empty root", func(t *testing.T) {
		if _, err := Discover("   "); err == nil {
			t.Fatalf("Discover() = nil error, want failure for an empty root")
		}
	})
}

func TestDiscoverEmptyDirectory(t *testing.T) {
	got, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Discover() returned %d integrations, want 0", len(got))
	}
}
