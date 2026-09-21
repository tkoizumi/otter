package identity

import (
	"context"
	"testing"

	"github.com/tkoizumi/otter/internal/database"
)

// newTestStore builds a migrated SQLite database in a temporary directory.
func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("database.Close: %v", err)
		}
	})
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}
	return NewStore(db.DB), ctx
}
