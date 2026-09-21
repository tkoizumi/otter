package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseAcceptsUUIDsAndLegacyNames(t *testing.T) {
	for _, raw := range []string{
		"0195a7c2-8e31-7b64-9f02-6dcb482ea510",
		"counter",
		"shopify-to-erp",
		"erp.sync_v2",
		"a",
	} {
		if _, err := Parse(raw); err != nil {
			t.Errorf("Parse(%q) = %v, want success", raw, err)
		}
	}
}

func TestParseRejectsUnsafeValues(t *testing.T) {
	for _, raw := range []string{
		"",
		".",
		"..",
		"a/b",
		`a\b`,
		"a b",
		"a\nb",
		"../escape",
		"nul\x00byte",
		string(make([]byte, MaxIDLength+1)),
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", raw)
		}
	}
}

func TestMarkerBodyRoundTrip(t *testing.T) {
	id := MustParse("0195a7c2-8e31-7b64-9f02-6dcb482ea510")
	got, err := ParseMarkerBody(MarkerBody(id))
	if err != nil {
		t.Fatalf("ParseMarkerBody: %v", err)
	}
	if got != id {
		t.Fatalf("round trip = %q, want %q", got, id)
	}
}

func TestParseMarkerBodyRejectsExtraLinesAndWhitespace(t *testing.T) {
	for _, body := range []string{
		"counter\n\n",
		"counter\nextra\n",
		" counter\n",
		"counter \n",
		"counter\r\n",
	} {
		if _, err := ParseMarkerBody([]byte(body)); err == nil {
			t.Errorf("ParseMarkerBody(%q) succeeded, want an error", body)
		}
	}
}

func TestMarkerWriteReadRemove(t *testing.T) {
	dir := t.TempDir()
	id := MustParse("counter")

	if _, err := ReadMarker(dir); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("ReadMarker on an unmarked directory = %v, want ErrNoMarker", err)
	}
	if err := WriteMarker(dir, id); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	info, err := os.Stat(MarkerPath(dir))
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("marker mode = %o, want 600", perm)
	}
	got, err := ReadMarker(dir)
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if got != id {
		t.Fatalf("ReadMarker = %q, want %q", got, id)
	}
	if err := WriteMarker(dir, MustParse("other")); err != nil {
		t.Fatalf("WriteMarker overwrite: %v", err)
	}
	if got, _ := ReadMarker(dir); got != MustParse("other") {
		t.Fatalf("overwrite = %q, want other", got)
	}
	if err := RemoveMarker(dir); err != nil {
		t.Fatalf("RemoveMarker: %v", err)
	}
	if MarkerExists(dir) {
		t.Fatalf("marker still exists after RemoveMarker")
	}
	// Removing an absent marker is fine.
	if err := RemoveMarker(dir); err != nil {
		t.Fatalf("RemoveMarker on absent marker: %v", err)
	}
}

