package pause

import (
	"context"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// newTestStore opens and migrates a throwaway SQLite database and returns a
// Store bound to it, plus the handle so a test can reopen a second Store over
// the same file.
func newTestStore(t *testing.T) (*Store, *database.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("database.Migrate() error = %v", err)
	}

	store, err := NewStore(ctx, db.DB)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store, db, ctx
}

// An integration with no row is enabled, and that is the zero State rather than
// an error: "never paused" is the common case, not a missing record.
func TestGetWithoutARowIsEnabled(t *testing.T) {
	store, _, _ := newTestStore(t)

	state := store.Get("never-paused")
	if state.Paused {
		t.Errorf("Get() = %+v, want enabled", state)
	}
	if store.Paused("never-paused") {
		t.Error("Paused() = true for an integration with no row")
	}
	if store.Len() != 0 {
		t.Errorf("Len() = %d, want 0", store.Len())
	}
}

func TestSetPauseRecordsWhenItBegan(t *testing.T) {
	store, _, ctx := newTestStore(t)
	before := time.Now().UTC().Add(-time.Second)

	state, changed, err := store.Set(ctx, "int-1", true)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if !changed {
		t.Error("pausing an enabled integration should report a change")
	}
	if !state.Paused {
		t.Error("state should be paused")
	}
	if state.Since.Before(before) || state.Since.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("since = %s, want approximately now", state.Since)
	}
	if !store.Paused("int-1") || store.Len() != 1 {
		t.Errorf("the mirror did not record the pause: paused=%v len=%d", store.Paused("int-1"), store.Len())
	}
}

// Pausing is idempotent so a deploy script can call it unconditionally, and a
// repeat must not move the instant the pause began.
func TestSetPauseIsIdempotent(t *testing.T) {
	store, _, ctx := newTestStore(t)

	first, changed, err := store.Set(ctx, "int-1", true)
	if err != nil || !changed {
		t.Fatalf("Set() = changed %v, err %v", changed, err)
	}

	repeated, changed, err := store.Set(ctx, "int-1", true)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if changed {
		t.Error("re-pausing should not report a change")
	}
	if !repeated.Since.Equal(first.Since) {
		t.Error("a repeat should not move the instant the pause began")
	}
	if store.Len() != 1 {
		t.Errorf("Len() = %d, want 1", store.Len())
	}
}

func TestSetResumeIsIdempotent(t *testing.T) {
	store, _, ctx := newTestStore(t)

	if _, changed, err := store.Set(ctx, "int-1", false); err != nil || changed {
		t.Fatalf("resuming a never-paused integration = changed %v, err %v; want a no-op", changed, err)
	}

	if _, _, err := store.Set(ctx, "int-1", true); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	state, changed, err := store.Set(ctx, "int-1", false)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if !changed {
		t.Error("resuming a paused integration should report a change")
	}
	if state.Paused || !state.Since.IsZero() {
		t.Errorf("state after resume = %+v, want the zero State", state)
	}
	if store.Len() != 0 {
		t.Errorf("Len() = %d after resume, want 0", store.Len())
	}
}

// The mirror is a cache of the table, not a substitute for it: a fresh Store
// over the same database must see the pause, which is what makes a pause
// survive a daemon restart.
func TestANewStoreLoadsPausesFromTheTable(t *testing.T) {
	store, db, ctx := newTestStore(t)

	if _, _, err := store.Set(ctx, "int-1", true); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	reopened, err := NewStore(ctx, db.DB)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	state := reopened.Get("int-1")
	if !state.Paused {
		t.Errorf("reopened state = %+v, want the pause that was written", state)
	}
	if state.Since.IsZero() {
		t.Error("a reloaded pause should keep the instant it began")
	}
	if reopened.Len() != 1 {
		t.Errorf("Len() = %d, want 1", reopened.Len())
	}
}

func TestDeleteForgetsAPause(t *testing.T) {
	store, db, ctx := newTestStore(t)

	if _, _, err := store.Set(ctx, "int-1", true); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := store.Delete(ctx, "int-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if store.Paused("int-1") {
		t.Error("the mirror kept a pause that was deleted")
	}

	reopened, err := NewStore(ctx, db.DB)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if reopened.Len() != 0 {
		t.Error("the deleted pause came back from the table")
	}

	// Deleting an integration that was never paused is the common purge path,
	// so it must not be an error.
	if err := store.Delete(ctx, "never-paused"); err != nil {
		t.Errorf("deleting a pause that does not exist: %v", err)
	}
}
