package runs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// RetentionBatchChains is how many retry chains one Store.ExpiredRunIDs call
// considers. It is a batching detail, not a ceiling: a sweep calls the read
// again until it returns nothing, so one query never holds the database's
// single connection for long.
const RetentionBatchChains = 50

// LogRetentionBatch is how many log rows one LogStore.DeleteOlderThan call
// removes. Like the chain batch it bounds a single statement; a sweep repeats
// the call until it removes nothing.
const LogRetentionBatch = 1000

// chainCTE maps every run to the root of its retry chain.
//
// A run is a root when it has no parent or when its parent row is gone. Chains
// are linear -- each attempt has at most one successor -- so the mapping is a
// single recursive descent from the roots. A parent cycle has no root, so its
// rows never appear in the mapping and can never be pruned; that is the
// fail-closed choice for corrupt data.
const chainCTE = `
WITH RECURSIVE chain(run_id, root_id) AS (
    SELECT r.id, r.id
      FROM runs r
     WHERE r.parent_run_id IS NULL
        OR NOT EXISTS (SELECT 1 FROM runs p WHERE p.id = r.parent_run_id)
    UNION ALL
    SELECT c.id, chain.root_id
      FROM runs c
      JOIN chain ON c.parent_run_id = chain.run_id
)`

// terminalSQL renders the terminal statuses as a SQL list. It is derived from
// Status.Terminal rather than spelled out, so a status added later is treated
// as live -- and therefore never pruned -- until it is deliberately made
// terminal.
func terminalSQL() string {
	quoted := make([]string, 0, len(AllStatuses()))
	for _, status := range AllStatuses() {
		if status.Terminal() {
			quoted = append(quoted, "'"+string(status)+"'")
		}
	}
	return strings.Join(quoted, ", ")
}

// ExpiredRunIDs returns every run of up to maxChains retry chains that are
// entirely terminal and whose newest attempt was created before cutoff.
//
// A chain is returned whole or not at all, so a caller that deletes the result
// never unpicks one attempt of a chain while leaving another behind. The
// newest attempt is what dates the chain: a chain whose latest retry is recent
// stays entirely, even when its root is old.
//
// Non-terminal work is never returned: an attempt in queued, running or
// retrying state makes its entire chain ineligible, because its logs may still
// be written and its release binding must survive.
func (s *Store) ExpiredRunIDs(ctx context.Context, cutoff time.Time, maxChains int) ([]string, error) {
	if maxChains <= 0 {
		maxChains = RetentionBatchChains
	}

	query := fmt.Sprintf(`%s,
		expired(root_id) AS (
		    SELECT chain.root_id
		      FROM chain
		      JOIN runs r ON r.id = chain.run_id
		     GROUP BY chain.root_id
		    HAVING MIN(CASE WHEN r.status IN (%s) THEN 1 ELSE 0 END) = 1
		       AND MAX(r.created_at) < ?
		     ORDER BY MAX(r.created_at) ASC
		     LIMIT ?
		)
		SELECT chain.run_id
		  FROM chain
		  JOIN expired ON expired.root_id = chain.root_id
		 ORDER BY chain.run_id`, chainCTE, terminalSQL())

	rows, err := s.db.QueryContext(ctx, query, database.FormatTime(cutoff), maxChains)
	if err != nil {
		return nil, fmt.Errorf("runs: list expired runs: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("runs: scan expired run: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs: list expired runs: %w", err)
	}
	return ids, nil
}

// DeleteRuns removes the named runs together with their captured logs, in one
// transaction, and returns how many run rows were removed.
//
// It is the run-retention path, so it deletes only terminal runs: a non-terminal
// run or a run in a chain that still has a live attempt is left untouched even
// if it was named. Logs are deleted in the same transaction as the run rows,
// because run_logs declares no foreign key and a run deleted on its own would
// otherwise orphan its output silently.
//
// Capture rows are not touched here. The daemon removes them before calling
// this, per run, because that order is idempotent: a batch that fails after
// its capture was deleted is simply re-run on the next sweep.
func (s *Store) DeleteRuns(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("runs: begin retire: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	list := strings.Join(placeholders, ", ")
	filter := fmt.Sprintf("id IN (%s) AND status IN (%s)", list, terminalSQL())

	// Logs first: once the run rows are gone the subquery can no longer find
	// them, which is exactly the orphaning this guards against.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_logs WHERE run_id IN (SELECT id FROM runs WHERE `+filter+`)`,
		args...); err != nil {
		return 0, fmt.Errorf("runs: delete logs for retired runs: %w", err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE `+filter, args...)
	if err != nil {
		return 0, fmt.Errorf("runs: delete retired runs: %w", err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("runs: count retired runs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runs: commit retire: %w", err)
	}
	return removed, nil
}

// deleteOlderThanQuery is the log-retention statement. It removes the logs of
// settled chains whose newest attempt predates the cutoff, at most one batch at
// a time. It is a function, not an inline literal, so a test can inspect the
// statement without opening a database.
func deleteOlderThanQuery() string {
	return fmt.Sprintf(`%s,
		expired(root_id) AS (
		    SELECT chain.root_id
		      FROM chain
		      JOIN runs r ON r.id = chain.run_id
		     GROUP BY chain.root_id
		    HAVING MIN(CASE WHEN r.status IN (%s) THEN 1 ELSE 0 END) = 1
		       AND MAX(r.created_at) < ?
		)
		DELETE FROM run_logs
		 WHERE id IN (
		     SELECT l.id
		       FROM run_logs l
		       JOIN chain ON chain.run_id = l.run_id
		       JOIN expired ON expired.root_id = chain.root_id
		      ORDER BY l.id ASC
		      LIMIT ?
		 )`, chainCTE, terminalSQL())
}

// Compile-time guard that the retention sweep and the prune primitive cannot
// drift apart silently: DeleteOlderThan is the only production caller-shaped
// entry point for the log window.
var _ func(context.Context, time.Time) (int64, error) = (&LogStore{}).DeleteOlderThan
