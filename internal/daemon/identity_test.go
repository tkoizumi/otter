package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/state"
)

// copyIntegrationFiles copies an integration directory the way `cp -r` does,
// marker included. Copying the marker is the operation the design must treat
// as a new instance.
func copyIntegrationFiles(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
}

func manifestFor(name string) string {
	return "version: 1\nname: " + name + "\nentrypoint: main.py\ntimeout: 30\n"
}

// Two integrations may share a label. The label is a human handle, not a key:
// a bare reference to it is ambiguous and must name every candidate, while an
// identity or a path resolves independently.
func TestTwoIntegrationsMayShareALabel(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "a", manifestFor("shared"), noopPython)
	writeIntegration(t, root, "b", manifestFor("shared"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	views := d.ListIntegrations()
	if len(views) != 2 {
		t.Fatalf("integrations = %+v, want two", views)
	}
	ids := map[string]bool{}
	for _, v := range views {
		if v.Name != "shared" {
			t.Fatalf("label = %q, want shared", v.Name)
		}
		if v.ID == "" || v.ID == "shared" {
			t.Fatalf("id = %q, want a minted identity", v.ID)
		}
		ids[v.ID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("identities = %v, want two distinct ids", ids)
	}

	// A bare label is ambiguous, and the refusal must name the candidates.
	_, err := d.SubmitRun(ctx, "shared", api.TriggerPayload{Type: api.TriggerManual})
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("ambiguous label = %v, want a conflict", err)
	}
	for id := range ids {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("ambiguity error does not name %s: %v", id, err)
		}
	}

	// An explicit identity resolves.
	for id := range ids {
		if _, err := d.SubmitRun(ctx, "id:"+id, api.TriggerPayload{Type: api.TriggerManual}); err != nil {
			t.Errorf("submit by id %s: %v", id, err)
		}
	}
}

