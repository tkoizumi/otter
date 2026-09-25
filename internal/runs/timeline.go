package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// LogCursor is a position in the run timeline, expressed for a query against one
// source. It is a lexicographic bound: the query must return exactly the rows
// the merged order places after (At, Rank, ID).
//
// Rank is the source rank of the cursor's own event, not of the table being
// queried. That distinction is the whole reason this carries a rank: a log row
// recorded at the same instant as a *higher-ranked* cursor position still sorts
// after it, while a log row at the same instant as a log cursor is only eligible
// when its id is larger.
type LogCursor struct {
	At   time.Time
	Rank int
	// ID is the id of the cursor's own event. It breaks the tie only for the
	// table the cursor came from; Rank = -1 marks "nothing emitted yet".
	ID int64
}

// Source ranks used by the merged timeline order. They live here as well as in
// internal/timeline so a paged read can compare the cursor's rank against the
// rank of the table it is querying without the stores importing the timeline
// package. The values are part of the ordering contract; changing them changes
// the meaning of a cursor.
const (
	RankLogs = 0
	RankHTTP = 1
)

// TimelineRun is the run context the timeline header needs, and nothing more.
//
// It is a deliberate projection rather than the whole Run: Run carries Metadata,
// which holds the raw trigger body and headers exactly as submitted. Those are
// not sanitized to the standard the HTTP inspection contract requires, so they
// must not travel through the timeline response or its cursor. A projection also
// means a field added to Run later cannot silently appear here.
type TimelineRun struct {
	ID                    string
	IntegrationID         string
	IntegrationName       string
	Status                Status
	Attempt               int
	ParentRunID           *string
	TriggerType           string
	Error                 *string
	ExitCode              *int
	ReleaseDigest         string
	CreatedAt             time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
	CapturePolicy         string
	EnvironmentDigest     string
	IntegrationGeneration int64
}

// TimelineRunTx reads the timeline projection for one run inside a caller's
// transaction, so it observes the same snapshot as the rest of the page.
//
// It returns sql.ErrNoRows for an unknown run; callers map that to their own
// not-found error.
func (s *Store) TimelineRunTx(ctx context.Context, tx *sql.Tx, runID string) (*TimelineRun, error) {
	var (
		r          TimelineRun
		status     string
		parent     sql.NullString
		errMsg     sql.NullString
		exitCode   sql.NullInt64
		created    database.NullableTime
		started    database.NullableTime
		finished   database.NullableTime
		generation sql.NullInt64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, integration_id, integration_name, status, attempt, parent_run_id,
		        trigger_type, error, exit_code, release_digest, created_at, started_at,
		        finished_at, capture_policy, environment_digest, integration_generation
		   FROM runs WHERE id = ?`, runID).
		Scan(&r.ID, &r.IntegrationID, &r.IntegrationName, &status, &r.Attempt, &parent,
			&r.TriggerType, &errMsg, &exitCode, &r.ReleaseDigest, &created, &started,
			&finished, &r.CapturePolicy, &r.EnvironmentDigest, &generation)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("runs: read timeline context for %s: %w", runID, err)
	}

	r.Status = Status(status)
	if parent.Valid {
		value := parent.String
		r.ParentRunID = &value
	}
	if errMsg.Valid {
		value := errMsg.String
		r.Error = &value
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		r.ExitCode = &value
	}
	if created.Valid {
		r.CreatedAt = created.Time
	}
	if started.Valid {
		value := started.Time
		r.StartedAt = &value
	}
	if finished.Valid {
		value := finished.Time
		r.FinishedAt = &value
	}
	if generation.Valid {
		r.IntegrationGeneration = generation.Int64
	}
	return &r, nil
}

// LogEvidence identifies the extent of a run's log rows without reading any of
// them.
//
// IDs are assigned by the database and never reused: run_logs declares
// INTEGER PRIMARY KEY AUTOINCREMENT, so sqlite_sequence keeps the high-water
// mark even after every row is deleted. That is what lets this pair stand in for
// a row count.
type LogEvidence struct {
	MinID int64
	MaxID int64
	// Present reports whether the run has any log rows at all.
	Present bool
}

// LogEvidenceTx reports the extent of a run's logs using two covering index
// seeks. It never loads a message.
//
// The pair detects every mutation the log store can perform: appending raises
// MaxID, and the only deletion paths (DeleteForRun, DeleteOlderThan and the
// per-integration cascade) remove whole rows, moving MinID or clearing both.
// Log rows are never updated. An in-place edit of a row, or any external write
// to the database, is outside what this revision claims to detect.
func (s *LogStore) LogEvidenceTx(ctx context.Context, tx *sql.Tx, runID string) (LogEvidence, error) {
	var (
		minID sql.NullInt64
		maxID sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(id), MAX(id) FROM run_logs WHERE run_id = ?`, runID).
		Scan(&minID, &maxID); err != nil {
		return LogEvidence{}, fmt.Errorf("runs: read log evidence for %s: %w", runID, err)
	}
	if !minID.Valid || !maxID.Valid {
		return LogEvidence{}, nil
	}
	return LogEvidence{MinID: minID.Int64, MaxID: maxID.Int64, Present: true}, nil
}

// LogPageTx reads one chronological page of a run's logs inside a caller's
// transaction, returning the rows the merged order places strictly after the
// cursor position.
//
// The predicate is the literal tuple comparison for this table's rank (0):
//
//	(timestamp, 0, id) > (cursor.At, cursor.Rank, cursor.ID)
//
// which is "later timestamp, or the same timestamp and a rank above the
// cursor's, or the same timestamp and the same rank and a larger id".
func (s *LogStore) LogPageTx(ctx context.Context, tx *sql.Tx, runID string, cursor LogCursor, limit int) ([]LogEntry, error) {
	if limit <= 0 {
		return nil, nil
	}

	// The equal-timestamp clauses are grouped explicitly: without the
	// parentheses the trailing ORDER BY would attach to the second operand of an
	// OR and the run_id filter would be bypassed.
	query := `SELECT id, run_id, timestamp, stream, message, origin FROM run_logs
	           WHERE run_id = ? AND (timestamp > ?`
	args := []any{runID, database.FormatTime(cursor.At)}
	switch {
	case cursor.Rank < RankLogs:
		// Nothing has been emitted yet: every row at this timestamp is eligible.
		query += ` OR timestamp = ?`
		args = append(args, database.FormatTime(cursor.At))
	case cursor.Rank == RankLogs:
		query += ` OR (timestamp = ? AND id > ?)`
		args = append(args, database.FormatTime(cursor.At), cursor.ID)
	}
	query += `) ORDER BY timestamp ASC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("runs: read log page for %s: %w", runID, err)
	}
	defer rows.Close()

	out := make([]LogEntry, 0, 16)
	for rows.Next() {
		var (
			e  LogEntry
			ts database.NullableTime
		)
		if err := rows.Scan(&e.ID, &e.RunID, &ts, &e.Stream, &e.Message, &e.Origin); err != nil {
			return nil, fmt.Errorf("runs: scan log page for %s: %w", runID, err)
		}
		if ts.Valid {
			e.Timestamp = ts.Time
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
