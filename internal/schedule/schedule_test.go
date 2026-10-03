package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// newTestStore opens and migrates a throwaway SQLite database and returns a
// Store bound to it plus the handle, so a test can reopen a second Store over
// the same file (the restart path).
func newTestStore(t *testing.T) (*Store, *database.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("database.Migrate() error = %v", err)
	}
	store, err := NewStore(ctx, db.DB)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store, db, ctx
}

func mustCreate(t *testing.T, s *Store, ctx context.Context, in CreateInput) Schedule {
	t.Helper()
	rec, _, err := s.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create(%+v) error = %v", in, err)
	}
	return rec
}

// A retried create with the same idempotency key returns the existing schedule
// rather than minting a second one.
func TestCreateIsIdempotentByKey(t *testing.T) {
	s, _, ctx := newTestStore(t)

	first := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 15m", IdempotencyKey: "key-1"})
	second, changed, err := s.Create(ctx, CreateInput{JobID: "job-1", Cron: "@every 15m", IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("duplicate Create() error = %v", err)
	}
	if changed {
		t.Error("a duplicate create reported changed = true")
	}
	if second.ID != first.ID {
		t.Errorf("duplicate create made a new row: %s != %s", second.ID, first.ID)
	}
	if got := len(s.ForJob("job-1")); got != 1 {
		t.Errorf("schedules for job-1 = %d, want 1", got)
	}

	// A different key is a deliberate second schedule and must be allowed.
	third, _, err := s.Create(ctx, CreateInput{JobID: "job-1", Cron: "@every 30m", IdempotencyKey: "key-2"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
	if third.ID == first.ID {
		t.Error("a second key reused the first schedule")
	}
	if got := len(s.ForJob("job-1")); got != 2 {
		t.Errorf("schedules for job-1 = %d, want 2", got)
	}
}

// The API may not change or delete a row the manifest owns.
func TestUpdateAndDeleteRefuseManifestOwned(t *testing.T) {
	s, _, ctx := newTestStore(t)
	rec := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 15m", Origin: OriginManifest, OriginRef: "trigger.cron"})

	cron := "@every 30m"
	if _, _, err := s.Update(ctx, rec.ID, UpdateInput{Cron: &cron}); !errors.Is(err, ErrManifestOwned) {
		t.Errorf("Update on a manifest row = %v, want ErrManifestOwned", err)
	}
	if err := s.Delete(ctx, rec.ID); !errors.Is(err, ErrManifestOwned) {
		t.Errorf("Delete on a manifest row = %v, want ErrManifestOwned", err)
	}
	// Pausing is an operator control and is allowed on a manifest row; reload
	// preserves it, so the two owners do not fight.
	if _, _, err := s.SetPaused(ctx, rec.ID, true); err != nil {
		t.Errorf("SetPaused on a manifest row = %v, want nil", err)
	}
}

// Reconciliation changes only manifest-owned rows, and preserves the operator
// state (pause, last fire) it does not own.
func TestReconcileManifestTouchesOnlyManifestRows(t *testing.T) {
	s, _, ctx := newTestStore(t)

	manifest := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 15m", Origin: OriginManifest, OriginRef: "trigger.cron"})
	apiRow := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 7m"})
	if _, _, err := s.SetPaused(ctx, manifest.ID, true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Change the manifest's expression; the paused flag and the API row stay.
	result, err := s.ReconcileManifest(ctx, "job-1", []ManifestSpec{{Ref: "trigger.cron", Cron: "@every 1h"}})
	if err != nil {
		t.Fatalf("ReconcileManifest() error = %v", err)
	}
	if result.Updated != 1 || result.Added != 0 || result.Removed != 0 {
		t.Fatalf("reconcile result = %+v, want one update", result)
	}
	got, _ := s.Get(manifest.ID)
	if got.Cron != "@every 1h" {
		t.Errorf("manifest cron = %q, want @every 1h", got.Cron)
	}
	if !got.Paused() {
		t.Error("reconciliation dropped the operator's pause")
	}
	if other, _ := s.Get(apiRow.ID); other.Cron != "@every 7m" || other.Origin != OriginAPI {
		t.Errorf("reconciliation touched the API row: %+v", other)
	}

	// Removing the manifest entry deletes only its row.
	result, err = s.ReconcileManifest(ctx, "job-1", nil)
	if err != nil {
		t.Fatalf("ReconcileManifest(nil) error = %v", err)
	}
	if result.Removed != 1 {
		t.Fatalf("reconcile result = %+v, want one removal", result)
	}
	if _, ok := s.Get(manifest.ID); ok {
		t.Error("the removed manifest row survived")
	}
	if _, ok := s.Get(apiRow.ID); !ok {
		t.Error("reconciliation removed the API row")
	}
}

// A stable Ref keeps a row across a reordered list; without one, position is
// the weaker identity and the row's identity follows the position.
func TestReconcileManifestMatchesByRef(t *testing.T) {
	s, _, ctx := newTestStore(t)
	a := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 1h", Origin: OriginManifest, OriginRef: "a"})
	b := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 2h", Origin: OriginManifest, OriginRef: "b"})

	if _, err := s.ReconcileManifest(ctx, "job-1", []ManifestSpec{
		{Ref: "b", Cron: "@every 2h"},
		{Ref: "a", Cron: "@every 1h"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(s.ForJob("job-1")) != 2 {
		t.Fatalf("reordering churned rows: %d", len(s.ForJob("job-1")))
	}
	if got, _ := s.Get(a.ID); got.Cron != "@every 1h" {
		t.Errorf("row a changed to %q", got.Cron)
	}
	if got, _ := s.Get(b.ID); got.Cron != "@every 2h" {
		t.Errorf("row b changed to %q", got.Cron)
	}
}

// The occurrence ledger is the exactly-once mechanism: the second write of the
// same occurrence loses and reports false.
func TestRecordFireDedupsOneOccurrence(t *testing.T) {
	s, db, ctx := newTestStore(t)
	rec := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 15m"})
	occurrence := time.Now().UTC().Truncate(time.Minute)

	record := func(runID string) bool {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		ok, err := s.RecordFireTx(ctx, tx, rec.ID, occurrence, runID)
		if err != nil {
			t.Fatalf("RecordFireTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return ok
	}

	if !record("run-1") {
		t.Fatal("the first occurrence was not recorded")
	}
	if record("run-2") {
		t.Fatal("a duplicate occurrence was recorded twice")
	}
	if found, err := s.FireRecorded(ctx, rec.ID, occurrence); err != nil || !found {
		t.Fatalf("FireRecorded = %v, %v; want true", found, err)
	}

	// A rollback must leave no ledger row behind.
	other := occurrence.Add(time.Minute)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.RecordFireTx(ctx, tx, rec.ID, other, "run-3"); err != nil {
		t.Fatalf("RecordFireTx: %v", err)
	}
	_ = tx.Rollback()
	if found, err := s.FireRecorded(ctx, rec.ID, other); err != nil || found {
		t.Fatalf("a rolled-back fire left a ledger row: found=%v err=%v", found, err)
	}
}

// The legacy single cadence keeps the v0.3.0 contract: clearing is sticky, and
// the row still declares legacy ownership so reconciliation leaves it alone.
func TestLegacyCadenceKeepsClearSticky(t *testing.T) {
	s, _, ctx := newTestStore(t)

	rec, changed, err := s.SetLegacyCadence(ctx, "job-1", "@every 15m")
	if err != nil || !changed {
		t.Fatalf("SetLegacyCadence = %v, changed=%v", err, changed)
	}
	if !s.LegacyCadenceForJob("job-1") {
		t.Error("a legacy cadence did not report legacy ownership")
	}

	cleared, changed, err := s.SetLegacyCadence(ctx, "job-1", "")
	if err != nil || !changed {
		t.Fatalf("clear = %v, changed=%v", err, changed)
	}
	if cleared.ID != rec.ID {
		t.Error("clearing replaced the row instead of clearing it")
	}
	if !s.LegacyCadenceForJob("job-1") {
		t.Error("clearing dropped legacy ownership, so a reload could reseed the manifest")
	}
}

// Deleting a job purges its schedule rows and their ledger.
func TestDeleteForJobPurgesFires(t *testing.T) {
	s, db, ctx := newTestStore(t)
	rec := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 15m"})

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.RecordFireTx(ctx, tx, rec.ID, time.Now().UTC(), "run-1"); err != nil {
		t.Fatalf("RecordFireTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := s.DeleteForJob(ctx, "job-1"); err != nil {
		t.Fatalf("DeleteForJob: %v", err)
	}
	if len(s.ForJob("job-1")) != 0 {
		t.Error("schedules survived a job purge")
	}
	var fires int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schedule_fires`).Scan(&fires); err != nil {
		t.Fatalf("count fires: %v", err)
	}
	if fires != 0 {
		t.Errorf("ledger rows after purge = %d, want 0", fires)
	}
}

// A payload must be a JSON object within the size bound, and a time zone must
// resolve; both are rejected before a row exists.
func TestCreateValidatesInput(t *testing.T) {
	s, _, ctx := newTestStore(t)

	cases := []struct {
		name string
		in   CreateInput
	}{
		{"bad cron", CreateInput{JobID: "job-1", Cron: "not a cron"}},
		{"bad timezone", CreateInput{JobID: "job-1", Cron: "@every 1h", Timezone: "Mars/Olympus"}},
		{"payload not an object", CreateInput{JobID: "job-1", Cron: "@every 1h", Payload: json.RawMessage(`[1,2]`)}},
		{"payload invalid json", CreateInput{JobID: "job-1", Cron: "@every 1h", Payload: json.RawMessage(`{`)}},
		{"unknown policy", CreateInput{JobID: "job-1", Cron: "@every 1h", MissedPolicy: "whenever"}},
		{"no job", CreateInput{Cron: "@every 1h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Create(ctx, tc.in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Create(%+v) = %v, want ErrInvalid", tc.in, err)
			}
		})
	}

	rec := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 1h", Payload: json.RawMessage(`{"dataset":42}`), Timezone: "Europe/London"})
	if string(rec.Payload) != `{"dataset":42}` || rec.Timezone != "Europe/London" {
		t.Errorf("stored schedule = %+v", rec)
	}
}

// A second Store over the same file sees what the first wrote, which is the
// restart path.
func TestStoreReloadsFromDisk(t *testing.T) {
	s, db, ctx := newTestStore(t)
	rec := mustCreate(t, s, ctx, CreateInput{JobID: "job-1", Cron: "@every 15m", IdempotencyKey: "key-1"})

	reopened, err := NewStore(ctx, db.DB)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	got, ok := reopened.Get(rec.ID)
	if !ok || got.Cron != "@every 15m" {
		t.Fatalf("reopened store = %+v (ok=%v)", got, ok)
	}
	// The idempotency key survives the restart too.
	if _, changed, err := reopened.Create(ctx, CreateInput{JobID: "job-1", Cron: "@every 15m", IdempotencyKey: "key-1"}); err != nil || changed {
		t.Fatalf("idempotent create after restart: changed=%v err=%v", changed, err)
	}
}

// Only skip is implemented. coalesce and catch_up are reserved names and are
// refused at the write boundary rather than accepted and silently ignored, so a
// caller cannot select behaviour that does not exist.
func TestUnsupportedMissedPoliciesAreRefused(t *testing.T) {
	s, _, ctx := newTestStore(t)

	for _, policy := range []MissedPolicy{MissedCoalesce, MissedCatchUp} {
		if _, _, err := s.Create(ctx, CreateInput{JobID: "job-1", Cron: "@every 1h", MissedPolicy: policy}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Create with missed_policy %q = %v, want ErrInvalid", policy, err)
		}
	}

	rec, _, err := s.Create(ctx, CreateInput{JobID: "job-1", Cron: "@every 1h", MissedPolicy: MissedSkip})
	if err != nil {
		t.Fatalf("Create with skip: %v", err)
	}
	coalesce := MissedCoalesce
	if _, _, err := s.Update(ctx, rec.ID, UpdateInput{MissedPolicy: &coalesce}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Update to coalesce = %v, want ErrInvalid", err)
	}
}
