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
	RunID         string    `json:"run_id"`
	IntegrationID string    `json:"integration_id"`
	AvailableAt   time.Time `json:"available_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// Capacity reserves execution slots for an integration. Reserve must be
// atomic: it returns false when the integration is already at its
// concurrency limit. Release gives the slot back.
//
// Keeping this as an interface lets the queue enforce per-integration
// concurrency without knowing anything about the daemon.
type Capacity interface {
	Reserve(integrationID string) bool
	Release(integrationID string)
}

// Queue is the SQLite-backed run queue.
type Queue struct {
	db *sql.DB
}

// New wraps a database handle.
func New(db *sql.DB) *Queue { return &Queue{db: db} }

// Enqueue adds a run to the queue.
func (q *Queue) Enqueue(ctx context.Context, runID, integrationID string, availableAt time.Time) error {
	return q.EnqueueTx(ctx, nil, runID, integrationID, availableAt)
}

// EnqueueTx adds a run to the queue, optionally inside an existing
// transaction so that creating a run record and enqueueing it are atomic.
func (q *Queue) EnqueueTx(ctx context.Context, tx *sql.Tx, runID, integrationID string, availableAt time.Time) error {
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	createdAt := time.Now().UTC()

	const query = `INSERT INTO run_queue (run_id, integration_id, available_at, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			integration_id = excluded.integration_id,
			available_at   = excluded.available_at`

	args := []any{runID, integrationID, database.FormatTime(availableAt), database.FormatTime(createdAt)}

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

// Claim atomically takes the oldest available run whose integration has spare
// concurrency capacity, reserving a slot for it. When nothing is claimable it
// returns ErrEmpty.
//
// The whole operation runs in one immediate transaction, so a run can never
// be handed to two workers.
func (q *Queue) Claim(ctx context.Context, now time.Time, capacity Capacity) (*Item, error) {
	var claimed *Item

	err := q.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT run_id, integration_id, available_at, created_at FROM run_queue
			 WHERE available_at <= ?
			 ORDER BY available_at ASC, created_at ASC, run_id ASC
			 LIMIT 200`,
			database.FormatTime(now))
		if err != nil {
			return fmt.Errorf("queue: select candidates: %w", err)
		}

		candidates, err := scanItems(rows)
		if err != nil {
			return err
		}

		for _, it := range candidates {
			if capacity != nil && !capacity.Reserve(it.IntegrationID) {
				continue // integration is at its concurrency limit or draining
			}

			res, err := tx.ExecContext(ctx, `DELETE FROM run_queue WHERE run_id = ?`, it.RunID)
			if err != nil {
				if capacity != nil {
					capacity.Release(it.IntegrationID)
				}
				return fmt.Errorf("queue: claim %s: %w", it.RunID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				if capacity != nil {
					capacity.Release(it.IntegrationID)
				}
				return fmt.Errorf("queue: claim %s: %w", it.RunID, err)
			}
			if n != 1 {
				// Someone else got there first; give the slot back.
				if capacity != nil {
					capacity.Release(it.IntegrationID)
				}
				continue
			}

			item := it
			claimed = &item
			return nil
		}
		return nil
	})
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

// DepthByIntegration returns the queue depth per integration.
func (q *Queue) DepthByIntegration(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT integration_id, COUNT(*) FROM run_queue GROUP BY integration_id`)
	if err != nil {
		return nil, fmt.Errorf("queue: depth by integration: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var (
			id string
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("queue: depth by integration scan: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// List returns every queued run, oldest first. It is intended for
// observability and tests rather than for hot paths.
func (q *Queue) List(ctx context.Context) ([]Item, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT run_id, integration_id, available_at, created_at FROM run_queue
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

// withTx runs fn inside a transaction using explicit BEGIN IMMEDIATE
// semantics supplied by the driver's _txlock setting.
func (q *Queue) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("queue: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("queue: commit: %w", err)
	}
	return nil
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
		if err := rows.Scan(&it.RunID, &it.IntegrationID, &availableAt, &createdAt); err != nil {
			return nil, fmt.Errorf("queue: scan item: %w", err)
		}
		it.AvailableAt = availableAt.Time
		it.CreatedAt = createdAt.Time
		out = append(out, it)
	}
	return out, rows.Err()
}
