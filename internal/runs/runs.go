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

	"github.com/tkoizumi/otter/internal/database"
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

// Run is one execution attempt of a job.
type Run struct {
	ID                string          `json:"id"`
	JobID             string          `json:"job_id"`
	TriggerType       string          `json:"trigger_type"`
	Status            Status          `json:"status"`
	Attempt           int             `json:"attempt"`
	ParentRunID       *string         `json:"parent_run_id"`
	CreatedAt         time.Time       `json:"created_at"`
	StartedAt         *time.Time      `json:"started_at"`
	FinishedAt        *time.Time      `json:"finished_at"`
	ExitCode          *int            `json:"exit_code"`
	Error             *string         `json:"error"`
	Metadata          json.RawMessage `json:"metadata"`
	PythonMode        string          `json:"python_mode,omitempty"`
	PythonVersion     string          `json:"python_version,omitempty"`
	EnvironmentDigest string          `json:"environment_digest,omitempty"`
	// PythonPolicy is the preparation-policy fingerprint folded into
	// EnvironmentDigest. It is what lets a retry resolve the same environment
	// its parent selected without re-deriving the policy.
	PythonPolicy string `json:"-"`
	// ReleaseDigest identifies the immutable source snapshot this attempt
	// executes, and ReleaseSourceDir is the directory that snapshot lives in.
	// Both are recorded at submission so a retry keeps running its parent's
	// snapshot after a newer release has been activated.
	ReleaseDigest    string `json:"release_digest,omitempty"`
	ReleaseSourceDir string `json:"-"`
	SDKVersion       string `json:"sdk_version,omitempty"`

	// JobName is the manifest label the run was submitted under, and
	// JobGeneration is the identity generation that authorized it.
	// A generation mismatch after a reset, move, retirement or deletion is
	// what fences a stale worker or state write.
	JobName       string `json:"job_name,omitempty"`
	JobGeneration int64  `json:"job_generation,omitempty"`

	// CapturePolicy is the HTTP capture policy this run was submitted with. A
	// retry inherits it from its parent. An empty value means the run predates
	// capture, which is what lets a reader tell "not recorded" from "recorded
	// nothing".
	CapturePolicy string `json:"capture_policy,omitempty"`

	// ScheduleID names the schedule whose occurrence started this run. It is
	// empty for a manual, webhook or retried run, and it is what lets a reader
	// correlate an occurrence with the run it produced.
	ScheduleID string `json:"schedule_id,omitempty"`

	// ConfigVersion is the job configuration version this run resolved at
	// submission. It is empty when the job had no configuration. Pinning it is
	// what keeps a queued, retrying or backlogged attempt on the values it was
	// accepted with, exactly as ReleaseDigest keeps it on its code.
	ConfigVersion string `json:"config_version,omitempty"`
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

const runColumns = `id, job_id, trigger_type, status, attempt, parent_run_id,
	created_at, started_at, finished_at, exit_code, error, metadata,
	python_mode, python_version, environment_digest, python_policy,
	release_digest, release_source_dir, sdk_version,
	job_name, job_generation, capture_policy, schedule_id, config_version`

// defaultListLimit is the page size List substitutes when a caller supplies no
// limit, and maxListLimit is the largest explicit limit it accepts. A limit
// outside (0, maxListLimit] is out of range and is replaced by the default.
//
// The substitution is deliberate for display reads -- "the newest 20 runs" --
// but it is silent: a caller that asks for 10,000 rows receives 50 and a nil
// error. Reads that must be complete use ListByStatusAll instead.
const (
	defaultListLimit = 50
	maxListLimit     = 1000
)

// DebugLogger is the subset of the daemon logger the store uses to make a
// rejected limit observable. A store with no logger stays silent.
type DebugLogger interface {
	Debug(event string, kv ...any)
}

// Store provides access to run records.
type Store struct {
	db  *sql.DB
	log DebugLogger
}

// NewStore wraps a database handle.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// WithLogger attaches a logger for diagnostic events and returns the store, so
// a caller can chain it onto NewStore.
func (s *Store) WithLogger(log DebugLogger) *Store {
	s.log = log
	return s
}

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

	const q = `INSERT INTO runs (` + runColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{
		r.ID, r.JobID, r.TriggerType, string(r.Status), r.Attempt,
		database.NullableString(deref(r.ParentRunID)),
		database.FormatTime(r.CreatedAt),
		database.FormatNullable(r.StartedAt),
		database.FormatNullable(r.FinishedAt),
		database.NullableInt(r.ExitCode),
		database.NullableString(deref(r.Error)),
		metadata, r.PythonMode, r.PythonVersion, r.EnvironmentDigest, r.PythonPolicy,
		r.ReleaseDigest, r.ReleaseSourceDir, r.SDKVersion,
		r.JobName, r.JobGeneration, r.CapturePolicy,
		database.NullableString(r.ScheduleID), database.NullableString(r.ConfigVersion),
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

// FindIdempotencyTx returns the run a (job, key) pair already produced.
//
// The caller checks this INSIDE the transaction that creates the run, which is
// what makes "check then insert" atomic: two concurrent deliveries of the same
// command cannot both see "not found" and both create a run. An early call
// outside a transaction is the fast path for the ordinary retry; this method
// serves both, so the retry and the race take the same code path.
func (s *Store) FindIdempotencyTx(ctx context.Context, tx *sql.Tx, jobID, key string) (string, bool, error) {
	const q = `SELECT run_id FROM run_idempotency WHERE job_id = ? AND idempotency_key = ?`
	var runID string
	var err error
	if tx != nil {
		err = tx.QueryRowContext(ctx, q, jobID, key).Scan(&runID)
	} else {
		err = s.db.QueryRowContext(ctx, q, jobID, key).Scan(&runID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("runs: find idempotency key: %w", err)
	}
	return runID, true, nil
}

// RecordIdempotencyTx binds a caller's key to the run it produced, in the same
// transaction as that run. A duplicate insert is a conflict rather than an
// overwrite: the first delivery owns the key, and rewriting it would point a
// retried command at a second run.
func (s *Store) RecordIdempotencyTx(ctx context.Context, tx *sql.Tx, jobID, key, runID string, at time.Time) error {
	const q = `INSERT INTO run_idempotency (job_id, idempotency_key, run_id, created_at) VALUES (?, ?, ?, ?)`
	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, q, jobID, key, runID, database.FormatTime(at))
	} else {
		_, err = s.db.ExecContext(ctx, q, jobID, key, runID, database.FormatTime(at))
	}
	if err != nil {
		return fmt.Errorf("runs: record idempotency key: %w", err)
	}
	return nil
}

// DeleteByJob removes every durable trace of one job's runs:
// captured logs, queue rows and run records. It exists for `otter delete`,
// which purges an identity's history deliberately, and returns how many run
// records were removed.
func (s *Store) DeleteByJob(ctx context.Context, jobID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_logs WHERE run_id IN (SELECT id FROM runs WHERE job_id = ?)`,
		jobID); err != nil {
		return 0, fmt.Errorf("runs: delete logs for %s: %w", jobID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_queue WHERE job_id = ?`, jobID); err != nil {
		return 0, fmt.Errorf("runs: delete queue rows for %s: %w", jobID, err)
	}
	// A command key left behind would let a stale retry return a run id that no
	// longer exists, so offboarding removes it with the run it produced.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_idempotency WHERE job_id = ?`, jobID); err != nil {
		return 0, fmt.Errorf("runs: delete idempotency keys for %s: %w", jobID, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE job_id = ?`, jobID)
	if err != nil {
		return 0, fmt.Errorf("runs: delete for %s: %w", jobID, err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}

// CancelPinnedToRelease cancels the queued and retrying attempts of one
// job that are bound to a single release digest, and removes them from
// the queue.
//
// It exists for a quarantined release: an attempt submitted before the release
// was disabled already recorded the snapshot it would execute, so removing the
// activation pointer is not enough. Cancelling is the honest outcome -- the code
// was staged from a source the operator rejected -- and the error says so.
func (s *Store) CancelPinnedToRelease(ctx context.Context, jobID, digest, reason string) (int, error) {
	if digest == "" {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_queue WHERE run_id IN (
		     SELECT id FROM runs
		      WHERE job_id = ? AND release_digest = ? AND status IN ('queued', 'retrying'))`,
		jobID, digest); err != nil {
		return 0, fmt.Errorf("runs: unqueue quarantined attempts: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET status = 'cancelled', error = ?, finished_at = ?
		  WHERE job_id = ? AND release_digest = ? AND status IN ('queued', 'retrying')`,
		reason, database.FormatTime(time.Now().UTC()), jobID, digest)
	if err != nil {
		return 0, fmt.Errorf("runs: cancel quarantined attempts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
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
	JobID string
	// JobIDs matches runs whose job_id is any of the listed
	// values. It exists because a run's job_id is not stable across
	// the identity migration: older rows were keyed by the manifest label,
	// newer ones by the durable identity. A reference can only be answered by
	// matching both spellings in one query.
	JobIDs      []string
	Status      Status
	ParentRunID string
	Limit       int
	Offset      int
	Ascending   bool
}

// List returns runs matching the filter, newest first by default.
//
// It is a bounded display read. A f.Limit outside (0, maxListLimit] -- including
// one larger than maxListLimit -- is out of range and is replaced with
// defaultListLimit rather than reported as an error, so a caller cannot tell a
// truncated result from a complete one. That is intentional here, where "the
// newest 20 runs" wants a default; it is why a read that must be exhaustive,
// such as recovery, uses ListByStatusAll instead of a large magic limit.
//
// When a logger is attached, an explicitly supplied limit that is out of range
// is logged at debug level so the substitution is at least observable.
func (s *Store) List(ctx context.Context, f Filter) ([]*Run, error) {
	var (
		where []string
		args  []any
	)
	if f.JobID != "" {
		where = append(where, "job_id = ?")
		args = append(args, f.JobID)
	}
	if len(f.JobIDs) > 0 {
		placeholders := make([]string, len(f.JobIDs))
		for i, id := range f.JobIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = append(where, "job_id IN ("+strings.Join(placeholders, ", ")+")")
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
	if limit <= 0 || limit > maxListLimit {
		// An explicit limit above the ceiling is the dangerous case: the caller
		// asked for everything and is about to receive a page. Say so when a
		// logger is attached. A non-positive limit means "not specified", which
		// is the documented default and not worth a line.
		if limit > maxListLimit && s.log != nil {
			s.log.Debug("runs_list_limit_rejected",
				"requested", limit,
				"max", maxListLimit,
				"default", defaultListLimit)
		}
		limit = defaultListLimit
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
//
// Like List it is a bounded read: a limit outside (0, maxListLimit] silently
// becomes defaultListLimit. Callers that need every run in a status must use
// ListByStatusAll.
func (s *Store) ListByStatus(ctx context.Context, status Status, limit int) ([]*Run, error) {
	return s.List(ctx, Filter{Status: status, Limit: limit, Ascending: true})
}

// exhaustivePageSize is how many rows ListByStatusAll fetches per keyset page.
// It is a batching detail, not a ceiling: the read keeps paging until a short
// page proves the status is exhausted.
const exhaustivePageSize = 500

// ListByStatusAll returns every run in a status, oldest first, with no default
// ceiling. Completeness is the entire point of this read, which is what
// separates it from ListByStatus.
//
// It pages with a keyset cursor over (created_at, rowid) -- the same key the
// ascending order uses, so no row is skipped or visited twice. created_at is
// stored as a fixed-width UTC string, so its lexicographic order is its
// chronological order. rowid, not id, is the tiebreaker because that is what
// the ordering already uses.
//
// The full set is collected before it is returned. A caller that mutates status
// while iterating therefore cannot disturb the cursor: the key is read from the
// table, and status changes never touch created_at or rowid. Rows inserted
// concurrently with a later created_at are picked up; rows already seen are not
// revisited.
func (s *Store) ListByStatusAll(ctx context.Context, status Status) ([]*Run, error) {
	return s.listByStatusAll(ctx, status, exhaustivePageSize)
}

// listByStatusAll is ListByStatusAll with an injectable page size, so a test can
// force many page boundaries -- including one that falls in the middle of a
// group of rows sharing a created_at.
func (s *Store) listByStatusAll(ctx context.Context, status Status, pageSize int) ([]*Run, error) {
	if pageSize <= 0 {
		pageSize = exhaustivePageSize
	}

	var out []*Run
	var afterCreatedAt string
	var afterRowID int64

	for {
		page, err := s.listByStatusPage(ctx, status, afterCreatedAt, afterRowID, pageSize)
		if err != nil {
			return nil, err
		}
		for _, row := range page {
			out = append(out, row.run)
		}
		if len(page) < pageSize {
			return out, nil
		}

		// Advance from the last row of the page just read, never from a row the
		// caller has since mutated.
		last := page[len(page)-1]
		afterCreatedAt = database.FormatTime(last.run.CreatedAt)
		afterRowID = last.rowID
	}
}

// runRow is a run together with the rowid that keys the pagination cursor.
type runRow struct {
	run   *Run
	rowID int64
}

// listByStatusPage reads one keyset page strictly after (afterCreatedAt,
// afterRowID). An empty afterCreatedAt means "from the beginning".
func (s *Store) listByStatusPage(ctx context.Context, status Status, afterCreatedAt string, afterRowID int64, pageSize int) ([]runRow, error) {
	if pageSize <= 0 {
		pageSize = 1
	}

	query := `SELECT rowid, ` + runColumns + ` FROM runs WHERE status = ?`
	args := []any{string(status)}
	if afterCreatedAt != "" {
		query += ` AND (created_at > ? OR (created_at = ? AND rowid > ?))`
		args = append(args, afterCreatedAt, afterCreatedAt, afterRowID)
	}
	query += ` ORDER BY created_at ASC, rowid ASC LIMIT ?`
	args = append(args, pageSize)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("runs: list by status page: %w", err)
	}
	defer rows.Close()

	page := make([]runRow, 0, pageSize)
	for rows.Next() {
		var rowID int64
		run, err := scanRunRow(rows, &rowID)
		if err != nil {
			return nil, fmt.Errorf("runs: list by status page scan: %w", err)
		}
		page = append(page, runRow{run: run, rowID: rowID})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs: list by status page: %w", err)
	}
	return page, nil
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

// LastSuccessByJob returns, for every job with at least one succeeded run, the
// completion time of its most recent success. It is the daemon-side answer to
// "did this job stop succeeding?" -- a question the per-status counts cannot
// answer, because a job that has been failing for a day still shows the same
// healthy totals.
//
// A job with no success is absent rather than present with a zero time, so a
// caller can tell "never succeeded" from "succeeded at the epoch".
func (s *Store) LastSuccessByJob(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id, MAX(finished_at) FROM runs
		  WHERE status = ? AND finished_at IS NOT NULL
		  GROUP BY job_id`, string(StatusSucceeded))
	if err != nil {
		return nil, fmt.Errorf("runs: last success by job: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var (
			jobID    string
			finished database.NullableTime
		)
		if err := rows.Scan(&jobID, &finished); err != nil {
			return nil, fmt.Errorf("runs: last success by job scan: %w", err)
		}
		if finished.Valid {
			out[jobID] = finished.Time
		}
	}
	return out, rows.Err()
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
	return s.FinishTx(ctx, nil, id, f)
}

// FinishTx writes the terminal state of a run, optionally inside an existing
// transaction. It is what lets a terminal write and the successor attempt it
// implies be committed together, so a crash can never leave one without the
// other.
func (s *Store) FinishTx(ctx context.Context, tx *sql.Tx, id string, f Finish) error {
	if !f.Status.Terminal() {
		return fmt.Errorf("runs: finish %s: status %q is not terminal", id, f.Status)
	}
	if f.FinishedAt.IsZero() {
		f.FinishedAt = time.Now().UTC()
	}

	const q = `UPDATE runs SET status = ?, finished_at = ?, exit_code = ?, error = ?
		 WHERE id = ?`
	args := []any{
		string(f.Status), database.FormatTime(f.FinishedAt),
		database.NullableInt(f.ExitCode), database.NullableString(f.Error), id,
	}

	var (
		res sql.Result
		err error
	)
	if tx != nil {
		res, err = tx.ExecContext(ctx, q, args...)
	} else {
		res, err = s.db.ExecContext(ctx, q, args...)
	}
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
	return scanRunRow(sc, nil)
}

// scanRunRow scans a run, optionally preceded by the rowid column. The
// exhaustive read's keyset cursor advances on that rowid, so it has to be read
// with the row rather than reconstructed from the run's fields.
func scanRunRow(sc interface{ Scan(...any) error }, rowID *int64) (*Run, error) {
	var (
		r                 Run
		status            string
		parent            sql.NullString
		created           database.NullableTime
		started           database.NullableTime
		finished          database.NullableTime
		exitCode          sql.NullInt64
		errMsg            sql.NullString
		meta              sql.NullString
		pythonMode        string
		pythonVersion     string
		environmentDigest string
		pythonPolicy      string
		releaseDigest     string
		releaseSourceDir  string
		sdkVersion        string
		jobName           string
		jobGen            int64
		capturePolicy     string
		scheduleID        sql.NullString
		configVersion     sql.NullString
	)

	dest := []any{
		&r.ID, &r.JobID, &r.TriggerType, &status, &r.Attempt,
		&parent, &created, &started, &finished, &exitCode, &errMsg, &meta,
		&pythonMode, &pythonVersion, &environmentDigest, &pythonPolicy,
		&releaseDigest, &releaseSourceDir, &sdkVersion,
		&jobName, &jobGen, &capturePolicy, &scheduleID, &configVersion,
	}
	if rowID != nil {
		dest = append([]any{rowID}, dest...)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}

	r.Status = Status(status)
	r.PythonMode, r.PythonVersion, r.EnvironmentDigest, r.PythonPolicy, r.SDKVersion =
		pythonMode, pythonVersion, environmentDigest, pythonPolicy, sdkVersion
	r.ReleaseDigest, r.ReleaseSourceDir = releaseDigest, releaseSourceDir
	r.JobName, r.JobGeneration = jobName, jobGen
	r.CapturePolicy = capturePolicy
	r.ScheduleID = scheduleID.String
	r.ConfigVersion = configVersion.String
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
