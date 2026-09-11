package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/otter-runtime/otter/internal/database"
)

// newTestStore opens and migrates a throwaway SQLite database and returns a
// Store bound to it. The database is closed when the test finishes.
func newTestStore(t *testing.T) (*Store, context.Context) {
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
	return NewStore(db.DB), ctx
}

// sameJSON reports whether a raw JSON value is semantically equal to want.
func sameJSON(got json.RawMessage, want string) bool {
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		return false
	}
	return reflect.DeepEqual(g, w)
}

func TestSetGetRoundTrip(t *testing.T) {
	store, ctx := newTestStore(t)

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"number", "num", `123`},
		{"string", "str", `"text"`},
		{"object", "obj", `{"a":[1,2]}`},
		{"null", "nil", `null`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Set(ctx, "int-a", tc.key, json.RawMessage(tc.value)); err != nil {
				t.Fatalf("Set(%q) error = %v", tc.key, err)
			}
			got, err := store.Get(ctx, "int-a", tc.key)
			if err != nil {
				t.Fatalf("Get(%q) error = %v", tc.key, err)
			}
			if !sameJSON(got, tc.value) {
				t.Errorf("Get(%q) = %s, want %s", tc.key, got, tc.value)
			}
		})
	}

	t.Run("missing key", func(t *testing.T) {
		_, err := store.Get(ctx, "int-a", "does-not-exist")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
		}
	})

	t.Run("missing key in missing integration", func(t *testing.T) {
		_, err := store.Get(ctx, "int-missing", "key")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(missing integration) error = %v, want ErrNotFound", err)
		}
	})
}

func TestSetOverwritesAndUpdatedAtIncreases(t *testing.T) {
	store, ctx := newTestStore(t)

	if _, err := store.Set(ctx, "int-a", "key", json.RawMessage(`1`)); err != nil {
		t.Fatalf("first Set() error = %v", err)
	}
	first, err := store.GetEntry(ctx, "int-a", "key")
	if err != nil {
		t.Fatalf("first GetEntry() error = %v", err)
	}
	if !sameJSON(first.Value, `1`) {
		t.Fatalf("first value = %s, want 1", first.Value)
	}

	if _, err := store.Set(ctx, "int-a", "key", json.RawMessage(`2`)); err != nil {
		t.Fatalf("second Set() error = %v", err)
	}
	second, err := store.GetEntry(ctx, "int-a", "key")
	if err != nil {
		t.Fatalf("second GetEntry() error = %v", err)
	}
	if !sameJSON(second.Value, `2`) {
		t.Errorf("second value = %s, want 2 (overwrite failed)", second.Value)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt did not increase: first=%s second=%s",
			first.UpdatedAt, second.UpdatedAt)
	}
	if second.Key != "key" {
		t.Errorf("GetEntry().Key = %q, want %q", second.Key, "key")
	}
}

func TestDelete(t *testing.T) {
	store, ctx := newTestStore(t)

	if _, err := store.Set(ctx, "int-a", "key", json.RawMessage(`1`)); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	removed, err := store.Delete(ctx, "int-a", "key")
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !removed {
		t.Errorf("first Delete() = false, want true")
	}

	removed, err = store.Delete(ctx, "int-a", "key")
	if err != nil {
		t.Fatalf("second Delete() error = %v", err)
	}
	if removed {
		t.Errorf("second Delete() = true, want false")
	}

	if _, err := store.Get(ctx, "int-a", "key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() after Delete error = %v, want ErrNotFound", err)
	}
}

func TestAllNamespacingAndDeterminism(t *testing.T) {
	store, ctx := newTestStore(t)

	wantA := map[string]json.RawMessage{
		"alpha": json.RawMessage(`1`),
		"beta":  json.RawMessage(`"b"`),
		"gamma": json.RawMessage(`{"x":0}`),
	}
	for key, value := range wantA {
		if _, err := store.Set(ctx, "int-a", key, value); err != nil {
			t.Fatalf("Set(int-a, %q) error = %v", key, err)
		}
	}
	if _, err := store.Set(ctx, "int-b", "beta", json.RawMessage(`"other"`)); err != nil {
		t.Fatalf("Set(int-b, beta) error = %v", err)
	}

	got, err := store.All(ctx, "int-a")
	if err != nil {
		t.Fatalf("All(int-a) error = %v", err)
	}
	if len(got) != len(wantA) {
		t.Fatalf("All(int-a) returned %d keys, want %d: %v", len(got), len(wantA), got)
	}
	if !reflect.DeepEqual(got, wantA) {
		t.Errorf("All(int-a) = %v, want %v", got, wantA)
	}

	// Keys written for another integration must not leak.
	gotB, err := store.All(ctx, "int-b")
	if err != nil {
		t.Fatalf("All(int-b) error = %v", err)
	}
	if len(gotB) != 1 {
		t.Fatalf("All(int-b) returned %d keys, want 1: %v", len(gotB), gotB)
	}
	if !sameJSON(gotB["beta"], `"other"`) {
		t.Errorf("All(int-b)[beta] = %s, want \"other\"", gotB["beta"])
	}

	// Repeated reads are deterministic even if unordered maps hide SQL order.
	again, err := store.All(ctx, "int-a")
	if err != nil {
		t.Fatalf("second All(int-a) error = %v", err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Errorf("All(int-a) is not deterministic:\nfirst  = %v\nsecond = %v", got, again)
	}
}

func TestValidateKey(t *testing.T) {
	long := strings.Repeat("a", MaxKeyLength)

	valid := []string{
		"simple",
		"MixedCase123",
		"with.dots",
		"with-dash",
		"with_underscore",
		"with:colon",
		"A.B_c-1:2",
		long,
	}
	for i, key := range valid {
		t.Run(fmt.Sprintf("valid_%d", i), func(t *testing.T) {
			if err := ValidateKey(key); err != nil {
				t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
			}
		})
	}

	invalid := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"too_long", strings.Repeat("a", MaxKeyLength+1)},
		{"slash", "a/b"},
		{"space", "a b"},
		{"unicode", "café"},
		{"question_mark", "a?b"},
		{"newline", "a\nb"},
		{"percent", "a%b"},
	}
	for _, tc := range invalid {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			err := ValidateKey(tc.key)
			if err == nil {
				t.Fatalf("ValidateKey(%q) = nil, want error", tc.key)
			}
			if !errors.Is(err, ErrInvalidKey) {
				t.Errorf("ValidateKey(%q) error = %v, want it to wrap ErrInvalidKey", tc.key, err)
			}
		})
	}
}

