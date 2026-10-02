package daemon

import (
	"context"
	"errors"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/logging"
)

// applyMigrations migrates the database and reports the schema movement the way
// an operator watching an upgrade needs to see it: one migration_applied record
// per applied migration, nothing when none were pending, and a migration_failed
// record naming the migration that aborted startup.
//
// It is separate from New so that the records -- not just the migration -- are
// testable.
func applyMigrations(ctx context.Context, log *logging.Logger, db *database.DB) error {
	applied, err := database.Migrate(ctx, db)
	if err != nil {
		logMigrationFailure(log, err)
		return err
	}
	// The silence when nothing was pending is the "schema did not move" signal,
	// so an upgrade must never be quiet.
	for _, m := range applied {
		log.Info("migration_applied", "version", m.Version, "name", m.Name)
	}
	return nil
}

// logMigrationFailure reports the migration that aborted startup.
//
// The version and name are logged as fields rather than left inside the error
// text, so a log query can name the exact migration without string matching. A
// failure that is not tied to one migration -- an unreadable embedded set, for
// example -- is still reported, with the error alone.
func logMigrationFailure(log *logging.Logger, err error) {
	var failed *database.MigrationError
	if errors.As(err, &failed) {
		log.Error("migration_failed", err, "version", failed.Version, "name", failed.Name)
		return
	}
	log.Error("migration_failed", err)
}
