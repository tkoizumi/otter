package runs

import (
	"context"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/secrets"
)

// RecordSecretSources stores where each declared secret came from, for one job.
//
// A REPORT, NOT CONFIGURATION. Nothing reads this to resolve anything; it exists
// so a control plane that cannot read a secret value (CL-13) can still tell an
// operator which source last satisfied it. Values never appear here — the map is
// name → source.
//
// The write is best-effort from the caller's point of view: a failure to record
// must not fail a run whose secrets resolved correctly.
func (s *Store) RecordSecretSources(ctx context.Context, jobID, runID string, sources map[string]secrets.Source, at time.Time) error {
	if jobID == "" || len(sources) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record secret sources: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for name, source := range sources {
		if name == "" || source == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_secret_sources (job_id, name, source, run_id, observed_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(job_id, name) DO UPDATE SET
			   source = excluded.source,
			   run_id = excluded.run_id,
			   observed_at = excluded.observed_at`,
			jobID, name, string(source), runID, database.FormatTime(at),
		); err != nil {
			return fmt.Errorf("record secret sources: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record secret sources: %w", err)
	}
	return nil
}

// SecretSourcesByJob returns the last recorded source per declared secret, keyed
// by job then by secret name. One query for a whole listing, like the freshness
// map beside it.
func (s *Store) SecretSourcesByJob(ctx context.Context) (map[string]map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id, name, source FROM job_secret_sources`)
	if err != nil {
		return nil, fmt.Errorf("read secret sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]map[string]string)
	for rows.Next() {
		var jobID, name, source string
		if err := rows.Scan(&jobID, &name, &source); err != nil {
			return nil, fmt.Errorf("read secret sources: %w", err)
		}
		if out[jobID] == nil {
			out[jobID] = make(map[string]string)
		}
		out[jobID][name] = source
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read secret sources: %w", err)
	}
	return out, nil
}

// SecretSources returns the last recorded source per declared secret for one job.
func (s *Store) SecretSources(ctx context.Context, jobID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, source FROM job_secret_sources WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, fmt.Errorf("read secret sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var name, source string
		if err := rows.Scan(&name, &source); err != nil {
			return nil, fmt.Errorf("read secret sources: %w", err)
		}
		out[name] = source
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read secret sources: %w", err)
	}
	return out, nil
}

// ForgetSecretSources drops a job's rows. A deleted job must not leave a record
// claiming what its credentials were.
func (s *Store) ForgetSecretSources(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM job_secret_sources WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("forget secret sources: %w", err)
	}
	return nil
}
