package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/schedule"
)

// foldableStatuses are the run states a coalesced occurrence may fold into:
// accepted, not yet executing. A running attempt is deliberately excluded --
// folding into it would mean editing a run that is already using its metadata.
// When only a running attempt exists, the occurrence becomes the next pending
// run instead, which still leaves exactly one waiting.
func foldableStatuses() []string {
	return []string{string(runs.StatusQueued), string(runs.StatusRetrying)}
}

// countedStatuses are the states that count against a job's max_queue_depth:
// accepted and unfinished. That is the queued-plus-running population the
// contract names, not just the wait queue -- a job whose runs are all executing
// is as saturated as one whose runs are all waiting, and bounding only the wait
// queue would let a schedule pile work onto a job that has not started any of
// it.
func countedStatuses() []string {
	return []string{string(runs.StatusQueued), string(runs.StatusRunning), string(runs.StatusRetrying)}
}

// pendingCount is how many of a job's runs are accepted and unfinished.
func (d *Daemon) pendingCount(ctx context.Context, jobID string) (int, error) {
	statuses := countedStatuses()
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE job_id = ? AND status IN (?, ?, ?)`,
		jobID, statuses[0], statuses[1], statuses[2]).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending runs for %s: %w", jobID, err)
	}
	return n, nil
}

// pendingRunFor returns the run a coalesced occurrence should fold into: the
// oldest accepted-but-unstarted run of this job, or false when there is none.
//
// Oldest first, because the run that has been waiting longest is the one the
// schedule's backlog is attached to, and folding into the newest would keep
// resetting the same slot while the real backlog aged.
func (d *Daemon) pendingRunFor(ctx context.Context, jobID string) (runID string, metadata json.RawMessage, ok bool, err error) {
	statuses := foldableStatuses()
	row := d.db.QueryRowContext(ctx,
		`SELECT id, metadata FROM runs
		  WHERE job_id = ? AND status IN (?, ?)
		  ORDER BY created_at ASC, rowid ASC
		  LIMIT 1`,
		jobID, statuses[0], statuses[1])
	var meta sql.NullString
	if err := row.Scan(&runID, &meta); err != nil {
		if err == sql.ErrNoRows {
			return "", nil, false, nil
		}
		return "", nil, false, fmt.Errorf("find a pending run for %s: %w", jobID, err)
	}
	if meta.Valid {
		metadata = json.RawMessage(meta.String)
	}
	return runID, metadata, true, nil
}

// foldOccurrence records a coalesced occurrence against a run that is already
// waiting, instead of creating a second one.
//
// It is one transaction, for the same reason a fire is: the ledger row and the
// run's updated accounting commit together, so a crash cannot leave an
// occurrence recorded against a run that does not mention it, or a run claiming
// an occurrence the ledger will replay later. The ledger's primary key is what
// makes a duplicate tick a no-op rather than a double count.
//
// It reports false when the occurrence was already recorded, which is the
// duplicate-tick case and not an error: the tick lost the race it was supposed
// to lose.
func (d *Daemon) foldOccurrence(ctx context.Context, rec schedule.Schedule, occurrence time.Time, prev json.RawMessage, runID string) (bool, error) {
	at := occurrence.UTC()
	meta, err := addCoalescedOccurrence(prev, at)
	if err != nil {
		return false, err
	}

	recorded := false
	err = d.db.Tx(ctx, func(tx *sql.Tx) error {
		ok, err := d.schedules.RecordFireTx(ctx, tx, rec.ID, occurrence, runID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		recorded = true
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET metadata = ? WHERE id = ?`,
			database.NullableString(string(meta)), runID); err != nil {
			return fmt.Errorf("record coalesced occurrence on run %s: %w", runID, err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if recorded {
		d.schedules.NoteFired(rec.ID, occurrence)
		// One occurrence folded, and the run it folded into now stands for one
		// more than it did. Counted here rather than at the call site so a
		// duplicate tick, which records nothing, cannot inflate it.
		d.schedules.NoteCoalesced(rec.ID, 1)
	}
	return recorded, nil
}

// addCoalescedOccurrence extends a run's trigger metadata with the window of
// occurrences it has absorbed. The shape is stable and additive: a reader sees
// `coalesced.count`, the number of occurrences standing behind the one run, and
// the window they span, so a run that represents five occurrences says so
// rather than looking like a single ordinary fire.
func addCoalescedOccurrence(prev json.RawMessage, at time.Time) (json.RawMessage, error) {
	meta := map[string]any{}
	if len(prev) > 0 {
		if err := json.Unmarshal(prev, &meta); err != nil {
			return nil, fmt.Errorf("read run metadata: %w", err)
		}
	}
	coalesced, _ := meta["coalesced"].(map[string]any)
	if coalesced == nil {
		coalesced = map[string]any{}
	}
	count := 0
	switch n := coalesced["count"].(type) {
	case float64:
		count = int(n)
	case int:
		count = n
	}
	count++
	coalesced["count"] = count
	coalesced["last"] = at.UTC().Format(time.RFC3339Nano)
	if _, ok := coalesced["first"]; !ok {
		coalesced["first"] = at.UTC().Format(time.RFC3339Nano)
	}
	meta["coalesced"] = coalesced

	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode run metadata: %w", err)
	}
	return encoded, nil
}
