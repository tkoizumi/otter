package identity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedIntegration writes a minimal but real integration directory.
func seedIntegration(t *testing.T, root, rel, name string) string {
	t.Helper()
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	manifest := "version: 1\nname: " + name + "\nentrypoint: main.py\n"
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("# entrypoint\n"), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	return dir
}

// observeIntegration describes a directory the way discovery would.
func observeIntegration(t *testing.T, dir, name string) Observation {
	t.Helper()
	canonical, err := Canonical(dir)
	if err != nil {
		t.Fatalf("canonical %s: %v", dir, err)
	}
	state, id := observeMarker(canonical)
	return Observation{
		Path: canonical, Exists: true, ManifestPresent: true, ManifestValid: true,
		Name: name, Marker: state, MarkerID: id,
	}
}

// copyDir copies regular files one level deep. It copies the marker too, which
// is exactly the operation the design must survive.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}
}

func newService(t *testing.T) (*Service, context.Context, string) {
	t.Helper()
	store, ctx := newTestStore(t)
	root := t.TempDir()
	return NewService(store, root), ctx, root
}

func TestServiceRegistersAndPreservesIdentity(t *testing.T) {
	svc, ctx, root := newService(t)
	dir := seedIntegration(t, root, "a", "a")

	res, err := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "a")}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Registered) != 1 {
		t.Fatalf("registered = %v, want one", res.Registered)
	}
	id := res.Registered[0]
	if marker, err := ReadMarker(dir); err != nil || marker != id {
		t.Fatalf("marker = %q, %v; want %q", marker, err, id)
	}

	// A second scan of an unchanged tree preserves the identity.
	res, err = svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "a")}})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(res.Preserved) != 1 || res.Preserved[0] != id {
		t.Fatalf("preserved = %v, want %q", res.Preserved, id)
	}
}

func TestServiceCopyGetsFreshIdentityAndOriginalKeepsItsOwn(t *testing.T) {
	svc, ctx, root := newService(t)
	a := seedIntegration(t, root, "a", "counter")

	res, err := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, a, "counter")}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	original := res.Registered[0]

	b := filepath.Join(root, "b")
	copyDir(t, a, b)

	res, err = svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{
		observeIntegration(t, a, "counter"),
		observeIntegration(t, b, "counter"),
	}})
	if err != nil {
		t.Fatalf("Reconcile after copy: %v", err)
	}
	if len(res.Preserved) != 1 || res.Preserved[0] != original {
		t.Fatalf("original preserved = %v, want %q", res.Preserved, original)
	}
	if len(res.Registered) != 1 {
		t.Fatalf("copy registered = %v, want exactly one fresh identity", res.Registered)
	}
	fresh := res.Registered[0]
	if fresh == original {
		t.Fatalf("the copy adopted the original's identity %q", original)
	}
	// The copy's marker is rewritten so the next scan sees a clean tree.
	if marker, _ := ReadMarker(b); marker != fresh {
		t.Fatalf("copy marker = %q, want %q", marker, fresh)
	}
	if marker, _ := ReadMarker(a); marker != original {
		t.Fatalf("original marker = %q, want %q", marker, original)
	}
}

func TestServiceCopyThenRemoveOriginalLeavesCopyFresh(t *testing.T) {
	svc, ctx, root := newService(t)
	a := seedIntegration(t, root, "a", "counter")
	res, _ := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, a, "counter")}})
	original := res.Registered[0]

	b := filepath.Join(root, "b")
	copyDir(t, a, b)
	if err := os.RemoveAll(a); err != nil {
		t.Fatalf("remove original: %v", err)
	}

	res, err := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{
		observeIntegration(t, b, "counter"),
		{Path: mustCanonical(t, a), Exists: false},
	}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Registered) != 1 || res.Registered[0] == original {
		t.Fatalf("copy registered = %v, want a fresh identity", res.Registered)
	}
	if len(res.Retired) != 1 || res.Retired[0] != original {
		t.Fatalf("retired = %v, want %q", res.Retired, original)
	}
}

func TestServiceReplacementRetiresOldIdentity(t *testing.T) {
	svc, ctx, root := newService(t)
	dir := seedIntegration(t, root, "a", "counter")
	res, _ := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}})
	original := res.Registered[0]

	// Someone replaced the tree: the marker is gone.
	if err := RemoveMarker(dir); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	res, err := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Retired) != 1 || res.Retired[0] != original {
		t.Fatalf("retired = %v, want %q", res.Retired, original)
	}
	if len(res.Replaced) != 1 || res.Replaced[0] == original {
		t.Fatalf("replaced = %v, want a fresh identity", res.Replaced)
	}
	old, err := svc.Store().Instance(ctx, original)
	if err != nil || old.Status != StatusRetired {
		t.Fatalf("old instance = %+v, %v; want retired", old, err)
	}
}

func TestServiceDeleteSuppressesButKeepsSource(t *testing.T) {
	svc, ctx, root := newService(t)
	dir := seedIntegration(t, root, "a", "counter")
	res, _ := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}})
	id := res.Registered[0]

	if err := svc.Delete(ctx, id, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	inst, err := svc.Store().Instance(ctx, id)
	if err != nil || inst.Status != StatusDeleted {
		t.Fatalf("instance = %+v, %v; want deleted", inst, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestFileName)); err != nil {
		t.Fatalf("delete removed source files: %v", err)
	}
	// Deleting again is a no-op.
	if err := svc.Delete(ctx, id, nil); err != nil {
		t.Fatalf("second Delete: %v", err)
	}

	// The source is still there, but discovery must not resurrect it.
	res, err = svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}})
	if err != nil {
		t.Fatalf("Reconcile after delete: %v", err)
	}
	if len(res.Suppressed) != 1 {
		t.Fatalf("suppressed = %v, want the deleted path", res.Suppressed)
	}
	if len(res.Registered) != 0 {
		t.Fatalf("registered = %v, want none", res.Registered)
	}

	// An explicit register clears the suppression and mints a fresh id.
	got, err := svc.Register(ctx, dir, "counter")
	if err != nil {
		t.Fatalf("Register after delete: %v", err)
	}
	if got.ID == id {
		t.Fatalf("register reused the deleted id %q", id)
	}
}

