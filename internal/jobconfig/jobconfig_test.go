package jobconfig

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/database"
)

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
	return NewStore(db.DB), db, ctx
}

// Writing values creates an immutable version and moves the pointer; writing
// the same values again is a no-op that mints nothing.
func TestSetVersionsAndPinsCurrent(t *testing.T) {
	s, _, ctx := newTestStore(t)

	if _, ok, err := s.Current(ctx, "job-1"); err != nil || ok {
		t.Fatalf("Current on an unconfigured job = ok=%v err=%v, want none", ok, err)
	}

	first, changed, err := s.Set(ctx, "job-1", json.RawMessage(`{"dataset":42}`), "admin")
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if !changed || first.ID == "" {
		t.Fatalf("first Set = %+v (changed=%v)", first, changed)
	}

	again, changed, err := s.Set(ctx, "job-1", json.RawMessage(`{"dataset":42}`), "admin")
	if err != nil || changed {
		t.Fatalf("re-setting the same values = changed=%v err=%v, want a no-op", changed, err)
	}
	if again.ID != first.ID {
		t.Errorf("a no-op minted a version: %s != %s", again.ID, first.ID)
	}

	second, changed, err := s.Set(ctx, "job-1", json.RawMessage(`{"dataset":43}`), "admin")
	if err != nil || !changed {
		t.Fatalf("changing values = changed=%v err=%v", changed, err)
	}
	if second.ID == first.ID {
		t.Error("a change reused the previous version id")
	}

	current, ok, err := s.Current(ctx, "job-1")
	if err != nil || !ok {
		t.Fatalf("Current() = ok=%v err=%v", ok, err)
	}
	if current.ID != second.ID {
		t.Errorf("current version = %s, want the newest %s", current.ID, second.ID)
	}

	// The superseded version is immutable and still resolvable, which is what a
	// queued run pinned to it depends on.
	old, ok, err := s.Get(ctx, first.ID)
	if err != nil || !ok {
		t.Fatalf("Get(old) = ok=%v err=%v", ok, err)
	}
	if string(old.Values) != `{"dataset":42}` {
		t.Errorf("old version values = %s", old.Values)
	}
}

func TestSetValidatesValues(t *testing.T) {
	s, _, ctx := newTestStore(t)

	tooLarge := json.RawMessage(`{"k":"` + strings.Repeat("x", MaxValuesBytes) + `"}`)
	cases := []struct {
		name   string
		jobID  string
		values json.RawMessage
	}{
		{"no job", "", json.RawMessage(`{}`)},
		{"array", "job-1", json.RawMessage(`[1,2]`)},
		{"invalid json", "job-1", json.RawMessage(`{`)},
		{"too large", "job-1", tooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Set(ctx, tc.jobID, tc.values, "admin"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Set(%s) = %v, want ErrInvalid", tc.name, err)
			}
		})
	}
}

// Purging an identity removes every version, not just the pointer.
func TestDeleteForJobRemovesEveryVersion(t *testing.T) {
	s, db, ctx := newTestStore(t)

	if _, _, err := s.Set(ctx, "job-1", json.RawMessage(`{"a":1}`), "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Set(ctx, "job-1", json.RawMessage(`{"a":2}`), "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteForJob(ctx, "job-1"); err != nil {
		t.Fatalf("DeleteForJob() error = %v", err)
	}
	if _, ok, err := s.Current(ctx, "job-1"); err != nil || ok {
		t.Errorf("Current after purge = ok=%v err=%v", ok, err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_configs`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("job_configs rows after purge = %d, want 0", rows)
	}
}
