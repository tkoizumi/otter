// Package queue implements Otter's durable run queue.
//
// The queue lives in SQLite next to the run records, so a queued run survives
// a daemon restart or a machine reboot. Claiming is a single transaction: a
// run is removed from the queue and its worker slot reserved atomically.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// ErrEmpty is returned by Claim when no run is currently claimable.
var ErrEmpty = errors.New("queue is empty")

// Item is a queued run.
type Item struct {
	RunID       string    `json:"run_id"`
	JobID       string    `json:"job_id"`
	AvailableAt time.Time `json:"available_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// Capacity reserves execution slots for a job. Reserve must be
// atomic: it returns false when the job is already at its
// concurrency limit. Release gives the slot back.
//
// Keeping this as an interface lets the queue enforce per-job
// concurrency without knowing anything about the daemon.
type Capacity interface {
	Reserve(jobID string) bool
	Release(jobID string)
}

// Queue is the SQLite-backed run queue.
type Queue struct {
	db *sql.DB
}

// New wraps a database handle.
func New(db *sql.DB) *Queue { return &Queue{db: db} }

// Enqueue adds a run to the queue.
func (q *Queue) Enqueue(ctx context.Context, runID, jobID string, availableAt time.Time) error {
	return q.EnqueueTx(ctx, nil, runID, jobID, availableAt)
}

// EnqueueTx adds a run to the queue, optionally inside an existing
// transaction so that creating a run record and enqueueing it are atomic.
func (q *Queue) EnqueueTx(ctx context.Context, tx *sql.Tx, runID, jobID string, availableAt time.Time) error {
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	createdAt := time.Now().UTC()

	const query = `INSERT INTO run_queue (run_id, job_id, available_at, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			job_id = excluded.job_id,
			available_at   = excluded.available_at`

	args := []any{runID, jobID, database.FormatTime(availableAt), database.FormatTime(createdAt)}

	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, query, args...)
	} else {
		_, err = q.db.ExecContext(ctx, query, args...)
	}
	if err != nil {
		return fmt.Errorf("queue: enqueue %s: %w", runID, err)
	}
	return nil
}

// Remove deletes a run from the queue. It reports whether a row was removed.
func (q *Queue) Remove(ctx context.Context, runID string) (bool, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM run_queue WHERE run_id = ?`, runID)
	if err != nil {
		return false, fmt.Errorf("queue: remove %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue: remove %s: %w", runID, err)
	}
	return n == 1, nil
}

// Claim atomically takes the oldest available run whose job has spare
// concurrency capacity, reserving a slot for it. When nothing is claimable it
// returns ErrEmpty.
//
// The whole operation runs in one immediate transaction, so a run can never
// be handed to two workers.
//
// A reservation is only handed to the caller once the transaction that removes
// the run has committed. Every other outcome -- an in-transaction failure, a
// conflict, or a failed commit -- gives the reserved slots back, because a
// reservation the caller never learns about would shrink the job's
// capacity for the rest of the process's life.
func (q *Queue) Claim(ctx context.Context, now time.Time, capacity Capacity) (*Item, error) {
	var (
		claimed *Item
		// held records every slot reserved by this attempt, in order, so the
		// cleanup below can give back exactly what was taken.
		held      []string
		committed bool
	)

	// The transaction removes the run from the queue and the caller must not
	// learn about a run that was never really removed. If any part of the
	// transaction fails -- including the commit -- the reservations that were
	// granted while it ran are handed back; a reservation the caller never
	// learns about would shrink this job's capacity for the rest of
	// the process's life.
	defer func() {
		if committed || capacity == nil {
			return
		}
		for i := len(held) - 1; i >= 0; i-- {
			capacity.Release(held[i])
		}
	}()

	ok, err := q.withTx(ctx, commitQueueTx, func(tx *sql.Tx) (bool, error) {
		rows, err := tx.QueryContext(ctx,
			`SELECT run_id, job_id, available_at, created_at FROM run_queue
			 WHERE available_at <= ?
			 ORDER BY available_at ASC, created_at ASC, run_id ASC
			 LIMIT 200`,
			database.FormatTime(now))
		if err != nil {
			return false, fmt.Errorf("queue: select candidates: %w", err)
		}

		candidates, err := scanItems(rows)
		if err != nil {
			return false, err
		}

		for _, it := range candidates {
			reserved := capacity != nil && capacity.Reserve(it.JobID)
			if reserved {
				held = append(held, it.JobID)
			} else if capacity != nil {
				continue // job is at its concurrency limit or draining
			}

			res, err := tx.ExecContext(ctx, `DELETE FROM run_queue WHERE run_id = ?`, it.RunID)
			if err != nil {
				return false, fmt.Errorf("queue: claim %s: %w", it.RunID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return false, fmt.Errorf("queue: claim %s: %w", it.RunID, err)
			}
			if n != 1 {
				// Someone else got there first; give this slot back now and
				// keep looking at the remaining candidates.
				if reserved {
					capacity.Release(it.JobID)
					held = held[:len(held)-1]
				}
				continue
			}

			item := it
			claimed = &item
			return true, nil
		}
		return false, nil
	})

	// The transaction committed, so the run really is gone from the queue and
	// the caller owns the reserved slots. Any other outcome -- a failed query,
	// a failed commit, or nothing claimable -- leaves the reservations to the
	// cleanup above.
	committed = ok
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return nil, ErrEmpty
	}
	return claimed, nil
}