// Copying an integration directory creates a separate instance with its own
// state, even though the copy carries the original's marker.
func TestCopyGetsFreshIdentityAndState(t *testing.T) {
	root := t.TempDir()
	a := writeIntegration(t, root, "a", manifestFor("counter"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	aID := runtimeID(t, d, "counter")

	if _, err := d.SetState(ctx, "id:"+aID, "count", json.RawMessage("41")); err != nil {
		t.Fatalf("set state: %v", err)
	}

	b := filepath.Join(root, "b")
	copyIntegrationFiles(t, a, b)
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	bEntry, err := d.resolveRef(b)
	if err != nil {
		t.Fatalf("resolve copy by path: %v", err)
	}
	bID := bEntry.Integration.ID
	if bID == aID {
		t.Fatalf("the copy adopted the original's identity %q", aID)
	}

	// The copy starts empty: a fresh identity has no state.
	if _, err := d.GetState(ctx, "id:"+bID, "count"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("copy state = %v, want not found", err)
	}
	// The original kept its own.
	value, err := d.GetState(ctx, "id:"+aID, "count")
	if err != nil || string(value) != "41" {
		t.Fatalf("original state = %s (%v), want 41", value, err)
	}
}

// Deleting an identity purges what it owns, leaves the source in place, and
// suppresses the path so a scan does not silently re-register it.
func TestDeletePurgesAndSuppressesWithoutRemovingSource(t *testing.T) {
	root := t.TempDir()
	dir := writeIntegration(t, root, "a", manifestFor("counter"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "counter")

	if _, err := d.SetState(ctx, "id:"+id, "count", json.RawMessage("7")); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if _, err := d.DeleteIntegration(ctx, "id:"+id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := d.state.Get(ctx, id, "count"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("state after delete = %v, want not found", err)
	}
	if _, err := os.Stat(filepath.Join(dir, config.ManifestFileName)); err != nil {
		t.Fatalf("delete removed source files: %v", err)
	}

	// The source is still on disk, but a scan reports it as suppressed.
	view, err := d.ident.Store().Instance(ctx, identity.MustParse(id))
	if err != nil || view.Status != identity.StatusDeleted {
		t.Fatalf("instance after delete = %+v (%v), want deleted", view, err)
	}
	rec, found, err := d.ident.Store().PathRecord(ctx, mustCanonicalPath(t, dir))
	if err != nil || !found || !rec.Suppressed {
		t.Fatalf("path record = %+v found=%v (%v), want suppressed", rec, found, err)
	}

	// Reload must not resurrect it.
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, v := range d.ListIntegrations() {
		if v.Path == mustCanonicalPath(t, dir) {
			t.Fatalf("reload re-registered a deleted path: %+v", v)
		}
	}

	// Re-registering explicitly clears the suppression and mints fresh.
	registered, err := d.RegisterIntegration(ctx, dir)
	if err != nil {
		t.Fatalf("register after delete: %v", err)
	}
	if registered.ID == id {
		t.Fatalf("register reused the deleted identity %q", id)
	}
}

// Reset retires the identity and mints a fresh one at the same path, keeping
// the old data for explicit deletion.
func TestResetMintsFreshIdentityAndKeepsOldData(t *testing.T) {
	root := t.TempDir()
	dir := writeIntegration(t, root, "a", manifestFor("counter"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	oldID := runtimeID(t, d, "counter")

	if _, err := d.SetState(ctx, "id:"+oldID, "count", json.RawMessage("5")); err != nil {
		t.Fatalf("set state: %v", err)
	}

	result, err := d.ResetIntegration(ctx, "id:"+oldID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if result.NewID == result.OldID {
		t.Fatalf("reset kept the old identity %q", result.OldID)
	}
	if marker, err := identity.ReadMarker(dir); err != nil || marker.String() != result.NewID {
		t.Fatalf("marker = %q (%v), want %q", marker, err, result.NewID)
	}

	// The fresh identity has no state; the old identity's data survives.
	if _, err := d.GetState(ctx, "id:"+result.NewID, "count"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("fresh identity state = %v, want not found", err)
	}
	value, err := d.state.Get(ctx, result.OldID, "count")
	if err != nil || string(value) != "5" {
		t.Fatalf("old state = %s (%v), want 5", value, err)
	}
}

// Move preserves identity: the same id, the same state, a new path.
func TestMovePreservesIdentity(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "a", manifestFor("counter"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "counter")

	if _, err := d.SetState(ctx, "id:"+id, "count", json.RawMessage("3")); err != nil {
		t.Fatalf("set state: %v", err)
	}

	destination := filepath.Join(root, "moved")
	view, err := d.MoveIntegration(ctx, "id:"+id, destination)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if view.ID != id {
		t.Fatalf("move changed the identity: %q -> %q", id, view.ID)
	}
	if view.Path != mustCanonicalPath(t, destination) {
		t.Fatalf("path = %q, want %q", view.Path, mustCanonicalPath(t, destination))
	}
	value, err := d.GetState(ctx, "id:"+id, "count")
	if err != nil || string(value) != "3" {
		t.Fatalf("state after move = %s (%v), want 3", value, err)
	}
	if _, err := os.Stat(filepath.Join(destination, config.ManifestFileName)); err != nil {
		t.Fatalf("moved tree is missing its manifest: %v", err)
	}
}

// An invalid manifest is still listed, with its real error, but it cannot run.
func TestInvalidManifestIsListedButNotRunnable(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "broken", "version: 1\nname: broken\nentrypoint: main.py\ntrigger:\n  cron: \"not a cron\"\n", noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	view, ok := d.GetIntegration("broken")
	if !ok || view.Valid {
		t.Fatalf("broken integration = %+v ok=%v, want present and invalid", view, ok)
	}
	if _, err := d.SubmitRun(ctx, "broken", api.TriggerPayload{Type: api.TriggerManual}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("submit invalid = %v, want invalid", err)
	}
}

func mustCanonicalPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := identity.Canonical(path)
	if err != nil {
		t.Fatalf("canonical %s: %v", path, err)
	}
	return canonical
}

// A reset retires the identity, so the release staged for it must not be
// silently reused by the fresh one: a new identity runs nothing until it has a
// release of its own.
func TestResetLeavesTheFreshIdentityUnreleased(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "a", manifestFor("counter"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	oldID := runtimeID(t, d, "counter")

	if _, _, ok, err := release.ActiveSourceDir(d.cfg.DataDir, oldID); err != nil || !ok {
		t.Fatalf("the old identity has no active release: ok=%v err=%v", ok, err)
	}

	result, err := d.ResetIntegration(ctx, "id:"+oldID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}

	if _, err := d.SubmitRun(ctx, "id:"+result.NewID, api.TriggerPayload{Type: api.TriggerManual}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("run after reset = %v, want a conflict: the fresh identity has no release", err)
	}
	// The old identity's release is kept, which is what makes its history and
	// a deliberate rollback possible.
	if _, _, ok, err := release.ActiveSourceDir(d.cfg.DataDir, oldID); err != nil || !ok {
		t.Fatalf("reset removed the old identity's release: ok=%v err=%v", ok, err)
	}
}

// A move preserves the identity, and therefore the release the identity owns:
// rollback to an intact snapshot must survive an authorized move.
func TestMoveKeepsTheActiveRelease(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "a", manifestFor("counter"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "counter")

	before, digest, ok, err := release.ActiveSourceDir(d.cfg.DataDir, id)
	if err != nil || !ok {
		t.Fatalf("no active release before the move: ok=%v err=%v", ok, err)
	}

	if _, err := d.MoveIntegration(ctx, "id:"+id, filepath.Join(root, "moved")); err != nil {
		t.Fatalf("move: %v", err)
	}

	after, afterDigest, ok, err := release.ActiveSourceDir(d.cfg.DataDir, id)
	if err != nil || !ok {
		t.Fatalf("release disappeared across a move: ok=%v err=%v", ok, err)
	}
	if after != before || afterDigest != digest {
		t.Fatalf("release changed across a move: %s@%s -> %s@%s", digest, before, afterDigest, after)
	}
}

// Deleting the source first and the data second must work. Once a directory is
// removed its identity retires and leaves the active registry, so the purge
// has to be addressable by id against the registry itself.
func TestDeleteWorksAfterTheSourceDirectoryIsGone(t *testing.T) {
	root := t.TempDir()
	dir := writeIntegration(t, root, "a", manifestFor("counter"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "counter")

	if _, err := d.SetState(ctx, "id:"+id, "count", json.RawMessage("9")); err != nil {
		t.Fatalf("set state: %v", err)
	}

	// Remove the source and let a scan retire the identity.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := d.resolveRef("id:" + id); err == nil {
		t.Fatalf("a retired identity is still resolvable as active")
	}

	deleted, err := d.DeleteIntegration(ctx, "id:"+id)
	if err != nil {
		t.Fatalf("delete a retired identity by id: %v", err)
	}
	if !deleted.Deleted || deleted.ID != id {
		t.Fatalf("deleted view = %+v", deleted)
	}
	if _, err := d.state.Get(ctx, id, "count"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("state survived the purge: %v", err)
	}
	inst, err := d.ident.Store().Instance(ctx, identity.MustParse(id))
	if err != nil || inst.Status != identity.StatusDeleted {
		t.Fatalf("instance = %+v (%v), want deleted", inst, err)
	}
}

// A retired identity has left the active registry, but it is still a legitimate
// administrative target: deleting by its label must work, not just by id.
func TestDeleteResolvesARetiredIdentityByLabel(t *testing.T) {
	root := t.TempDir()
	dir := writeIntegration(t, root, "a", manifestFor("customer-sync"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "customer-sync")

	// The source disappears, so a scan retires the identity.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	deleted, err := d.DeleteIntegration(ctx, "customer-sync")
	if err != nil {
		t.Fatalf("delete a retired identity by label: %v", err)
	}
	if deleted.ID != id || deleted.Name != "customer-sync" {
		t.Fatalf("deleted view = %+v", deleted)
	}
	inst, err := d.ident.Store().Instance(ctx, identity.MustParse(id))
	if err != nil || inst.Status != identity.StatusDeleted {
		t.Fatalf("instance = %+v (%v), want deleted", inst, err)
	}
}
