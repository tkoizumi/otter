package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fixture builds a checkout-shaped tree: <root>/integrations/<name> plus a
// shared <root>/lib, which is the canonical layout the release package mirrors.
type fixture struct {
	root   string
	data   string
	name   string
	source string
	shared string
}

func newFixture(t *testing.T, name string) fixture {
	t.Helper()
	root, data := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "integrations", name)
	shared := filepath.Join(root, "lib")
	for _, dir := range []string{source, shared} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "version: 1\nname: " + name + "\nentrypoint: main.py\npython:\n  mode: managed\n  path:\n    - ../../lib\n"
	write(t, filepath.Join(source, "otter.yaml"), manifest)
	write(t, filepath.Join(source, "main.py"), "print('hello')\n")
	write(t, filepath.Join(shared, "shared_lib.py"), "VALUE = 1\n")
	return fixture{root: root, data: data, name: name, source: source, shared: shared}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) manager() Manager    { return Manager{DataDir: f.data} }
func (f fixture) trees() []SharedTree { return []SharedTree{{Source: f.shared}} }

func (f fixture) layout(t *testing.T) Layout {
	t.Helper()
	layout, err := Plan(f.root, f.source, f.trees())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return layout
}

func (f fixture) stage(t *testing.T, env string) Metadata {
	t.Helper()
	meta, err := f.manager().StageWithLayout(f.name, f.source, f.layout(t), env)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	return meta
}

// released resolves the integration directory inside a staged release through
// the metadata, which is the only supported way to read the placement back.
func (f fixture) released(t *testing.T, meta Metadata) string {
	t.Helper()
	dir, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	src, err := meta.SourceDir(dir)
	if err != nil {
		t.Fatalf("resolve release source: %v", err)
	}
	return src
}

func TestStageMirrorsTheLayout(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")

	dir, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if meta.IntegrationPath != "integrations/one" {
		t.Errorf("IntegrationPath = %q, want integrations/one", meta.IntegrationPath)
	}
	// The mirror is the whole point: the manifest's ../../lib must resolve
	// inside the release exactly as it does in the checkout.
	for _, want := range []string{
		filepath.Join(dir, "integrations", f.name, "main.py"),
		filepath.Join(dir, "integrations", f.name, "otter.yaml"),
		filepath.Join(dir, "lib", "shared_lib.py"),
		filepath.Join(dir, ManifestFileName),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("release is missing %s: %v", want, err)
		}
	}

	// The relative path the manifest declares must resolve from the snapshot.
	resolved := filepath.Join(f.released(t, meta), "..", "..", "lib")
	if got, err := filepath.EvalSymlinks(resolved); err == nil {
		if want, _ := filepath.EvalSymlinks(filepath.Join(dir, "lib")); got != want {
			t.Errorf("../../lib resolves to %s, want %s", got, want)
		}
	} else {
		t.Errorf("../../lib does not resolve inside the release: %v", err)
	}
}

func TestStageIsIdempotent(t *testing.T) {
	f := newFixture(t, "one")
	first := f.stage(t, "env-1")
	second := f.stage(t, "env-1")

	if first.Digest != second.Digest {
		t.Errorf("identical inputs produced different digests: %s != %s", first.Digest, second.Digest)
	}
	releases, err := f.manager().List(f.name)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 {
		t.Errorf("staged %d releases for identical inputs, want 1", len(releases))
	}
}

