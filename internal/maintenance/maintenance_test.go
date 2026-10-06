package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/tkoizumi/otter/internal/database"
)

// newStore opens a migrated database and loads the maintenance store over it,
// the same way the daemon does at start.
func newStore(t *testing.T) (*Store, *database.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s, db, ctx
}

// A runtime with no maintenance row serves. This is the compatibility
// requirement, not a detail: an ordinary dedicated deployment has no row and
// must behave exactly as it did before maintenance existed, so "absent" cannot
// mean "gated".
func TestRuntimeWithNoRowServes(t *testing.T) {
	s, db, ctx := newStore(t)

	state := s.State()
	if state.Mode != ModeServing {
		t.Errorf("fresh mode = %q, want %q", state.Mode, ModeServing)
	}
	if s.Gated() {
		t.Error("a runtime with no maintenance row reported gated")
	}
	if err := db.Tx(ctx, func(tx *sql.Tx) error { return s.CheckTx(ctx, tx) }); err != nil {
		t.Errorf("CheckTx refused work on a runtime that never opted in: %v", err)
	}
}

// Starting gated is the explicit opt-in the pooled lifecycle uses: it persists
// the gate before anything can run, and it survives a reload, so a crash before
// activation cannot bring the runtime back up serving.
func TestStartGatedPersistsAndSurvivesReload(t *testing.T) {
	s, db, ctx := newStore(t)

	state, changed, err := s.StartGated(ctx, "pooled start")
	if err != nil {
		t.Fatalf("StartGated: %v", err)
	}
	if !changed {
		t.Error("StartGated reported no change on a serving runtime")
	}
	if state.Mode != ModeStartup {
		t.Errorf("mode = %q, want %q", state.Mode, ModeStartup)
	}
	if state.Explicit {
		t.Error("a start is not an operator's decision, so Explicit must be false")
	}
	if !s.Gated() {
		t.Error("StartGated did not gate the runtime")
	}
	if err := db.Tx(ctx, func(tx *sql.Tx) error { return s.CheckTx(ctx, tx) }); !errors.Is(err, ErrGated) {
		t.Errorf("CheckTx admitted work on a gated start: %v", err)
	}

	reloaded, err := Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Gated() {
		t.Error("the gate did not survive a reload, so a crash would resume serving")
	}

	// An operator's own maintenance outranks a later start: the window began
	// before this process did, so its entry time and reason are kept.
	entered, _, err := s.Enter(ctx, "operator window")
	if err != nil {
		t.Fatal(err)
	}
	after, changed, err := s.StartGated(ctx, "another start")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("StartGated changed an already-gated runtime")
	}
	if !after.EnteredAt.Equal(entered.EnteredAt) || after.Reason != "operator window" {
		t.Errorf("start clobbered the operator's window: %+v", after)
	}
}

// Enter is durable and idempotent: the first call changes state, the second
// reports no change, and the entry time is the operator's, not the retry's.
func TestEnterIsDurableAndIdempotent(t *testing.T) {
	s, db, ctx := newStore(t)

	first, changed, err := s.Enter(ctx, "planned upgrade")
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if !changed {
		t.Error("the first Enter reported no change")
	}
	if first.Mode != ModeDraining {
		t.Errorf("mode after Enter = %q, want %q (draining is the honest start)", first.Mode, ModeDraining)
	}
	if first.Reason != "planned upgrade" {
		t.Errorf("reason = %q, want the caller's", first.Reason)
	}
	if !s.Gated() {
		t.Error("the runtime is not gated after Enter")
	}

	// A second Enter is a no-op and must not move the entry time: an operator
	// reading "held back for 40 minutes" should not see that reset by a retry.
	second, changed, err := s.Enter(ctx, "retry")
	if err != nil {
		t.Fatalf("second Enter: %v", err)
	}
	if changed {
		t.Error("a repeated Enter reported a change")
	}
	if !second.EnteredAt.Equal(first.EnteredAt) {
		t.Errorf("entry time moved from %s to %s", first.EnteredAt, second.EnteredAt)
	}
	if second.Reason != "planned upgrade" {
		t.Errorf("reason = %q, want the original to survive", second.Reason)
	}

	// And it survives a fresh load over the same database, which is what a
	// restart is.
	reloaded, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Gated() {
		t.Error("maintenance did not survive a reload")
	}
	if got := reloaded.State().Mode; got != ModeDraining {
		t.Errorf("mode after reload = %q, want %q", got, ModeDraining)
	}
}

// Exit clears the gate and admits work again, and an operator's decision has to
// survive a restart -- otherwise a crash would silently re-gate a runtime that
// was serving, or worse, un-gate one that was not.
func TestExitClearsTheGateAndSurvivesReload(t *testing.T) {
	s, db, ctx := newStore(t)

	if _, _, err := s.Enter(ctx, ""); err != nil {
		t.Fatal(err)
	}
	state, changed, err := s.Exit(ctx)
	if err != nil {
		t.Fatalf("Exit: %v", err)
	}
	if !changed {
		t.Error("Exit reported no change")
	}
	if state.Mode != ModeServing {
		t.Errorf("mode after Exit = %q, want %q", state.Mode, ModeServing)
	}
	if s.Gated() {
		t.Error("the runtime is still gated after Exit")
	}

	reloaded, err := Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Gated() {
		t.Error("an operator's activation did not survive a reload")
	}

	// Exiting again is a no-op, so a deploy script can call it unconditionally.
	again, changed, err := s.Exit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a repeated Exit reported a change")
	}
	if again.Mode != ModeServing {
		t.Errorf("mode = %q, want serving", again.Mode)
	}
}

// SetMode records that a drain finished without losing the operator's entry
// time, so "held back for N minutes" stays true across the transition.
func TestSetModeKeepsTheOriginalEntryTime(t *testing.T) {
	s, _, ctx := newStore(t)

	entered, _, err := s.Enter(ctx, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	drained, err := s.SetMode(ctx, ModeMaintenance, "")
	if err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if drained.Mode != ModeMaintenance {
		t.Errorf("mode = %q, want %q", drained.Mode, ModeMaintenance)
	}
	if !drained.EnteredAt.Equal(entered.EnteredAt) {
		t.Errorf("entry time moved from %s to %s", entered.EnteredAt, drained.EnteredAt)
	}
	if drained.Reason != "upgrade" {
		t.Errorf("reason = %q, want the operator's to survive", drained.Reason)
	}
	if !s.Gated() {
		t.Error("a drained runtime reported serving")
	}
}

// CheckTx reads the table, not the cache, so a committed gate cannot be missed
// by a transaction that started before it.
func TestCheckTxReadsThePersistedState(t *testing.T) {
	s, db, ctx := newStore(t)

	if _, _, err := s.Exit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *sql.Tx) error { return s.CheckTx(ctx, tx) }); err != nil {
		t.Fatalf("CheckTx refused a serving runtime: %v", err)
	}

	if _, _, err := s.Enter(ctx, "now"); err != nil {
		t.Fatal(err)
	}
	err := db.Tx(ctx, func(tx *sql.Tx) error { return s.CheckTx(ctx, tx) })
	if !errors.Is(err, ErrGated) {
		t.Fatalf("CheckTx on a gated runtime = %v, want ErrGated", err)
	}
}
