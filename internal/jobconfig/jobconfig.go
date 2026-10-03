// Package jobconfig owns a job's configuration: the values between the release
// and the run.
//
// A release is code and is immutable. An environment variable in the manifest
// is part of that code. But a store name, a dataset id or a backfill date is
// neither code nor secret: it changes per tenant and per campaign, and editing
// a manifest to change it means a release per value.
//
// Configuration is therefore versioned. Writing values creates a new immutable
// version and moves a pointer; every run records the version it resolved at
// submission, so a queued, retrying or backlogged attempt keeps the values it
// was accepted with. Reading live values at execution would break exactly the
// guarantee releases exist to provide.
package jobconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tkoizumi/otter/internal/database"
)

// MaxValuesBytes bounds a configuration document. 64 KiB is generous for the
// deployment values a job needs and keeps configuration from becoming a bulk
// data store. Configuration values are not secrets and must never hold one.
const MaxValuesBytes = 64 * 1024

// Sentinel errors, mapped by the daemon onto API errors.
var (
	// ErrNotFound reports an unknown version or a job with no configuration.
	ErrNotFound = errors.New("job configuration not found")
	// ErrInvalid reports a malformed configuration document.
	ErrInvalid = errors.New("invalid job configuration")
)

// Version is one immutable configuration document.
type Version struct {
	ID        string
	JobID     string
	Values    json.RawMessage
	CreatedAt time.Time
	CreatedBy string
}

// Store is the durable configuration store.
//
// It is read on submission (to pin a version) and on execution (to resolve the
// pinned version), and written rarely. Unlike the schedule store it keeps no
// in-memory mirror: configuration does not need to be consulted on the cron
// path, and a version is immutable once written, so a database read is both
// cheap enough and impossible to make stale.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store over db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const versionColumns = `id, job_id, config_values, created_at, created_by`

// Current returns the version a new run should pin, if the job has one.
func (s *Store) Current(ctx context.Context, jobID string) (Version, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+prefixColumns("c", versionColumns)+`
		   FROM job_config_current cur
		   JOIN job_configs c ON c.id = cur.config_id
		  WHERE cur.job_id = ?`, jobID)
	rec, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, fmt.Errorf("jobconfig: current %s: %w", jobID, err)
	}
	return rec, true, nil
}

// Get returns one immutable version by id.
func (s *Store) Get(ctx context.Context, versionID string) (Version, bool, error) {
	if strings.TrimSpace(versionID) == "" {
		return Version{}, false, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM job_configs WHERE id = ?`, versionID)
	rec, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, fmt.Errorf("jobconfig: get %s: %w", versionID, err)
	}
	return rec, true, nil
}

// Set writes a new immutable version and points the job at it. It reports
// whether anything changed: setting the current values again is a no-op that
// does not mint a version, so a deploy script can apply configuration
// unconditionally.
func (s *Store) Set(ctx context.Context, jobID string, values json.RawMessage, by string) (Version, bool, error) {
	if strings.TrimSpace(jobID) == "" {
		return Version{}, false, fmt.Errorf("%w: job id is required", ErrInvalid)
	}
	normalized, err := normalize(values)
	if err != nil {
		return Version{}, false, err
	}

	if current, ok, err := s.Current(ctx, jobID); err != nil {
		return Version{}, false, err
	} else if ok && string(current.Values) == string(normalized) {
		return current, false, nil
	}

	now := time.Now().UTC()
	rec := Version{
		ID:        uuid.NewString(),
		JobID:     jobID,
		Values:    normalized,
		CreatedAt: now,
		CreatedBy: by,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Version{}, false, fmt.Errorf("jobconfig: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_configs (`+versionColumns+`) VALUES (?, ?, ?, ?, ?)`,
		rec.ID, rec.JobID, string(rec.Values), database.FormatTime(rec.CreatedAt), rec.CreatedBy); err != nil {
		return Version{}, false, fmt.Errorf("jobconfig: insert %s: %w", jobID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_config_current (job_id, config_id, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(job_id) DO UPDATE SET config_id = excluded.config_id, updated_at = excluded.updated_at`,
		jobID, rec.ID, database.FormatTime(now)); err != nil {
		return Version{}, false, fmt.Errorf("jobconfig: point %s: %w", jobID, err)
	}
	if err := tx.Commit(); err != nil {
		return Version{}, false, fmt.Errorf("jobconfig: commit: %w", err)
	}
	return rec, true, nil
}

// DeleteForJob removes a job's configuration and every version of it. It is
// called when an identity is purged.
//
// Versions pinned by a run that still exists are not a concern here: the purge
// belongs to the same `otter delete` that removes the runs.
func (s *Store) DeleteForJob(ctx context.Context, jobID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jobconfig: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM job_config_current WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("jobconfig: delete current %s: %w", jobID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_configs WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("jobconfig: delete versions %s: %w", jobID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("jobconfig: commit: %w", err)
	}
	return nil
}

// List returns a job's versions, newest first.
func (s *Store) List(ctx context.Context, jobID string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+versionColumns+` FROM job_configs WHERE job_id = ? ORDER BY created_at DESC, id DESC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("jobconfig: list %s: %w", jobID, err)
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		rec, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("jobconfig: scan %s: %w", jobID, err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// prefixColumns qualifies every column of a comma-separated list with a table
// alias, which the join in Current needs.
func prefixColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

func scanVersion(sc interface{ Scan(...any) error }) (Version, error) {
	var (
		rec       Version
		values    string
		createdAt string
	)
	if err := sc.Scan(&rec.ID, &rec.JobID, &values, &createdAt, &rec.CreatedBy); err != nil {
		return Version{}, err
	}
	rec.Values = json.RawMessage(values)
	t, err := database.ParseTime(createdAt)
	if err != nil {
		return Version{}, fmt.Errorf("%s created_at: %w", rec.ID, err)
	}
	rec.CreatedAt = t
	return rec, nil
}

// normalize validates and canonicalizes a configuration document.
func normalize(values json.RawMessage) (json.RawMessage, error) {
	if len(values) == 0 {
		return json.RawMessage("{}"), nil
	}
	if len(values) > MaxValuesBytes {
		return nil, fmt.Errorf("%w: values are %d bytes; the maximum is %d", ErrInvalid, len(values), MaxValuesBytes)
	}
	trimmed := strings.TrimSpace(string(values))
	if !json.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("%w: values are not valid JSON", ErrInvalid)
	}
	if !strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("%w: values must be a JSON object", ErrInvalid)
	}
	return json.RawMessage(trimmed), nil
}
