package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a checkout-shaped tree: <root>/integrations/<name> plus a
// shared <root>/lib, which is the layout the release package mirrors.
type fixture struct {
	root     string
	data     string
	name     string
	source   string
	shared   string
	manifest string
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
	return fixture{root: root, data: data, name: name, source: source, shared: shared, manifest: manifest}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) manager() Manager    { return Manager{DataDir: f.data} }
func (f fixture) trees() []SharedTree { return []SharedTree{{Source: f.shared, Name: "lib"}} }
func (f fixture) stage(t *testing.T, env string) Metadata {
	t.Helper()
	meta, err := f.manager().StageWithShared(f.name, f.source, f.trees(), env)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	return meta
}

func TestStageMirrorsTheLayout(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")

	dir, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// The mirror is the whole point: the manifest's ../../lib/python must
	// resolve inside the release exactly as it does in the checkout.
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
	resolved := filepath.Join(SourceDir(dir, f.name), "..", "..", "lib")
	if got, err := filepath.EvalSymlinks(resolved); err == nil {
		if want, _ := filepath.EvalSymlinks(filepath.Join(dir, "lib")); got != want {
			t.Errorf("../../lib resolves to %s, want %s", got, want)
		}
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

	// A different landing name for the same shared tree changes every relative
	// import, so it must be a different release.
	renamed, err := f.manager().StageWithShared(f.name, f.source,
		[]SharedTree{{Source: f.shared, Name: "vendor"}}, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Digest == base {
		t.Error("renaming the shared tree did not produce a new release")
	}
}

func TestStageSkipsSecretsAndCaches(t *testing.T) {
	f := newFixture(t, "one")
	write(t, filepath.Join(f.source, ".env"), "SECRET=hunter2\n")
	write(t, filepath.Join(f.source, "otter.db"), "not a database")
	if err := os.MkdirAll(filepath.Join(f.source, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.source, "__pycache__", "main.cpython-313.pyc"), "cache")
	// The pulled schema is editor tooling and runs to megabytes; the query
	// document beside it is read while the integration runs.
	for _, dir := range []string{filepath.Join("schema", "shopify"), "queries"} {
		if err := os.MkdirAll(filepath.Join(f.source, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(f.source, "schema", "shopify", "shopify.graphql"), "type Product { id: ID }\n")
	write(t, filepath.Join(f.source, "queries", "products.graphql"), "query { products { nodes { id } } }\n")

	meta := f.stage(t, "env-1")
	dir, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	released := SourceDir(dir, f.name)
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

func TestSafeSharedNameRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"..", "../escape", "/absolute", "a/../..", ""} {
		if _, err := safeJoin(root, name); err == nil {
			// An empty name is rejected by the callers' validation, not here,
			// so only assert on the escaping forms.
			if name != "" {
				t.Errorf("safeJoin accepted %q", name)
			}
		}
	}
	if got, err := safeJoin(root, "lib/python"); err != nil {
		t.Errorf("safeJoin rejected a valid nested name: %v", err)
	} else if !strings.HasPrefix(got, root) {
		t.Errorf("safeJoin escaped the root: %s", got)
	}
}

func TestStageRequiresAManifest(t *testing.T) {
	f := newFixture(t, "one")
	if err := os.Remove(filepath.Join(f.source, "otter.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager().StageWithShared(f.name, f.source, f.trees(), "env"); err == nil {
		t.Fatal("staged a directory with no manifest")
	}
}

func TestListReportsTheActiveRelease(t *testing.T) {
	f := newFixture(t, "one")
	manager := f.manager()
	first := f.stage(t, "env-1")
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
	_ = first
}