// The placement takes part in the digest: the same files arranged at a
// different depth change every relative import, so they are a different release.
func TestDigestIsPlacementSensitive(t *testing.T) {
	f := newFixture(t, "one")
	canonical, err := f.manager().StageWithLayout(f.name, f.source,
		Layout{IntegrationPath: "integrations/one", Trees: []SharedTree{{Source: f.shared, Name: "lib"}}}, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	flat, err := f.manager().StageWithLayout(f.name, f.source,
		Layout{IntegrationPath: "one", Trees: []SharedTree{{Source: f.shared, Name: "lib"}}}, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Digest == flat.Digest {
		t.Error("moving the integration changed no digest, so a queued run could execute the wrong snapshot")
	}

	renamed, err := f.manager().StageWithLayout(f.name, f.source,
		Layout{IntegrationPath: "integrations/one", Trees: []SharedTree{{Source: f.shared, Name: "vendor"}}}, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Digest == canonical.Digest {
		t.Error("renaming the shared tree did not produce a new release")
	}
}

func TestDigestCoversEveryInput(t *testing.T) {
	f := newFixture(t, "one")
	base := f.stage(t, "env-1").Digest

	// A source change.
	write(t, filepath.Join(f.source, "main.py"), "print('changed')\n")
	sourceChanged := f.stage(t, "env-1").Digest
	if sourceChanged == base {
		t.Error("a source change did not produce a new release")
	}

	// A shared-code change.
	write(t, filepath.Join(f.source, "main.py"), "print('hello')\n")
	write(t, filepath.Join(f.shared, "shared_lib.py"), "VALUE = 2\n")
	sharedChanged := f.stage(t, "env-1").Digest
	if sharedChanged == sourceChanged || sharedChanged == base {
		t.Error("a shared-code change did not produce a new release")
	}

	// An environment change.
	write(t, filepath.Join(f.shared, "shared_lib.py"), "VALUE = 1\n")
	envChanged := f.stage(t, "env-2").Digest
	if envChanged == base {
		t.Error("an environment change did not produce a new release")
	}
}

func TestStageSkipsSecretsAndCaches(t *testing.T) {
	f := newFixture(t, "one")
	write(t, filepath.Join(f.source, ".env"), "SECRET=hunter2\n")
	write(t, filepath.Join(f.source, "otter.db"), "not a database")
	write(t, filepath.Join(f.source, "__pycache__", "main.cpython-313.pyc"), "cache")
	// The pulled schema is editor tooling and runs to megabytes; the query
	// document beside it is read while the integration runs.
	write(t, filepath.Join(f.source, "schema", "shopify", "shopify.graphql"), "type Product { id: ID }\n")
	write(t, filepath.Join(f.source, "queries", "products.graphql"), "query { products { nodes { id } } }\n")

	meta := f.stage(t, "env-1")
	released := f.released(t, meta)
	for _, forbidden := range []string{
		".env", "otter.db", "__pycache__", "schema/shopify/shopify.graphql",
	} {
		if _, err := os.Stat(filepath.Join(released, filepath.FromSlash(forbidden))); err == nil {
			t.Errorf("release captured %s; it must never become immutable", forbidden)
		}
	}
	if _, err := os.Stat(filepath.Join(released, "queries", "products.graphql")); err != nil {
		t.Errorf("release dropped the query document the integration reads: %v", err)
	}
}

func TestActivateIsAtomicAndResolvable(t *testing.T) {
	f := newFixture(t, "one")
	first := f.stage(t, "env-1")
	manager := f.manager()

	if _, ok, err := manager.Active(f.name); err != nil || ok {
		t.Fatalf("integration reported an active release before activation (ok=%v err=%v)", ok, err)
	}

	if err := manager.Activate(f.name, first.Digest); err != nil {
		t.Fatalf("activate: %v", err)
	}
	active, ok, err := manager.Active(f.name)
	if err != nil || !ok {
		t.Fatalf("no active release after activation (ok=%v err=%v)", ok, err)
	}
	if active.Digest != first.Digest {
		t.Errorf("active digest = %s, want %s", active.Digest, first.Digest)
	}

	// A switch must replace the link, never leave it missing.
	write(t, filepath.Join(f.source, "main.py"), "print('second')\n")
	second := f.stage(t, "env-1")
	if err := manager.Activate(f.name, second.Digest); err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	active, _, err = manager.Active(f.name)
	if err != nil {
		t.Fatalf("resolve after switch: %v", err)
	}
	if active.Digest != second.Digest {
		t.Errorf("active digest = %s, want the newest %s", active.Digest, second.Digest)
	}

	// The previous release must still exist: a running attempt may be using it.
	if _, err := manager.Dir(f.name, first.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Metadata(f.name, first.Digest); err != nil {
		t.Errorf("the previous release was removed by activation: %v", err)
	}
}

func TestActivateRefusesAnIncompleteRelease(t *testing.T) {
	f := newFixture(t, "one")
	manager := f.manager()
	digest := strings.Repeat("a", 64)
	dir, err := manager.Dir(f.name, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(f.name, digest); err == nil {
		t.Fatal("activated a release with no metadata")
	}
	if _, ok, _ := manager.Active(f.name); ok {
		t.Error("a refused activation still became active")
	}
}

func TestRetainKeepsTheActiveAndReferenced(t *testing.T) {
	f := newFixture(t, "one")
	var digests []string
	for i := 0; i < 5; i++ {
		write(t, filepath.Join(f.source, "main.py"), "print('v"+string(rune('a'+i))+"')\n")
		digests = append(digests, f.stage(t, "env-1").Digest)
	}
	manager := f.manager()
	active := digests[len(digests)-1]
	if err := manager.Activate(f.name, active); err != nil {
		t.Fatal(err)
	}

	// The oldest release is still referenced by a queued run, so retention must
	// protect it even though it is far outside the retained window.
	referenced := digests[0]
	removed, err := manager.Retain(f.name, 1, map[string]bool{referenced: true})
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("retention removed nothing, so it proves nothing")
	}
	if _, err := manager.Metadata(f.name, active); err != nil {
		t.Errorf("retention removed the active release: %v", err)
	}
	if _, err := manager.Metadata(f.name, referenced); err != nil {
		t.Errorf("retention removed a referenced release: %v", err)
	}
	for _, digest := range removed {
		if digest == active || digest == referenced {
			t.Errorf("retention removed a protected release %s", digest[:12])
		}
	}

	// Every other inactive release is gone, so the bound is real.
	remaining, err := manager.List(f.name)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Errorf("kept %d releases, want the active, the referenced and one to roll back to", len(remaining))
	}
}

// Retention must never remove the release a rollback would need, even when the
// window is as small as possible.
func TestRetainAlwaysKeepsOneInactiveRelease(t *testing.T) {
	f := newFixture(t, "one")
	first := f.stage(t, "env-1")
	write(t, filepath.Join(f.source, "main.py"), "print('two')\n")
	second := f.stage(t, "env-1")
	manager := f.manager()
	if err := manager.Activate(f.name, second.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Retain(f.name, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Metadata(f.name, first.Digest); err != nil {
		t.Errorf("retention removed the only rollback target: %v", err)
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"..", "../escape", "/absolute", "a/../.."} {
		if _, err := safeJoin(root, name); err == nil {
			t.Errorf("safeJoin accepted %q", name)
		}
	}
	if got, err := safeJoin(root, "lib/python"); err != nil {
		t.Errorf("safeJoin rejected a valid nested name: %v", err)
	} else if !strings.HasPrefix(got, root) {
		t.Errorf("safeJoin escaped the root: %s", got)
	}
	// The root itself is a legal placement: an integration at the release root.
	if got, err := safeJoin(root, "."); err != nil || got != filepath.Clean(root) {
		t.Errorf("safeJoin(root, \".\") = %q, %v; want %q", got, err, root)
	}
}

func TestStageRequiresAManifest(t *testing.T) {
	f := newFixture(t, "one")
	if err := os.Remove(filepath.Join(f.source, "otter.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager().StageWithLayout(f.name, f.source, f.layout(t), "env"); err == nil {
		t.Fatal("staged a directory with no manifest")
	}
}

func TestListReportsTheActiveRelease(t *testing.T) {
	f := newFixture(t, "one")
	manager := f.manager()
	f.stage(t, "env-1")
	write(t, filepath.Join(f.source, "main.py"), "print('two')\n")
	second := f.stage(t, "env-1")
	if err := manager.Activate(f.name, second.Digest); err != nil {
		t.Fatal(err)
	}

	releases, err := manager.List(f.name)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 2 {
		t.Fatalf("listed %d releases, want 2", len(releases))
	}
	for _, rel := range releases {
		wantActive := rel.Digest == second.Digest
		if rel.Active != wantActive {
			t.Errorf("release %s active=%v, want %v", rel.Digest[:12], rel.Active, wantActive)
		}
	}
	if releases[0].Digest != second.Digest {
		t.Error("releases are not ordered newest first")
	}
}

// --- placement rule ---------------------------------------------------------

// TestPlanLayouts pins the one rule against every shape a workspace can have:
// the integration and the shared tree each land relative to the same base.
func TestPlanLayouts(t *testing.T) {
	tests := []struct {
		name        string
		root        string
		integration string
		tree        string
		wantInteg   string
		wantTree    string
	}{
		{
			name:        "canonical integrations/<name>",
			root:        "/repo",
			integration: "/repo/integrations/foo",
			tree:        "/repo/lib/python",
			wantInteg:   "integrations/foo",
			wantTree:    "lib/python",
		},
		{
			name:        "flat workspace root",
			root:        "/repo",
			integration: "/repo/foo",
			tree:        "/repo/lib/python",
			wantInteg:   "foo",
			wantTree:    "lib/python",
		},
		{
			name:        "grouped integration",
			root:        "/repo",
			integration: "/repo/group/foo",
			tree:        "/repo/group/lib/python",
			wantInteg:   "group/foo",
			wantTree:    "group/lib/python",
		},
		{
			// Deploy: the discovery root is <remote>/integrations while the
			// shared library is a sibling of it.
			name:        "deploy discovery root",
			root:        "/opt/otter/integrations",
			integration: "/opt/otter/integrations/foo",
			tree:        "/opt/otter/lib/python",
			wantInteg:   "integrations/foo",
			wantTree:    "lib/python",
		},
		{
			// --source outside the discovery root but inside the same base.
			name:        "source outside the discovery root",
			root:        "/repo/integrations",
			integration: "/repo/other/foo",
			tree:        "/repo/lib",
			wantInteg:   "other/foo",
			wantTree:    "lib",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			layout, err := Plan(tc.root, tc.integration, []SharedTree{{Source: tc.tree}})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if layout.IntegrationPath != tc.wantInteg {
				t.Errorf("integration placed at %q, want %q", layout.IntegrationPath, tc.wantInteg)
			}
			if len(layout.Trees) != 1 || layout.Trees[0].Name != tc.wantTree {
				t.Errorf("trees placed at %+v, want one at %q", layout.Trees, tc.wantTree)
			}
		})
	}
}

func TestPlanDropsTreesAlreadyCarried(t *testing.T) {
	// A tree inside the integration travels with the integration's own copy.
	layout, err := Plan("/repo", "/repo/integrations/foo", []SharedTree{
		{Source: "/repo/integrations/foo/vendor"},
		{Source: "/repo/lib"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Trees) != 1 || layout.Trees[0].Name != "lib" {
		t.Fatalf("trees = %+v, want only lib", layout.Trees)
	}

	// A tree nested inside another captured tree is already carried.
	layout, err = Plan("/repo", "/repo/integrations/foo", []SharedTree{
		{Source: "/repo/lib"},
		{Source: "/repo/lib/python/connectors"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Trees) != 1 || layout.Trees[0].Name != "lib" {
		t.Fatalf("trees = %+v, want only the outer lib", layout.Trees)
	}
}

func TestPlanCollectsMultipleSharedTrees(t *testing.T) {
	layout, err := Plan("/repo", "/repo/integrations/foo", []SharedTree{
		{Source: "/repo/lib/python"},
		{Source: "/repo/vendor/sdk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Trees) != 2 {
		t.Fatalf("captured %d trees, want 2", len(layout.Trees))
	}
	want := map[string]bool{"lib/python": true, "vendor/sdk": true}
	for _, tree := range layout.Trees {
		if !want[tree.Name] {
			t.Errorf("unexpected tree placement %q", tree.Name)
		}
	}
}

func TestPlanRefusesUnrepresentablePlacements(t *testing.T) {
	// No common ancestor below the filesystem root: the tree lives on a
	// different branch entirely.
	if _, err := Plan("/repo/integrations", "/repo/integrations/foo", []SharedTree{{Source: "/elsewhere/lib"}}); err == nil {
		t.Error("planned a release whose shared tree has no relative placement")
	}
	// A tree containing the integration cannot be copied into the release
	// without placing the release inside itself.
	if _, err := Plan("/repo", "/repo/integrations/foo", []SharedTree{{Source: "/repo/integrations"}}); err == nil {
		t.Error("planned a release whose shared tree contains the integration")
	}
}

// --- resolver and legacy metadata -------------------------------------------

func TestLegacyMetadataDefaultsPlacement(t *testing.T) {
	f := newFixture(t, "one")
	legacy := Metadata{Integration: f.name, Digest: strings.Repeat("b", 64)}
	rel, err := legacy.IntegrationRel()
	if err != nil {
		t.Fatal(err)
	}
	if rel != "integrations/one" {
		t.Errorf("legacy placement = %q, want integrations/one", rel)
	}

	dir, err := f.manager().Dir(f.name, legacy.Digest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "integrations", "one", "main.py"), "print('legacy')\n")
	src, err := legacy.SourceDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "main.py")); err != nil {
		t.Errorf("legacy metadata did not resolve to its snapshot: %v", err)
	}
}

func TestInvalidIntegrationPathRejected(t *testing.T) {
	root := t.TempDir()
	for _, raw := range []string{"../escape", "a/../../b", "/absolute", ".."} {
		meta := Metadata{Integration: "one", Digest: strings.Repeat("c", 64), IntegrationPath: raw}
		if _, err := meta.SourceDir(root); err == nil {
			t.Errorf("IntegrationPath %q was accepted", raw)
		}
	}
	// An absolute path dressed up as relative must not sneak through safeJoin.
	meta := Metadata{Integration: "one", Digest: strings.Repeat("c", 64), IntegrationPath: "a/../../../etc"}
	if _, err := meta.SourceDir(root); err == nil {
		t.Error("an escaping IntegrationPath was accepted")
	}
}

// A snapshot named by the previous layout generation's digest must never be
// reused, even though the inputs are identical: its internal placement is
// different from what this implementation would produce.
func TestOldDigestSnapshotsAreNotReused(t *testing.T) {
	f := newFixture(t, "one")
	v1, err := digestV1(f.source, []SharedTree{{Source: f.shared, Name: "lib"}}, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	manager := f.manager()
	old, err := manager.Dir(f.name, v1)
	if err != nil {
		t.Fatal(err)
	}
	// The metadata the old implementation wrote: no integration_path, and the
	// snapshot at the hardcoded integrations/<name>.
	write(t, filepath.Join(old, "integrations", f.name, "main.py"), "print('old')\n")
	if err := writeMetadata(old, Metadata{
		Integration: f.name,
		Digest:      v1,
		Environment: "env-1",
		Source:      f.source,
	}); err != nil {
		t.Fatal(err)
	}

	meta := f.stage(t, "env-1")
	if meta.Digest == v1 {
		t.Fatal("the new implementation reused an old-layout digest")
	}
	releases, err := manager.List(f.name)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 2 {
		t.Fatalf("staged %d releases, want the old one plus a freshly laid out one", len(releases))
	}
}

// digestV1 reproduces the hashing of the previous implementation, so the test
// can prove a snapshot it named is not reused.
func digestV1(integrationDir string, shared []SharedTree, environmentDigest string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "otter-release-v1\x00%s\x00%s\x00", filepath.Base(integrationDir), environmentDigest)
	if err := hashTree(h, integrationDir, integrationDir); err != nil {
		return "", err
	}
	sorted := append([]SharedTree(nil), shared...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, tree := range sorted {
		fmt.Fprintf(h, "shared\x00%s\x00", filepath.ToSlash(tree.Name))
		if err := hashTree(h, tree.Source, tree.Source); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// --- shared-tree policy -----------------------------------------------------

func TestMissingSharedTreeIsAnError(t *testing.T) {
	f := newFixture(t, "one")
	if err := os.RemoveAll(f.shared); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager().StageWithLayout(f.name, f.source, f.layout(t), "env-1"); err == nil {
		t.Fatal("staging silently skipped a declared shared tree that is missing")
	} else if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error does not explain the missing tree: %v", err)
	}
}

// An integration at the workspace root would have the data directory inside the
// tree it captures. That copy would contain the staging directory itself, so it
// is refused instead of recursing until the disk fills.
func TestStageRefusesADataDirectoryInsideTheIntegration(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "otter.yaml"), "version: 1\nname: one\nentrypoint: main.py\n")
	write(t, filepath.Join(root, "main.py"), "print('ok')\n")

	manager := Manager{DataDir: filepath.Join(root, ".otter", "data")}
	layout, err := Plan(root, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if layout.IntegrationPath != "." {
		t.Fatalf("placement = %q, want the release root", layout.IntegrationPath)
	}
	if _, err := manager.StageWithLayout("one", root, layout, ""); err == nil {
		t.Fatal("staged a release inside its own data directory")
	}
}

func TestEscapingSymlinkIsRejected(t *testing.T) {
	f := newFixture(t, "one")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.py"), "SECRET = 'hunter2'\n")
	if err := os.Symlink(filepath.Join(outside, "secret.py"), filepath.Join(f.source, "link.py")); err != nil {
		t.Fatal(err)
	}
	_, err := f.manager().StageWithLayout(f.name, f.source, f.layout(t), "env-1")
	if err == nil {
		t.Fatal("a symlink that resolves outside the release was captured")
	}
	if !strings.Contains(err.Error(), "outside the release") {
		t.Errorf("error does not explain the escaping link: %v", err)
	}
}

func TestAbsoluteSymlinkIsRejected(t *testing.T) {
	f := newFixture(t, "one")
	write(t, filepath.Join(f.source, "real.py"), "X = 1\n")
	if err := os.Symlink(filepath.Join(f.source, "real.py"), filepath.Join(f.source, "link.py")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager().StageWithLayout(f.name, f.source, f.layout(t), "env-1"); err == nil {
		t.Fatal("an absolute symlink was captured; it cannot resolve inside a release on another host")
	}
}

func TestInternalSymlinksArePreserved(t *testing.T) {
	f := newFixture(t, "one")
	write(t, filepath.Join(f.source, "real.py"), "X = 1\n")
	// Within the integration.
	if err := os.Symlink("real.py", filepath.Join(f.source, "alias.py")); err != nil {
		t.Fatal(err)
	}
	// Into a captured shared tree.
	if err := os.Symlink(filepath.Join("..", "..", "lib", "shared_lib.py"), filepath.Join(f.source, "shared_alias.py")); err != nil {
		t.Fatal(err)
	}
	meta := f.stage(t, "env-1")
	released := f.released(t, meta)

	info, err := os.Lstat(filepath.Join(released, "alias.py"))
	if err != nil {
		t.Fatalf("internal symlink was dropped: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("internal symlink was replaced by a regular file")
	}
	if target, _ := os.Readlink(filepath.Join(released, "alias.py")); target != "real.py" {
		t.Errorf("symlink target = %q, want real.py", target)
	}
	if _, err := os.Lstat(filepath.Join(released, "shared_alias.py")); err != nil {
		t.Errorf("symlink into a captured shared tree was dropped: %v", err)
	}
}
