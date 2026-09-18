package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// Log streams stored in run_logs.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamOtter  = "otter"
)

// LogEntry is one captured line of output.
type LogEntry struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Message   string    `json:"message"`
}

// LogStore persists and reads captured run output.
type LogStore struct {
	db *sql.DB
}

// NewLogStore wraps a database handle.
func NewLogStore(db *sql.DB) *LogStore { return &LogStore{db: db} }

// Append writes a single log line.
func (s *LogStore) Append(ctx context.Context, e LogEntry) error {
	return s.AppendBatch(ctx, []LogEntry{e})
}

// AppendBatch writes several lines in one transaction. Batching matters:
// integrations can be chatty and a transaction per line would be wasteful.
func (s *LogStore) AppendBatch(ctx context.Context, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("runs: begin log batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO run_logs (run_id, timestamp, stream, message) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("runs: prepare log insert: %w", err)
	}
	defer stmt.Close()

	for _, e := range entries {
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx, e.RunID, database.FormatTime(ts), e.Stream, e.Message); err != nil {
			return fmt.Errorf("runs: insert log for %s: %w", e.RunID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runs: commit log batch: %w", err)
	}
	return nil
}

// List returns log lines for a run with an id greater than afterID, oldest
// first. Passing afterID lets a client poll incrementally.
func (s *LogStore) List(ctx context.Context, runID string, afterID int64, limit int) ([]LogEntry, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, timestamp, stream, message FROM run_logs
		 WHERE run_id = ? AND id > ? ORDER BY id ASC LIMIT ?`,
		runID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("runs: list logs for %s: %w", runID, err)
	}
	defer rows.Close()

	out := make([]LogEntry, 0, 16)
	for rows.Next() {
		var (
			e  LogEntry
			ts database.NullableTime
		)
		if err := rows.Scan(&e.ID, &e.RunID, &ts, &e.Stream, &e.Message); err != nil {
			return nil, fmt.Errorf("runs: scan log for %s: %w", runID, err)
		}
		if ts.Valid {
			e.Timestamp = ts.Time
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Last returns the most recent message for a run on one stream. Otter uses it
// to report an integration's own final log line when a run finishes.
func (s *LogStore) Last(ctx context.Context, runID, stream string) (string, bool, error) {
	var message string
	err := s.db.QueryRowContext(ctx,
		`SELECT message FROM run_logs WHERE run_id = ? AND stream = ? ORDER BY id DESC LIMIT 1`,
		runID, stream).Scan(&message)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("runs: last log line for %s: %w", runID, err)
	}
	return message, true, nil
}

// DeleteForRun removes all logs for a run.
func (s *LogStore) DeleteForRun(ctx context.Context, runID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM run_logs WHERE run_id = ?`, runID)
	if err != nil {
		return 0, fmt.Errorf("runs: delete logs for %s: %w", runID, err)
	}
	return res.RowsAffected()
}

// DeleteOlderThan prunes log lines older than cutoff. Operators use this to
// keep the database from growing without bound.
func (s *LogStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM run_logs WHERE timestamp < ?`, database.FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("runs: prune logs: %w", err)
	}
	return res.RowsAffected()
}
