package identity

import (
	"context"
	"errors"
	"testing"
)

func TestBootstrapLegacyAssignsAnUnambiguousKey(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	dir := seedIntegration(t, root, "counter", "counter")
	svc := NewService(store, root)

	scan := Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}}
	result, err := svc.BootstrapLegacy(ctx, scan, []string{"counter"}, nil, false)
	if err != nil {
		t.Fatalf("BootstrapLegacy: %v", err)
	}
	if len(result.Assigned) != 1 || result.Assigned[0].ID != MustParse("counter") {
		t.Fatalf("assigned = %+v, want counter", result.Assigned)
	}

	// The legacy key becomes the identity, so no state has to move.
	inst, err := store.Instance(ctx, MustParse("counter"))
	if err != nil || inst.Status != StatusActive {
		t.Fatalf("instance = %+v (%v), want active", inst, err)
	}
	if marker, err := ReadMarker(dir); err != nil || marker != MustParse("counter") {
		t.Fatalf("marker = %q (%v), want counter", marker, err)
	}
}

func TestBootstrapLegacyReservesAnOrphanKey(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	svc := NewService(store, root)

	result, err := svc.BootstrapLegacy(ctx, Scan{Complete: true}, []string{"ghost"}, nil, false)
	if err != nil {
		t.Fatalf("BootstrapLegacy: %v", err)
	}
	if len(result.Reserved) != 1 || result.Reserved[0] != MustParse("ghost") {
		t.Fatalf("reserved = %+v, want ghost", result.Reserved)
	}
	inst, err := store.Instance(ctx, MustParse("ghost"))
	if err != nil || inst.Status != StatusRetired {
		t.Fatalf("instance = %+v (%v), want retired", inst, err)
	}
}

func TestBootstrapLegacyCollisionNeedsAnOwner(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	a := seedIntegration(t, root, "a", "shared")
	b := seedIntegration(t, root, "b", "shared")
	svc := NewService(store, root)

	scan := Scan{Complete: true, Observations: []Observation{
		observeIntegration(t, a, "shared"),
		observeIntegration(t, b, "shared"),
	}}

	_, err := svc.BootstrapLegacy(ctx, scan, []string{"shared"}, nil, false)
	var conflict *BootstrapConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("collision = %v, want a BootstrapConflictError", err)
	}
	if len(conflict.Conflicts) != 1 || conflict.Conflicts[0].Name != "shared" || len(conflict.Conflicts[0].Paths) != 2 {
		t.Fatalf("conflict = %+v, want both paths named", conflict.Conflicts)
	}
	// Nothing was assigned while the collision is unresolved.
	if _, err := store.Instance(ctx, MustParse("shared")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a conflicting bootstrap wrote a registry row")
	}

	result, err := svc.BootstrapLegacy(ctx, scan, []string{"shared"}, map[string]string{"shared": b}, false)
	if err != nil {
		t.Fatalf("BootstrapLegacy with an assignment: %v", err)
	}
	if len(result.Assigned) != 1 || result.Assigned[0].Path != mustCanonicalPath(t, b) {
		t.Fatalf("assigned = %+v, want the chosen directory", result.Assigned)
	}

	// The losing directory is left to register fresh rather than steal the
	// legacy state.
	minted := 0
	mint := func() (ID, error) {
		minted++
		return MustParse("fresh"), nil
	}
	plan, err := BuildPlan(scan, mustInstances(t, store, ctx), mustPaths(t, store, ctx), mint)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if minted == 0 {
		t.Fatalf("no fresh identity was needed, but a losing directory remains: %+v", plan.Actions)
	}
	var fresh bool
	for _, action := range plan.Actions {
		if action.Path == mustCanonicalPath(t, a) && action.Kind == ActionRegister && action.NewID == MustParse("fresh") {
			fresh = true
		}
	}
	if !fresh {
		t.Fatalf("the losing directory did not get a fresh identity: %+v", plan.Actions)
	}
}

func TestBootstrapLegacyDryRunWritesNothing(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	dir := seedIntegration(t, root, "counter", "counter")
	svc := NewService(store, root)

	scan := Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}}
	result, err := svc.BootstrapLegacy(ctx, scan, []string{"counter"}, nil, true)
	if err != nil {
		t.Fatalf("BootstrapLegacy dry run: %v", err)
	}
	if len(result.Assigned) != 1 {
		t.Fatalf("plan = %+v, want the assignment reported", result)
	}
	if _, err := store.Instance(ctx, MustParse("counter")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a dry run wrote a registry row")
	}
	if MarkerExists(dir) {
		t.Fatalf("a dry run wrote a marker")
	}
}

