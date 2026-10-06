package config

// These tests validate the WP0 qualification fixtures with the runtime's own
// manifest loader.
//
// The fixtures live in the sibling `otter-platform` repository, because they are
// platform qualification inputs rather than runtime tests. That repository has
// no Go module, so the check cannot live beside them; putting it here is what
// lets it use the real loader instead of a copy of its rules. A fixture the
// runtime would refuse is worse than no fixture -- qualification would fail on a
// schema typo and read as a sandbox incompatibility.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixturesRoot locates the sibling repository's fixture directory. A missing
// tree skips rather than fails, because the runtime must be testable without
// its platform sibling checked out beside it; every other failure is loud.
func fixturesRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file to find the fixtures")
	}
	// internal/config -> repository root -> sibling checkout
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "otter-platform", "hosting", "fixtures")
	if _, err := os.Stat(filepath.Join(root, "profiles.json")); err != nil {
		t.Skipf("qualification fixtures are not checked out at %s", root)
	}
	return root
}

// fixtureDirs returns every fixture directory holding a manifest.
func fixtureDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), ManifestFileName)); err == nil {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no fixtures found: the qualification has nothing to run")
	}
	return dirs
}

// Every fixture must load and validate exactly as a deployed job would. This is
// the check that keeps a manifest typo from being mistaken for a sandbox
// failure during qualification.
func TestQualificationFixturesLoadAndValidate(t *testing.T) {
	root := fixturesRoot(t)

	for _, dir := range fixtureDirs(t, root) {
		t.Run(dir, func(t *testing.T) {
			path := filepath.Join(root, dir, ManifestFileName)
			m, err := LoadAndValidate(path)
			if err != nil {
				// Class C is managed, so it needs a resolved uv.lock, which
				// only exists once the dependency tree has been resolved
				// against a real index. That is a step in setting the
				// qualification up (WP1), not a fixture defect -- so it is
				// reported as a skip with the command that fixes it, rather
				// than passing silently or blocking everything else.
				if strings.Contains(err.Error(), "requires uv.lock") {
					t.Skipf("class C is not yet locked; run 'uv lock' in %s before qualification", dir)
				}
				t.Fatalf("the runtime would refuse this fixture: %v", err)
			}
			// The entrypoint must exist beside the manifest, or the fixture
			// fails at release time rather than at qualification time.
			if _, err := os.Stat(filepath.Join(root, dir, m.Entrypoint)); err != nil {
				t.Errorf("entrypoint %q: %v", m.Entrypoint, err)
			}
		})
	}
}

// The classes are named in the qualification contract and in profiles.json. A
// fixture that quietly disappeared would leave a class unqualified without
// anything failing, so the set is asserted rather than discovered.
func TestQualificationClassSetIsComplete(t *testing.T) {
	root := fixturesRoot(t)

	want := map[string]string{
		"order-sync":            "class A: paginated API I/O",
		"catalog-reconcile":     "class B: memory-heavy transformation",
		"hubspot-netsuite-sync": "class C: native dependencies and retry",
		"subprocess-batch":      "the subprocess shape under class A's profile",
	}
	found := map[string]bool{}
	for _, dir := range fixtureDirs(t, root) {
		found[dir] = true
	}
	for name, why := range want {
		if !found[name] {
			t.Errorf("fixture %q is missing (%s); the qualification contract names it", name, why)
		}
	}
}

// Class C is the only fixture whose point is a real dependency tree, so it is
// the only one that must be managed. The others run on the host interpreter
// deliberately: their classes are about I/O and memory, and dragging a prepared
// environment into them would confound the measurement.
func TestOnlyTheNativeDependencyClassIsManaged(t *testing.T) {
	root := fixturesRoot(t)

	for _, dir := range fixtureDirs(t, root) {
		m, err := LoadAndValidate(filepath.Join(root, dir, ManifestFileName))
		if err != nil {
			if strings.Contains(err.Error(), "requires uv.lock") {
				t.Skipf("class C is not yet locked; run 'uv lock' in %s", dir)
			}
			t.Fatalf("%s: %v", dir, err)
		}
		managed := m.Python.Mode == "managed"
		if dir == "hubspot-netsuite-sync" && !managed {
			t.Error("class C must be managed: preparation is the property under test")
		}
		if dir != "hubspot-netsuite-sync" && managed {
			t.Errorf("%s is managed; only class C should be, or its measurement is confounded", dir)
		}
	}
}

// The profiles file is machine-readable input to the harness, so it must stay
// valid JSON and keep naming fixtures that exist.
func TestQualificationProfilesNameRealFixtures(t *testing.T) {
	root := fixturesRoot(t)

	body, err := os.ReadFile(filepath.Join(root, "profiles.json"))
	if err != nil {
		t.Fatalf("read profiles.json: %v", err)
	}
	// The file carries prose in $comment fields, so it is validated as JSON
	// rather than pinned to a Go struct that would make the contract harder to
	// annotate than the code it describes.
	if !json.Valid(body) {
		t.Fatal("profiles.json is not valid JSON")
	}
	for _, name := range []string{"order-sync", "catalog-reconcile", "hubspot-netsuite-sync", "subprocess-batch"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("profiles.json does not mention %q", name)
		}
	}
}