func TestValidateValue(t *testing.T) {
	valid := []string{`123`, `"text"`, `{"a":[1,2]}`, `null`, `[]`, `true`}
	for i, value := range valid {
		t.Run(fmt.Sprintf("valid_%d", i), func(t *testing.T) {
			if err := ValidateValue(json.RawMessage(value)); err != nil {
				t.Errorf("ValidateValue(%q) = %v, want nil", value, err)
			}
		})
	}

	invalid := []struct {
		name  string
		value json.RawMessage
	}{
		{"empty", json.RawMessage("")},
		{"nil", nil},
		{"plain text", json.RawMessage("not json")},
		{"truncated object", json.RawMessage(`{"a":`)},
		{"truncated literal", json.RawMessage(`tru`)},
	}
	for _, tc := range invalid {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			err := ValidateValue(tc.value)
			if err == nil {
				t.Fatalf("ValidateValue(%q) = nil, want error", tc.value)
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Errorf("ValidateValue(%q) error = %v, want it to wrap ErrInvalidValue", tc.value, err)
			}
		})
	}
}

func TestDeleteAll(t *testing.T) {
	store, ctx := newTestStore(t)

	for _, key := range []string{"a", "b", "c"} {
		if _, err := store.Set(ctx, "int-a", key, json.RawMessage(`1`)); err != nil {
			t.Fatalf("Set(int-a, %q) error = %v", key, err)
		}
	}
	if _, err := store.Set(ctx, "int-b", "keep", json.RawMessage(`1`)); err != nil {
		t.Fatalf("Set(int-b, keep) error = %v", err)
	}

	removed, err := store.DeleteAll(ctx, "int-a")
	if err != nil {
		t.Fatalf("DeleteAll(int-a) error = %v", err)
	}
	if removed != 3 {
		t.Errorf("DeleteAll(int-a) removed %d, want 3", removed)
	}

	gotA, err := store.All(ctx, "int-a")
	if err != nil {
		t.Fatalf("All(int-a) error = %v", err)
	}
	if len(gotA) != 0 {
		t.Errorf("All(int-a) = %v, want empty", gotA)
	}

	gotB, err := store.All(ctx, "int-b")
	if err != nil {
		t.Fatalf("All(int-b) error = %v", err)
	}
	if len(gotB) != 1 {
		t.Errorf("DeleteAll(int-a) leaked into int-b: got %v", gotB)
	}

	// Clearing an already-empty integration is a no-op, not an error.
	removed, err = store.DeleteAll(ctx, "int-a")
	if err != nil {
		t.Fatalf("second DeleteAll(int-a) error = %v", err)
	}
	if removed != 0 {
		t.Errorf("second DeleteAll(int-a) removed %d, want 0", removed)
	}
}

func TestConcurrentWriters(t *testing.T) {
	store, ctx := newTestStore(t)
	const writers = 20

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key%02d", i)
			value := json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
			if _, err := store.Set(ctx, "int-concurrent", key, value); err != nil {
				errCh <- fmt.Errorf("Set(%q): %w", key, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent writer error: %v", err)
	}

	all, err := store.All(ctx, "int-concurrent")
	if err != nil {
		t.Fatalf("All() error = %v", err)
	}
	if len(all) != writers {
		t.Fatalf("All() returned %d keys, want %d", len(all), writers)
	}
	for i := 0; i < writers; i++ {
		key := fmt.Sprintf("key%02d", i)
		value, ok := all[key]
		if !ok {
			t.Errorf("key %q missing after concurrent writes", key)
			continue
		}
		var decoded struct {
			I int `json:"i"`
		}
		if err := json.Unmarshal(value, &decoded); err != nil {
			t.Errorf("unmarshal %q (%s): %v", key, value, err)
			continue
		}
		if decoded.I != i {
			t.Errorf("key %q decoded i=%d, want %d", key, decoded.I, i)
		}
	}
}

func TestSetRejectsInvalidInput(t *testing.T) {
	store, ctx := newTestStore(t)

	if _, err := store.Set(ctx, "int-a", "bad/key", json.RawMessage(`1`)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Set(bad key) error = %v, want ErrInvalidKey", err)
	}
	if _, err := store.Set(ctx, "int-a", "good", json.RawMessage(`{`)); !errors.Is(err, ErrInvalidValue) {
		t.Errorf("Set(bad value) error = %v, want ErrInvalidValue", err)
	}
	if _, err := store.Get(ctx, "int-a", "bad/key"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Get(bad key) error = %v, want ErrInvalidKey", err)
	}
	if _, err := store.Delete(ctx, "int-a", "bad/key"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Delete(bad key) error = %v, want ErrInvalidKey", err)
	}
}