func TestBootstrapLegacyIsIdempotent(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	dir := seedIntegration(t, root, "counter", "counter")
	svc := NewService(store, root)
	scan := Scan{Complete: true, Observations: []Observation{observeIntegration(t, dir, "counter")}}

	if _, err := svc.BootstrapLegacy(ctx, scan, []string{"counter"}, nil, false); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	result, err := svc.BootstrapLegacy(ctx, scan, []string{"counter"}, nil, false)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if len(result.Assigned) != 0 || len(result.Reserved) != 0 {
		t.Fatalf("second bootstrap did work: %+v", result)
	}
	instances, _ := store.Instances(ctx)
	if len(instances) != 1 {
		t.Fatalf("instances = %+v, want one", instances)
	}
}

func mustInstances(t *testing.T, store *Store, ctx context.Context) []Instance {
	t.Helper()
	instances, err := store.Instances(ctx)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	return instances
}

func mustPaths(t *testing.T, store *Store, ctx context.Context) []PathRecord {
	t.Helper()
	paths, err := store.Paths(ctx)
	if err != nil {
		t.Fatalf("list paths: %v", err)
	}
	return paths
}

func mustCanonicalPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := Canonical(path)
	if err != nil {
		t.Fatalf("canonical %s: %v", path, err)
	}
	return canonical
}

// A collision must leave the registry exactly as it was. A bootstrap that
// assigned the keys it could and then refused the rest would strand the
// remaining keys' state outside the registry, and a later start would see a
// non-empty registry and consider itself finished.
func TestBootstrapLegacyCollisionAppliesNothing(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	a := seedIntegration(t, root, "a", "shared")
	b := seedIntegration(t, root, "b", "shared")
	solo := seedIntegration(t, root, "c", "solo")
	svc := NewService(store, root)

	scan := Scan{Complete: true, Observations: []Observation{
		observeIntegration(t, a, "shared"),
		observeIntegration(t, b, "shared"),
		observeIntegration(t, solo, "solo"),
	}}

	// "shared" sorts before "solo", so the old order would have assigned the
	// unambiguous key before discovering the collision.
	_, err := svc.BootstrapLegacy(ctx, scan, []string{"shared", "solo"}, nil, false)
	var conflict *BootstrapConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("collision = %v, want a BootstrapConflictError", err)
	}

	instances, err := store.Instances(ctx)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("a refused bootstrap mutated the registry: %+v", instances)
	}
	for _, dir := range []string{a, b, solo} {
		if MarkerExists(dir) {
			t.Fatalf("a refused bootstrap wrote a marker in %s", dir)
		}
	}
}

// Bootstrap is resumable: keys already assigned are left alone and the rest
// are still processed, which is what lets a daemon recover a half-applied
// registry rather than declaring itself done.
func TestBootstrapLegacyResumesAPartiallyAppliedRegistry(t *testing.T) {
	store, ctx := newTestStore(t)
	root := t.TempDir()
	solo := seedIntegration(t, root, "c", "solo")
	other := seedIntegration(t, root, "d", "other")
	svc := NewService(store, root)

	// A previous run assigned "solo" and stopped before "other".
	if err := svc.assignLegacy(ctx, MustParse("solo"), "solo", observeIntegration(t, solo, "solo")); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}

	scan := Scan{Complete: true, Observations: []Observation{
		observeIntegration(t, solo, "solo"),
		observeIntegration(t, other, "other"),
	}}
	result, err := svc.BootstrapLegacy(ctx, scan, []string{"solo", "other"}, nil, false)
	if err != nil {
		t.Fatalf("BootstrapLegacy: %v", err)
	}
	if len(result.Assigned) != 1 || result.Assigned[0].ID != MustParse("other") {
		t.Fatalf("assigned = %+v, want only the pending key", result.Assigned)
	}
	if _, err := store.Instance(ctx, MustParse("other")); err != nil {
		t.Fatalf("the pending key was not registered: %v", err)
	}
	instances, _ := store.Instances(ctx)
	if len(instances) != 2 {
		t.Fatalf("instances = %+v, want two", instances)
	}
}
