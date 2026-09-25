package inspection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// TimelineCursor is a position in the run timeline, expressed for a query
// against the exchange table. It is a lexicographic bound: the query must return
// exactly the rows the merged order places after (At, Rank, ID).
//
// Rank is the source rank of the cursor's own event, not of this table. See
// runs.LogCursor for the full explanation.
type TimelineCursor struct {
	At   time.Time
	Rank int
	// ID is the id of the cursor's own event. It breaks the tie only when the
	// cursor also came from this table; Rank = -1 means nothing was emitted yet.
	ID int64
}

// Source ranks for the merged order, mirroring runs.RankLogs/RankHTTP.
const (
	rankLogs = 0
	rankHTTP = 1
)

// TimelineExchange is one HTTP exchange projected for the timeline.
//
// It exists so the timeline can show request metadata without ever reading a
// header or body column. Adding a field here is a deliberate act; the payload
// columns are not in the projection and cannot leak into the response by
// accident.
type TimelineExchange struct {
	ID         int64
	RequestID  string
	OccurredAt time.Time
	IngestedAt time.Time
	UpdatedAt  time.Time
	Phase      Phase
	Complete   bool
	Method     string
	URL        string
	StatusCode *int
	Transport  string
	Class      string
	DurationMS *int64
	CallSite   string
	Payloads   string
}

// Evidence identifies the extent of a run's retained exchanges without reading
// anything but their ids.
type Evidence struct {
	MinID int64
	MaxID int64
	// Present reports whether any exchange row remains. Retention removing them
	// all is a change this must detect, and it leaves neither id valid.
	Present bool
}

// TimelineExchangeTx reads one chronological page of a run's exchanges inside a
// caller's transaction.
//
// includeEqual decides whether the cursor position itself is eligible. The
// timeline passes false, so a cursor means "everything after the last event
// already emitted"; see runs.LogPageTx for why the comparison is made on the
// exclusive bound rather than on the merged rank.
func (s *Store) TimelineExchangeTx(ctx context.Context, tx *sql.Tx, runID string, cursor TimelineCursor, limit int) ([]TimelineExchange, error) {
	if limit <= 0 {
		return nil, nil
	}

	// The predicate is the literal tuple comparison for this table's rank (1):
	//
	//	(occurred_at, 1, id) > (cursor.At, cursor.Rank, cursor.ID)
	query := `SELECT id, request_id, occurred_at, ingested_at, updated_at, phase, complete,
	                 method, sanitized_url, status_code, transport_error_class, duration_total_ms,
	                 call_site, payloads
	            FROM http_exchanges
	           WHERE run_id = ? AND (occurred_at > ?`
	args := []any{runID, database.FormatTime(cursor.At)}
	switch {
	case cursor.Rank < rankHTTP:
		// The cursor sits on a lower-ranked event, so every exchange at this
		// instant still sorts after it.
		query += ` OR occurred_at = ?`
		args = append(args, database.FormatTime(cursor.At))
	case cursor.Rank == rankHTTP:
		query += ` OR (occurred_at = ? AND id > ?)`
		args = append(args, database.FormatTime(cursor.At), cursor.ID)
	}
	query += `) ORDER BY occurred_at ASC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("inspection: read timeline page for %s: %w", runID, err)
	}
	defer rows.Close()

	out := make([]TimelineExchange, 0, 16)
	for rows.Next() {
		var (
			item     TimelineExchange
			phase    string
			complete int
			occurred database.NullableTime
			ingested database.NullableTime
			updated  database.NullableTime
			status   sql.NullInt64
			duration sql.NullInt64
		)
		if err := rows.Scan(&item.ID, &item.RequestID, &occurred, &ingested, &updated,
			&phase, &complete, &item.Method, &item.URL, &status, &item.Class,
			&duration, &item.CallSite, &item.Payloads); err != nil {
			return nil, fmt.Errorf("inspection: scan timeline exchange for %s: %w", runID, err)
		}
		item.Phase = Phase(phase)
		item.Complete = complete != 0
		if occurred.Valid {
			item.OccurredAt = occurred.Time
		}
		if ingested.Valid {
			item.IngestedAt = ingested.Time
		}
		if updated.Valid {
			item.UpdatedAt = updated.Time
		}
		if status.Valid {
			value := int(status.Int64)
			item.StatusCode = &value
		}
		if duration.Valid {
			value := duration.Int64
			item.DurationMS = &value
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// TimelineEvidenceTx reports the extent of the run's retained exchanges with two
// covering index seeks.
func (s *Store) TimelineEvidenceTx(ctx context.Context, tx *sql.Tx, runID string) (Evidence, error) {
	var (
		minID sql.NullInt64
		maxID sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(id), MAX(id) FROM http_exchanges WHERE run_id = ?`, runID).
		Scan(&minID, &maxID); err != nil {
		return Evidence{}, fmt.Errorf("inspection: read exchange evidence for %s: %w", runID, err)
	}
	if !minID.Valid || !maxID.Valid {
		return Evidence{}, nil
	}
	return Evidence{MinID: minID.Int64, MaxID: maxID.Int64, Present: true}, nil
}