func TestMarkerSymlinkIsRefused(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("symlinks are not portable here")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real-marker")
	if err := os.WriteFile(target, MarkerBody(MustParse("counter")), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, MarkerPath(dir)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := ReadMarker(dir); !errors.Is(err, ErrUnsafeMarker) {
		t.Fatalf("ReadMarker through a symlink = %v, want ErrUnsafeMarker", err)
	}
	if err := WriteMarker(dir, MustParse("other")); !errors.Is(err, ErrUnsafeMarker) {
		t.Fatalf("WriteMarker over a symlink = %v, want ErrUnsafeMarker", err)
	}
	if err := RemoveMarker(dir); !errors.Is(err, ErrUnsafeMarker) {
		t.Fatalf("RemoveMarker over a symlink = %v, want ErrUnsafeMarker", err)
	}
}

// ---------------------------------------------------------------- planner

func seqMint(ids ...ID) func() (ID, error) {
	i := 0
	return func() (ID, error) {
		if i >= len(ids) {
			return "", fmt.Errorf("mint exhausted")
		}
		id := ids[i]
		i++
		return id, nil
	}
}

func active(id, path, name string) Instance {
	return Instance{ID: MustParse(id), Name: name, CanonicalPath: path, Status: StatusActive, Generation: 1}
}

func ownedPath(id, path string) PathRecord {
	return PathRecord{CanonicalPath: path, OwnerID: MustParse(id)}
}

func actionKinds(plan Plan) []ActionKind {
	out := make([]ActionKind, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		out = append(out, a.Kind)
	}
	return out
}

func TestPlanRegistersNewPath(t *testing.T) {
	scan := Scan{Complete: true, Observations: []Observation{{
		Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a", Marker: MarkerNone,
	}}}
	plan, err := BuildPlan(scan, nil, nil, seqMint("fresh"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionRegister {
		t.Fatalf("actions = %v, want one register", actionKinds(plan))
	}
	if plan.Actions[0].NewID != MustParse("fresh") {
		t.Fatalf("new id = %q, want fresh", plan.Actions[0].NewID)
	}
}

func TestPlanPreservesMatchingMarker(t *testing.T) {
	scan := Scan{Complete: true, Observations: []Observation{{
		Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true,
		Name: "renamed", Marker: MarkerValid, MarkerID: MustParse("A"),
	}}}
	plan, err := BuildPlan(scan, []Instance{active("A", "/root/a", "a")}, []PathRecord{ownedPath("A", "/root/a")}, seqMint())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionPreserve {
		t.Fatalf("actions = %v, want one preserve", actionKinds(plan))
	}
	if plan.Actions[0].Name != "renamed" {
		t.Fatalf("label = %q, want the new label to be recorded", plan.Actions[0].Name)
	}
}

func TestPlanReplacesMissingMarker(t *testing.T) {
	scan := Scan{Complete: true, Observations: []Observation{{
		Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a", Marker: MarkerNone,
	}}}
	plan, err := BuildPlan(scan, []Instance{active("A", "/root/a", "a")}, []PathRecord{ownedPath("A", "/root/a")}, seqMint("B"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	got := actionKinds(plan)
	if len(got) != 1 || got[0] != ActionReplace {
		t.Fatalf("actions = %v, want a single replace", got)
	}
	if plan.Actions[0].NewID != MustParse("B") {
		t.Fatalf("replacement id = %q, want B", plan.Actions[0].NewID)
	}
	if plan.Actions[0].Owner != MustParse("A") {
		t.Fatalf("replacement owner = %q, want A to be retired by the replacement", plan.Actions[0].Owner)
	}
}

func TestPlanNeverAdoptsCopiedMarker(t *testing.T) {
	// B carries A's marker. A is the registered owner of its own path; B is a
	// new path. B must get a fresh identity, never A's.
	scan := Scan{Complete: true, Observations: []Observation{
		{Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a", Marker: MarkerValid, MarkerID: MustParse("A")},
		{Path: "/root/b", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a", Marker: MarkerValid, MarkerID: MustParse("A")},
	}}
	plan, err := BuildPlan(scan, []Instance{active("A", "/root/a", "a")}, []PathRecord{ownedPath("A", "/root/a")}, seqMint("B"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	byPath := map[string]Action{}
	for _, a := range plan.Actions {
		byPath[a.Path] = a
	}
	if byPath["/root/a"].Kind != ActionPreserve {
		t.Fatalf("path a action = %v, want preserve", byPath["/root/a"].Kind)
	}
	if byPath["/root/b"].Kind != ActionRegister || byPath["/root/b"].NewID != MustParse("B") {
		t.Fatalf("path b action = %+v, want register with a fresh id", byPath["/root/b"])
	}
}

func TestPlanDoesNotReviveRetiredIdentity(t *testing.T) {
	retired := active("A", "/root/a", "a")
	retired.Status = StatusRetired
	scan := Scan{Complete: true, Observations: []Observation{{
		Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true,
		Name: "a", Marker: MarkerValid, MarkerID: MustParse("A"),
	}}}
	// The path row survives with no owner; the instance is retired.
	plan, err := BuildPlan(scan, []Instance{retired}, []PathRecord{{CanonicalPath: "/root/a"}}, seqMint("B"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionRegister || plan.Actions[0].NewID != MustParse("B") {
		t.Fatalf("actions = %+v, want a fresh register", plan.Actions)
	}
}

func TestPlanSuppressedPathIsReportedNotRegistered(t *testing.T) {
	scan := Scan{Complete: true, Observations: []Observation{{
		Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a", Marker: MarkerNone,
	}}}
	rec := PathRecord{CanonicalPath: "/root/a", Suppressed: true, SuppressionReason: "deleted by operator"}
	plan, err := BuildPlan(scan, nil, []PathRecord{rec}, seqMint("fresh"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionSuppressed {
		t.Fatalf("actions = %v, want one suppressed report", actionKinds(plan))
	}
}

func TestPlanRetiresGoneDirectory(t *testing.T) {
	scan := Scan{Complete: true, Observations: []Observation{{Path: "/root/a", Exists: false}}}
	plan, err := BuildPlan(scan, []Instance{active("A", "/root/a", "a")}, []PathRecord{ownedPath("A", "/root/a")}, seqMint())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionRetire || plan.Actions[0].Owner != MustParse("A") {
		t.Fatalf("actions = %+v, want retire A", plan.Actions)
	}
}

func TestPlanBlocksInvalidManifestButKeepsIdentity(t *testing.T) {
	for _, obs := range []Observation{
		{Path: "/root/a", Exists: true, ManifestPresent: false, Marker: MarkerValid, MarkerID: MustParse("A")},
		{Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: false, ManifestError: "name is required", Marker: MarkerValid, MarkerID: MustParse("A")},
	} {
		plan, err := BuildPlan(Scan{Complete: true, Observations: []Observation{obs}},
			[]Instance{active("A", "/root/a", "a")}, []PathRecord{ownedPath("A", "/root/a")}, seqMint())
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionBlock || plan.Actions[0].Owner != MustParse("A") {
			t.Fatalf("actions = %+v, want block A", plan.Actions)
		}
	}
}

func TestPlanBlocksUnsafeMarker(t *testing.T) {
	scan := Scan{Complete: true, Observations: []Observation{{
		Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a",
		Marker: MarkerUnsafe, MarkerError: "symlink",
	}}}
	plan, err := BuildPlan(scan, []Instance{active("A", "/root/a", "a")}, []PathRecord{ownedPath("A", "/root/a")}, seqMint())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionBlock {
		t.Fatalf("actions = %v, want block", actionKinds(plan))
	}
}

func TestPlanIncompleteScanChangesNothing(t *testing.T) {
	scan := Scan{
		Complete: false,
		Errors:   []string{"/root/a: permission denied"},
		Observations: []Observation{
			{Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a", Marker: MarkerNone},
		},
	}
	plan, err := BuildPlan(scan, []Instance{active("A", "/root/a", "a")}, []PathRecord{ownedPath("A", "/root/a")}, seqMint("fresh"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.HasChanges() {
		t.Fatalf("incomplete scan produced changes: %+v", plan.Actions)
	}
	for _, a := range plan.Actions {
		if a.Kind != ActionBlock {
			t.Fatalf("incomplete scan produced %v, want blocks only", a.Kind)
		}
	}
}

func TestPlanIsDeterministicAcrossObservationOrder(t *testing.T) {
	a := Observation{Path: "/root/a", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "a", Marker: MarkerNone}
	b := Observation{Path: "/root/b", Exists: true, ManifestPresent: true, ManifestValid: true, Name: "b", Marker: MarkerNone}

	first, err := BuildPlan(Scan{Complete: true, Observations: []Observation{a, b}}, nil, nil, seqMint("one", "two"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	second, err := BuildPlan(Scan{Complete: true, Observations: []Observation{b, a}}, nil, nil, seqMint("one", "two"))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(first.Actions) != len(second.Actions) {
		t.Fatalf("different action counts: %d vs %d", len(first.Actions), len(second.Actions))
	}
	for i := range first.Actions {
		if first.Actions[i] != second.Actions[i] {
			t.Fatalf("action %d differs by observation order: %+v vs %+v", i, first.Actions[i], second.Actions[i])
		}
	}
}

func TestCanonicalPathHelpers(t *testing.T) {
	if Within("/srv/otter", "/srv/otter-2") {
		t.Errorf("Within treated a sibling prefix as containment")
	}
	if !Within("/srv/otter", "/srv/otter/a/b") {
		t.Errorf("Within missed a real descendant")
	}
	if !Within("/srv/otter", "/srv/otter") {
		t.Errorf("Within missed the directory itself")
	}
	if !IsNested("/srv/otter", "/srv/otter/a") || IsNested("/srv/otter", "/srv/otter") {
		t.Errorf("IsNested misclassified")
	}
}

func TestStoreInstanceLifecycle(t *testing.T) {
	store, ctx := newTestStore(t)
	inst := active("A", "/root/a", "a")
	inst.CreatedAt = time.Now().UTC()
	if err := store.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	got, err := store.Instance(ctx, MustParse("A"))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if got.Name != "a" || got.Status != StatusActive || got.Generation != 1 {
		t.Fatalf("instance = %+v", got)
	}
	// A second owner for the same path is refused.
	if err := store.CreateInstance(ctx, active("B", "/root/a", "b")); !errors.Is(err, ErrPathOwned) {
		t.Fatalf("second owner for a path = %v, want ErrPathOwned", err)
	}
	gen, err := store.BumpGeneration(ctx, MustParse("A"))
	if err != nil || gen != 2 {
		t.Fatalf("BumpGeneration = %d, %v; want 2", gen, err)
	}
	if err := store.SetStatus(ctx, MustParse("A"), StatusRetired, "gone"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, _ = store.Instance(ctx, MustParse("A"))
	if got.Status != StatusRetired || got.RetiredAt == nil {
		t.Fatalf("retired instance = %+v", got)
	}
}

func TestStoreBootstrapFlag(t *testing.T) {
	store, ctx := newTestStore(t)
	done, err := store.BootstrapComplete(ctx)
	if err != nil || done {
		t.Fatalf("BootstrapComplete = %v, %v; want false", done, err)
	}
	if err := store.SetBootstrapComplete(ctx, true); err != nil {
		t.Fatalf("SetBootstrapComplete: %v", err)
	}
	if done, _ := store.BootstrapComplete(ctx); !done {
		t.Fatalf("bootstrap flag did not persist")
	}
}

func TestStoreOperationJournal(t *testing.T) {
	store, ctx := newTestStore(t)
	op := Operation{ID: "op-1", Kind: OpReset, Phase: PhasePlanned, InstanceID: MustParse("A"), FromPath: "/root/a"}
	if err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	ops, err := store.UnfinishedOperations(ctx)
	if err != nil || len(ops) != 1 {
		t.Fatalf("UnfinishedOperations = %+v, %v; want one", ops, err)
	}
	op.Phase = PhaseDone
	if err := store.UpdateOperation(ctx, op); err != nil {
		t.Fatalf("UpdateOperation: %v", err)
	}
	ops, _ = store.UnfinishedOperations(ctx)
	if len(ops) != 0 {
		t.Fatalf("finished operation still listed: %+v", ops)
	}
}
