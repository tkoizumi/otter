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

// Log streams stored in run_logs.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamOtter  = "otter"
)

// Log origins record who wrote a line. They exist because the `otter` stream has
// more than one writer: the daemon narrates a run's lifecycle there, and the
// SDK's structured logger writes an integration's own ctx.log output to the same
// stream. Classifying by stream alone would call `ctx.log` a lifecycle event.
const (
	// OriginDaemon marks the runtime's own narration about a run.
	OriginDaemon = "daemon"
	// OriginChild marks anything the integration produced: its stdout and
	// stderr, and its ctx.log calls.
	OriginChild = "child"
)

// LogEntry is one captured line of output.
type LogEntry struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Message   string    `json:"message"`
	// Origin is OriginDaemon or OriginChild. An empty value means the row
	// predates the column, and IsLifecycle falls back to the old rule.
	Origin string `json:"origin,omitempty"`
}

// IsLifecycle reports whether this line is the daemon's own narration rather
// than output the integration produced.
//
// It prefers the recorded origin, because that is a fact about who wrote the
// line. Only a row written before the origin column existed has to be inferred:
// the SDK's structured-log suffix identifies ctx.log output, and the narration's
// fixed shapes identify the rest. The inference is deliberately confined to
// legacy rows, and deliberately conservative — an unrecognised line is treated
// as the integration's, because mislabelling a child's output as runtime
// narration is the more misleading error.
func (e LogEntry) IsLifecycle() bool {
	switch e.Origin {
	case OriginDaemon:
		return true
	case OriginChild:
		return false
	}
	if e.Stream != StreamOtter {
		return false
	}
	if hasStructuredSuffix(e.Message) {
		return false
	}
	return LooksLikeDaemonNarration(e.Message)
}

// narrationPrefixes are the message shapes appendOtterLog emits. Each is a fixed
// prefix that the real narration always produces; see the call sites in
// internal/daemon. A child sentence like "run failed because ..." does not match
// "run failed,", and a child writing exactly "run failed" is indistinguishable
// in a legacy row either way.
var narrationPrefixes = []string{
	"run queued (",
	"run started (",
	// Terminal summaries and the cancellation notices are parenthesis-first;
	// the crash and pre-launch lines are label-first.
	"run cancelled before execution",
	"run cancelled:",
	"run ",
	"retry ",
	"not started: ",
	"marked failed: ",
}

// narrationStatusWords are the statuses that may follow the generic "run "
// prefix. They exist so the "run " prefix above cannot swallow a child's own
// sentence: "run failed because the token expired" is the integration talking,
// and this classifier must not claim it.
var narrationStatusWords = []string{
	"timed out",
	"succeeded",
	"timed_out",
	"retrying",
	"failed",
}

// LooksLikeDaemonNarration reports whether a stored message has the shape of the
// runtime's own lifecycle narration. It exists for rows that predate the origin
// column; new rows record the origin and never need it. The same shapes are used
// by the migration that backfills those older rows, and this function is what the
// tests check so the two cannot drift apart silently.
func LooksLikeDaemonNarration(message string) bool {
	for _, prefix := range narrationPrefixes {
		if !strings.HasPrefix(message, prefix) {
			continue
		}
		if prefix != "run " {
			return true
		}

		// The bare "run " prefix is only narration when a status word follows and
		// the line punctuates it: the runtime always writes "run failed (attempt
		// 1, 41ms)…" or "run cancelled: …". A status followed by a word instead
		// ("run failed because the token expired") is the integration's own
		// sentence, and this classifier must not claim it.
		//
		// Longest status first, so "timed out" is considered before "timed".
		rest := message[len(prefix):]
		for _, status := range narrationStatusWords {
			if !strings.HasPrefix(rest, status) {
				continue
			}
			punctuation := strings.TrimPrefix(rest, status)
			if len(punctuation) == len(rest) {
				// A bare "run failed" is not a shape the runtime emits; every
				// narration line continues with detail.
				continue
			}
			first := strings.TrimLeft(punctuation, " ")
			if len(first) == len(punctuation) {
				// No space separated the status from what follows.
				continue
			}
			switch first[0] {
			case '(', ':', ',':
				return true
			}
			// A space then a word: not narration. Keep looking in case a longer
			// status word matches this line instead.
		}
	}
	return false
}

// hasStructuredSuffix reports whether a stored line ends with the JSON object the
// SDK's logger appends to ctx.log calls.
func hasStructuredSuffix(message string) bool {
	trimmed := strings.TrimRight(message, "\n")
	if !strings.HasSuffix(trimmed, "}") {
		return false
	}
	if strings.HasPrefix(trimmed, "{") {
		return json.Valid([]byte(trimmed))
	}
	index := strings.LastIndex(trimmed, " {")
	if index < 0 {
		return false
	}
	return json.Valid([]byte(trimmed[index+1:]))
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
		`INSERT INTO run_logs (run_id, timestamp, stream, message, origin) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("runs: prepare log insert: %w", err)
	}
	defer stmt.Close()

	for _, e := range entries {
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		origin := e.Origin
		if origin == "" {
			// A writer that did not say is treated as the integration: the
			// daemon's own narration is a small, explicit set of call sites.
			origin = OriginChild
		}
		if _, err := stmt.ExecContext(ctx, e.RunID, database.FormatTime(ts), e.Stream, e.Message, origin); err != nil {
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
		`SELECT id, run_id, timestamp, stream, message, origin FROM run_logs
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
		if err := rows.Scan(&e.ID, &e.RunID, &ts, &e.Stream, &e.Message, &e.Origin); err != nil {
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