// TimelineDigestTx folds the run's retained exchange summaries into a stable
// digest, so a continuation can tell that an exchange was updated or removed
// between pages.
//
// Capture bounds a run to 1,000 exchanges, so this reads at most that many rows
// and only metadata columns. It never touches a header or body column. Bodies
// are excluded deliberately: a payload-only change does not alter what the
// timeline displays, and reading them would turn an evidence check into a
// payload read.
func (s *Store) TimelineDigestTx(ctx context.Context, tx *sql.Tx, runID string) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, occurred_at, ingested_at, updated_at, phase, complete, method,
		        sanitized_url, status_code, transport_error_class, duration_total_ms,
		        call_site, payloads
		   FROM http_exchanges WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return "", fmt.Errorf("inspection: read timeline digest for %s: %w", runID, err)
	}
	defer rows.Close()

	hash := sha256.New()
	// Fields are joined with a NUL separator. NUL cannot appear in a SQLite TEXT
	// value produced by this code, so no combination of values can be rearranged
	// into a different row set with the same digest.
	write := func(values ...string) {
		for i, value := range values {
			if i > 0 {
				hash.Write([]byte{0})
			}
			hash.Write([]byte(value))
		}
		hash.Write([]byte{0, '\n'})
	}

	count := 0
	for rows.Next() {
		var (
			id       int64
			occurred database.NullableTime
			ingested database.NullableTime
			updated  database.NullableTime
			phase    string
			complete int
			method   string
			url      string
			status   sql.NullInt64
			class    string
			duration sql.NullInt64
			callSite string
			payloads string
		)
		if err := rows.Scan(&id, &occurred, &ingested, &updated, &phase, &complete,
			&method, &url, &status, &class, &duration, &callSite, &payloads); err != nil {
			return "", fmt.Errorf("inspection: scan timeline digest for %s: %w", runID, err)
		}
		write(
			strconv.FormatInt(id, 10),
			formatNullable(occurred),
			formatNullable(ingested),
			formatNullable(updated),
			phase,
			strconv.FormatBool(complete != 0),
			method,
			url,
			nullInt(status),
			class,
			nullInt(duration),
			callSite,
			payloads,
		)
		count++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("inspection: read timeline digest for %s: %w", runID, err)
	}
	// The row count is part of the digest so that a truncated read cannot hash
	// to the same value as a complete one.
	hash.Write([]byte("rows=" + strconv.Itoa(count)))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func nullInt(value sql.NullInt64) string {
	if !value.Valid {
		return ""
	}
	return strconv.FormatInt(value.Int64, 10)
}

// formatNullable renders a timestamp for the digest. An absent timestamp is the
// empty string rather than the zero time, so a NULL and a genuine zero value
// cannot be confused.
func formatNullable(value database.NullableTime) string {
	if !value.Valid {
		return ""
	}
	return database.FormatTime(value.Time)
}

// TimelineCaptureTx reads the run's capture summary inside a caller's
// transaction. A run with no recording reports "unavailable", matching the read
// path the inspection API uses.
//
// The returned summary keeps the raw finalization and loss counters alongside
// the derived State. That matters for an expired recording: deriveState reports
// "expired" and would otherwise hide that the recording was already incomplete
// before retention touched it, which is exactly the fact a reader must not lose.
func (s *Store) TimelineCaptureTx(ctx context.Context, tx *sql.Tx, runID string) (*RunCapture, error) {
	capture, err := scanCapture(tx.QueryRowContext(ctx,
		`SELECT `+captureColumns+` FROM run_capture WHERE run_id = ?`, runID))
	if errors.Is(err, ErrNotConfigured) {
		return UnavailableCapture(runID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspection: read capture for %s: %w", runID, err)
	}
	return capture, nil
}
