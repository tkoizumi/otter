// Package runs stores execution history in SQLite.
//
// One row is written per execution attempt. A retry creates a new row whose
// parent_run_id points at the attempt being retried and whose attempt value
// is incremented, which keeps a full audit trail of a retry chain.
package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/otter-runtime/otter/internal/database"
)

// ErrNotFound is returned when a run id does not exist.
var ErrNotFound = errors.New("run not found")

// Status is the lifecycle state of a run.
type Status string

// Run statuses.
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusRetrying  Status = "retrying"
	StatusCancelled Status = "cancelled"
	StatusTimedOut  Status = "timed_out"
)

// AllStatuses lists every valid status.
func AllStatuses() []Status {
	return []Status{
		StatusQueued, StatusRunning, StatusSucceeded,
		StatusFailed, StatusRetrying, StatusCancelled, StatusTimedOut,
	}
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, known := range AllStatuses() {
		if s == known {
			return true
		}
	}
	return false
}

// Terminal reports whether the attempt has finished for good. Queued,
// retrying and running are not terminal.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusTimedOut:
		return true
	default:
		return false
	}
}

// Trigger types recorded on a run.
const (
	TriggerManual  = "manual"
	TriggerCron    = "cron"
	TriggerWebhook = "webhook"
)

// Run is one execution attempt of an integration.
type Run struct {
	ID            string          `json:"id"`
	IntegrationID string          `json:"integration_id"`
	TriggerType   string          `json:"trigger_type"`
	Status        Status          `json:"status"`
	Attempt       int             `json:"attempt"`
	ParentRunID   *string         `json:"parent_run_id"`
	CreatedAt     time.Time       `json:"created_at"`
	StartedAt     *time.Time      `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
	ExitCode      *int            `json:"exit_code"`
	Error         *string         `json:"error"`
	Metadata      json.RawMessage `json:"metadata"`
}

// Duration returns how long the run has been running, or ran for.
func (r *Run) Duration() time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	end := time.Now().UTC()
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	return end.Sub(*r.StartedAt)
}

// ErrorString dereferences the error message.
func (r *Run) ErrorString() string {
	if r.Error == nil {
		return ""
	}
	return *r.Error
}

const runColumns = `id, integration_id, trigger_type, status, attempt, parent_run_id,
	created_at, started_at, finished_at, exit_code, error, metadata`

// Store provides access to run records.
type Store struct {
	db *sql.DB
}

// NewStore wraps a database handle.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Create inserts a run record.
func (s *Store) Create(ctx context.Context, r *Run) error {
	return s.CreateTx(ctx, nil, r)
}

// CreateTx inserts a run record, optionally inside an existing transaction.
func (s *Store) CreateTx(ctx context.Context, tx *sql.Tx, r *Run) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Attempt < 1 {
		r.Attempt = 1
	}
	if !r.Status.Valid() {
		return fmt.Errorf("runs: invalid status %q", r.Status)
	}
	metadata := string(r.Metadata)
	if strings.TrimSpace(metadata) == "" {
		metadata = ""
	}

	const q = `INSERT INTO runs (` + runColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{
		r.ID, r.IntegrationID, r.TriggerType, string(r.Status), r.Attempt,
		database.NullableString(deref(r.ParentRunID)),
		database.FormatTime(r.CreatedAt),
		database.FormatNullable(r.StartedAt),
		database.FormatNullable(r.FinishedAt),
		database.NullableInt(r.ExitCode),
		database.NullableString(deref(r.Error)),
		metadata,
	}

	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, q, args...)
	} else {
		_, err = s.db.ExecContext(ctx, q, args...)
	}
	if err != nil {
		return fmt.Errorf("runs: insert %s: %w", r.ID, err)
	}
	return nil
}

// Get loads a single run.
func (s *Store) Get(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runs: get %s: %w", id, err)
	}
	return r, nil
}

// Filter narrows a run listing.
type Filter struct {
	IntegrationID string
	Status        Status
	ParentRunID   string
	Limit         int
	Offset        int
	Ascending     bool
}

// List returns runs matching the filter, newest first by default.
func (s *Store) List(ctx context.Context, f Filter) ([]*Run, error) {
	var (
		where []string
		args  []any
	)
	if f.IntegrationID != "" {
		where = append(where, "integration_id = ?")
		args = append(args, f.IntegrationID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.ParentRunID != "" {
		where = append(where, "parent_run_id = ?")
		args = append(args, f.ParentRunID)
	}

	query := `SELECT ` + runColumns + ` FROM runs`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Ascending {
		query += " ORDER BY created_at ASC, rowid ASC"
	} else {
		query += " ORDER BY created_at DESC, rowid DESC"
	}

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit, max0(f.Offset))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("runs: list: %w", err)
	}
	defer rows.Close()

	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("runs: list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListByStatus returns runs in a given status, oldest first so that recovery
// and queue reconciliation process them in creation order.
func (s *Store) ListByStatus(ctx context.Context, status Status, limit int) ([]*Run, error) {
	return s.List(ctx, Filter{Status: status, Limit: limit, Ascending: true})
}

// CountByStatus returns the number of runs in each status.
func (s *Store) CountByStatus(ctx context.Context) (map[Status]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM runs GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("runs: count by status: %w", err)
	}
	defer rows.Close()

	counts := map[Status]int{}
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("runs: count by status scan: %w", err)
		}
		counts[Status(status)] = n
	}
	return counts, rows.Err()
}

// MarkRunning transitions a queued or retrying run into running. It returns
// false when the run is no longer claimable, for example because it was
// cancelled while waiting in the queue.
func (s *Store) MarkRunning(ctx context.Context, id string, startedAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, started_at = ?, finished_at = NULL, exit_code = NULL, error = NULL
		 WHERE id = ? AND status IN (?, ?)`,
		string(StatusRunning), database.FormatTime(startedAt), id,
		string(StatusQueued), string(StatusRetrying))
	if err != nil {
		return false, fmt.Errorf("runs: mark running %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("runs: mark running %s: %w", id, err)
	}
	return n == 1, nil
}

// Finish records the terminal state of an attempt.
type Finish struct {
	Status     Status
	ExitCode   *int
	Error      string
	FinishedAt time.Time
}

// Finish writes the terminal state of a run.
func (s *Store) Finish(ctx context.Context, id string, f Finish) error {
	if !f.Status.Terminal() {
		return fmt.Errorf("runs: finish %s: status %q is not terminal", id, f.Status)
	}
	if f.FinishedAt.IsZero() {
		f.FinishedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, exit_code = ?, error = ?
		 WHERE id = ?`,
		string(f.Status), database.FormatTime(f.FinishedAt),
		database.NullableInt(f.ExitCode), database.NullableString(f.Error), id)
	if err != nil {
		return fmt.Errorf("runs: finish %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatus updates only the status of a run.
func (s *Store) SetStatus(ctx context.Context, id string, status Status, errMsg string) error {
	if !status.Valid() {
		return fmt.Errorf("runs: invalid status %q", status)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, error = ? WHERE id = ?`,
		string(status), database.NullableString(errMsg), id)
	if err != nil {
		return fmt.Errorf("runs: set status %s: %w", id, err)
	}
	return nil
}

// Chain returns the root run and every attempt derived from it, ordered by
// attempt number. The chain is linear: each attempt has at most one successor.
func (s *Store) Chain(ctx context.Context, id string) (*Run, []*Run, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	// Walk up to the root.
	root := current
	for i := 0; i < 1000 && root.ParentRunID != nil; i++ {
		parent, err := s.Get(ctx, *root.ParentRunID)
		if err != nil {
			break // a missing parent means this is as far up as we can go
		}
		root = parent
	}

	// Walk back down, collecting attempts.
	attempts := []*Run{root}
	seen := map[string]bool{root.ID: true}
	node := root
	for i := 0; i < 1000; i++ {
		children, err := s.List(ctx, Filter{ParentRunID: node.ID, Limit: 10, Ascending: true})
		if err != nil {
			return nil, nil, err
		}
		var next *Run
		for _, c := range children {
			if !seen[c.ID] {
				next = c
				break
			}
		}
		if next == nil {
			break
		}
		seen[next.ID] = true
		attempts = append(attempts, next)
		node = next
	}

	return root, attempts, nil
}

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var (
		r        Run
		status   string
		parent   sql.NullString
		created  database.NullableTime
		started  database.NullableTime
		finished database.NullableTime
		exitCode sql.NullInt64
		errMsg   sql.NullString
		meta     sql.NullString
	)
	if err := sc.Scan(
		&r.ID, &r.IntegrationID, &r.TriggerType, &status, &r.Attempt,
		&parent, &created, &started, &finished, &exitCode, &errMsg, &meta,
	); err != nil {
		return nil, err
	}

	r.Status = Status(status)
	if parent.Valid {
		v := parent.String
		r.ParentRunID = &v
	}
	if created.Valid {
		r.CreatedAt = created.Time
	}
	r.StartedAt = started.Ptr()
	r.FinishedAt = finished.Ptr()
	if exitCode.Valid {
		v := int(exitCode.Int64)
		r.ExitCode = &v
	}
	if errMsg.Valid {
		v := errMsg.String
		r.Error = &v
	}
	if meta.Valid && strings.TrimSpace(meta.String) != "" {
		r.Metadata = json.RawMessage(meta.String)
	}
	return &r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