func TestServiceMovePreservesIdentity(t *testing.T) {
	svc, ctx, root := newService(t)
	dir := seedIntegration(t, root, "a", "counter")
	res, _ := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}})
	id := res.Registered[0]

	dest := filepath.Join(root, "moved")
	moved, err := svc.Move(ctx, id, dest)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.ID != id {
		t.Fatalf("move changed the identity: %q -> %q", id, moved.ID)
	}
	canonicalDest := mustCanonical(t, dest)
	if moved.CanonicalPath != canonicalDest {
		t.Fatalf("canonical path = %q, want %q", moved.CanonicalPath, canonicalDest)
	}
	if marker, _ := ReadMarker(dest); marker != id {
		t.Fatalf("marker after move = %q, want %q", marker, id)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source directory still exists after move")
	}
	// A plain scan of the moved tree preserves the identity.
	res, err = svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dest, "counter")}})
	if err != nil {
		t.Fatalf("Reconcile after move: %v", err)
	}
	if len(res.Preserved) != 1 || res.Preserved[0] != id {
		t.Fatalf("preserved = %v, want %q", res.Preserved, id)
	}
}

func TestServiceMoveRejectsOccupiedDestination(t *testing.T) {
	svc, ctx, root := newService(t)
	a := seedIntegration(t, root, "a", "a")
	b := seedIntegration(t, root, "b", "b")
	res, _ := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{
		observeIntegration(t, a, "a"), observeIntegration(t, b, "b"),
	}})
	if len(res.Registered) != 2 {
		t.Fatalf("registered = %v, want both", res.Registered)
	}
	if _, err := svc.Move(ctx, res.Registered[0], b); err == nil {
		t.Fatalf("Move onto an existing destination succeeded, want refusal")
	}
}

func TestServiceResetMintsFreshIdentityAtSamePath(t *testing.T) {
	svc, ctx, root := newService(t)
	dir := seedIntegration(t, root, "a", "counter")
	res, _ := svc.Reconcile(ctx, Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}})
	old := res.Registered[0]

	fresh, err := svc.Reset(ctx, old, "counter")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if fresh.ID == old {
		t.Fatalf("reset kept the old identity")
	}
	if marker, _ := ReadMarker(dir); marker != fresh.ID {
		t.Fatalf("marker after reset = %q, want %q", marker, fresh.ID)
	}
	oldInst, err := svc.Store().Instance(ctx, old)
	if err != nil || oldInst.Status != StatusRetired {
		t.Fatalf("old instance = %+v, %v; want retired", oldInst, err)
	}
}

func TestServiceRecoversHalfFinishedRegistration(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	dir := seedIntegration(t, root, "a", "counter")
	canonical := mustCanonical(t, dir)
	svc := NewService(store, root)

	newID := MustParse("recovered-id")
	payload, err := json.Marshal(registerPayload{
		Name:                "counter",
		NewID:               newID.String(),
		ExpectedMarkerState: string(MarkerNone),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	op := Operation{ID: "op-recover", Kind: OpRegister, Phase: PhasePlanned, InstanceID: newID, ToPath: canonical, Data: payload}
	if err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	res, err := svc.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("recovery errors: %v", res.Errors)
	}
	if marker, _ := ReadMarker(dir); marker != newID {
		t.Fatalf("marker = %q, want %q", marker, newID)
	}
	inst, err := store.Instance(ctx, newID)
	if err != nil || inst.Status != StatusActive {
		t.Fatalf("recovered instance = %+v, %v", inst, err)
	}
	if ops, _ := store.UnfinishedOperations(ctx); len(ops) != 0 {
		t.Fatalf("unfinished operations remain: %+v", ops)
	}

	// Re-running recovery changes nothing: no second identity is minted.
	if _, err := svc.Recover(ctx); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	instances, _ := store.Instances(ctx)
	if len(instances) != 1 {
		t.Fatalf("instances = %+v, want exactly one", instances)
	}
}

func TestServiceRecoveryRefusesChangedMarker(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	dir := seedIntegration(t, root, "a", "counter")
	canonical := mustCanonical(t, dir)
	svc := NewService(store, root)

	newID := MustParse("recovered-id")
	payload, _ := json.Marshal(registerPayload{
		Name:                "counter",
		NewID:               newID.String(),
		ExpectedMarkerState: string(MarkerNone),
	})
	op := Operation{ID: "op-race", Kind: OpRegister, Phase: PhasePlanned, InstanceID: newID, ToPath: canonical, Data: payload}
	if err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	// Something else wrote a marker while the daemon was down.
	if err := WriteMarker(dir, MustParse("someone-else")); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	res, err := svc.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatalf("recovery overwrote an unexpected marker")
	}
	if marker, _ := ReadMarker(dir); marker != MustParse("someone-else") {
		t.Fatalf("marker = %q, want the unexpected value preserved", marker)
	}
	if _, err := store.Instance(ctx, newID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked recovery still registered an instance")
	}
}

func mustCanonical(t *testing.T, path string) string {
	t.Helper()
	canonical, err := Canonical(path)
	if err != nil {
		t.Fatalf("canonical %s: %v", path, err)
	}
	return canonical
}