// Contains reports whether a run is currently queued.
func (q *Queue) Contains(ctx context.Context, runID string) (bool, error) {
	var one int
	err := q.db.QueryRowContext(ctx, `SELECT 1 FROM run_queue WHERE run_id = ?`, runID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("queue: contains %s: %w", runID, err)
	}
	return true, nil
}

// Depth returns the number of runs waiting in the queue.
func (q *Queue) Depth(ctx context.Context) (int, error) {
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_queue`).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue: depth: %w", err)
	}
	return n, nil
}

// DepthByJob returns the queue depth per job.
func (q *Queue) DepthByJob(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT job_id, COUNT(*) FROM run_queue GROUP BY job_id`)
	if err != nil {
		return nil, fmt.Errorf("queue: depth by job: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var (
			id string
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("queue: depth by job scan: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// OldestWaiting reports the submission time of the oldest queued run that is
// claimable now, and whether there is one.
//
// "Claimable now" is available_at <= now: a run parked for retry backoff is
// deliberately not counted, because it is waiting on a clock rather than on
// capacity. Retry activity is reported separately (see NextRetryAt), so a queue
// blocked by its workers cannot be confused with one that is merely between
// attempts.
func (q *Queue) OldestWaiting(ctx context.Context, now time.Time) (time.Time, bool, error) {
	var raw sql.NullString
	if err := q.db.QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM run_queue WHERE available_at <= ?`,
		database.FormatTime(now)).Scan(&raw); err != nil {
		return time.Time{}, false, fmt.Errorf("queue: oldest waiting: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return time.Time{}, false, nil
	}
	at, err := database.ParseTime(raw.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("queue: oldest waiting: %w", err)
	}
	return at, true, nil
}

// NextRetryAt reports when the next queued run whose run status is status
// becomes claimable, and whether any run is. The daemon passes the retrying
// status, so the answer is "when the next retry is due" rather than "when the
// queue next moves".
func (q *Queue) NextRetryAt(ctx context.Context, status string) (time.Time, bool, error) {
	var raw sql.NullString
	if err := q.db.QueryRowContext(ctx,
		`SELECT MIN(q.available_at) FROM run_queue q
		   JOIN runs r ON r.id = q.run_id
		  WHERE r.status = ?`, status).Scan(&raw); err != nil {
		return time.Time{}, false, fmt.Errorf("queue: next retry: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return time.Time{}, false, nil
	}
	at, err := database.ParseTime(raw.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("queue: next retry: %w", err)
	}
	return at, true, nil
}

// List returns every queued run, oldest first. It is intended for
// observability and tests rather than for hot paths.
func (q *Queue) List(ctx context.Context) ([]Item, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT run_id, job_id, available_at, created_at FROM run_queue
		 ORDER BY available_at ASC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("queue: list: %w", err)
	}
	defer rows.Close()
	return scanItems(rows)
}

// DiscardAll empties the queue. Only used by tests and by explicit
// administrative resets.
func (q *Queue) DiscardAll(ctx context.Context) (int64, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM run_queue`)
	if err != nil {
		return 0, fmt.Errorf("queue: discard all: %w", err)
	}
	return res.RowsAffected()
}

// commitQueueTx is the production commit step. It exists as a variable so
// tests can inject a commit failure, which is otherwise impossible to trigger
// against a healthy SQLite file and is exactly the failure that used to leak
// capacity slots.
var commitQueueTx = func(tx *sql.Tx) error { return tx.Commit() }

// withTx runs fn inside a transaction using explicit BEGIN IMMEDIATE
// semantics supplied by the driver's _txlock setting. fn reports whether it
// wants the transaction committed; when it does not, the transaction is rolled
// back and withTx reports ok=false.
//
// The commit is performed by the caller-supplied commit function so that a
// failed commit is treated as a transaction failure rather than as an error
// the caller has to interpret.
func (q *Queue) withTx(ctx context.Context, commit func(tx *sql.Tx) error, fn func(tx *sql.Tx) (bool, error)) (ok bool, err error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("queue: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	wantCommit, err := fn(tx)
	if err != nil {
		return false, err
	}
	if !wantCommit {
		return false, nil
	}
	if err := commit(tx); err != nil {
		return false, fmt.Errorf("queue: commit: %w", err)
	}
	return true, nil
}

func scanItems(rows *sql.Rows) ([]Item, error) {
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var (
			it          Item
			availableAt database.NullableTime
			createdAt   database.NullableTime
		)
		if err := rows.Scan(&it.RunID, &it.JobID, &availableAt, &createdAt); err != nil {
			return nil, fmt.Errorf("queue: scan item: %w", err)
		}
		it.AvailableAt = availableAt.Time
		it.CreatedAt = createdAt.Time
		out = append(out, it)
	}
	return out, rows.Err()
}
