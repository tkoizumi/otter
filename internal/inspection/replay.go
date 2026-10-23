package inspection

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

// MaxReplaysPerRun bounds how many replays one run may record.
//
// The Cloud route rate-limits an operator, but the runtime is the component that
// actually sends traffic, so it keeps its own ceiling: a compromised or buggy
// control plane must not be able to turn one captured exchange into an unbounded
// outbound loop.
const MaxReplaysPerRun = 100

// ErrReplayLimit means a run has reached MaxReplaysPerRun. The original exchange
// is untouched; the refusal is about volume, not about the request.
var ErrReplayLimit = errors.New("inspection: replay limit reached for this run")

// Replay is one resend of a captured exchange.
//
// RequestHeaders and RequestBody hold the template, exactly as captured: headers
// whose value is a {{secret:NAME}} placeholder, and a sanitized body. The
// resolved bytes were sent by the runtime and are deliberately not stored; only
// ResolvedSHA256 records what was sent.
type Replay struct {
	ReplayID  string
	RunID     string
	RequestID string
	JobID     string

	OccurredAt time.Time
	Complete   bool
	Method     string
	URL        string

	RequestHeaders []HeaderPair
	RequestBody    *BodyDescriptor

	StatusCode     *int
	ResponseBody   *BodyDescriptor
	TransportError string
	TransportClass string

	DurationTotalMS *int64
	ResolvedSHA256  string
	CreatedAt       time.Time
}

const replayColumns = `replay_id, run_id, request_id, job_id, occurred_at, complete,
	method, url, request_headers, request_body, status_code, response_body,
	transport_error, transport_error_class, duration_total_ms, resolved_sha256,
	created_at`

// RecordReplay stores one replay and queues its document in the same transaction.
//
// The original exchange is never updated. The replay row and its export row
// commit together, so an operator who saw the replay response can rely on the
// document existing locally. A full outbox is swallowed for the same reason a
// failed exporter is swallowed during ingest: the exchange already happened, and
// losing the export of it must not turn a successful resend into an error.
func (s *Store) RecordReplay(ctx context.Context, replay *Replay) error {
	if replay == nil {
		return fmt.Errorf("inspection: replay is nil")
	}
	if strings.TrimSpace(replay.ReplayID) == "" {
		return fmt.Errorf("inspection: replay has no replay id")
	}
	if strings.TrimSpace(replay.RunID) == "" {
		return fmt.Errorf("inspection: replay has no run id")
	}
	if strings.TrimSpace(replay.RequestID) == "" {
		return fmt.Errorf("inspection: replay names no original request")
	}
	if replay.OccurredAt.IsZero() {
		replay.OccurredAt = time.Now().UTC()
	}
	replay.CreatedAt = time.Now().UTC()

	headers, err := json.Marshal(replay.RequestHeaders)
	if err != nil {
		return fmt.Errorf("inspection: encode replay request headers: %w", err)
	}
	requestBody, err := encodeOptionalBody(replay.RequestBody)
	if err != nil {
		return fmt.Errorf("inspection: encode replay request body: %w", err)
	}
	responseBody, err := encodeOptionalBody(replay.ResponseBody)
	if err != nil {
		return fmt.Errorf("inspection: encode replay response body: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("inspection: begin replay: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM http_replays WHERE run_id = ?`, replay.RunID).Scan(&count); err != nil {
		return fmt.Errorf("inspection: count replays for %s: %w", replay.RunID, err)
	}
	if count >= MaxReplaysPerRun {
		return fmt.Errorf("%w: %s has %d", ErrReplayLimit, replay.RunID, count)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO http_replays
		   (replay_id, run_id, request_id, job_id, occurred_at, complete,
		    method, url, request_headers, request_body, status_code, response_body,
		    transport_error, transport_error_class, duration_total_ms, resolved_sha256,
		    created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		replay.ReplayID, replay.RunID, replay.RequestID, replay.JobID,
		database.FormatTime(replay.OccurredAt), boolInt(replay.Complete),
		replay.Method, replay.URL, string(headers), requestBody,
		nullableInt(replay.StatusCode), responseBody,
		replay.TransportError, replay.TransportClass,
		nullableInt64(replay.DurationTotalMS), replay.ResolvedSHA256,
		database.FormatTime(replay.CreatedAt)); err != nil {
		return fmt.Errorf("inspection: store replay %s: %w", replay.ReplayID, err)
	}

	// The outbox is diagnostic work. If it is full, the replay still exists
	// locally; only its export is missing, and a later replay or a drain of the
	// queue is the path back.
	_ = (OutboxExporter{}).Export(ctx, tx, []ExportRecord{{
		Kind:      ExportReplay,
		RunID:     replay.RunID,
		RequestID: replay.ReplayID,
	}})

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("inspection: commit replay: %w", err)
	}
	return nil
}

// GetReplay returns one replay document by its own id.
func (s *Store) GetReplay(ctx context.Context, runID, replayID string) (*Replay, error) {
	replay, err := scanReplay(s.db.QueryRowContext(ctx,
		`SELECT `+replayColumns+` FROM http_replays WHERE run_id = ? AND replay_id = ?`,
		runID, replayID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inspection: get replay %s/%s: %w", runID, replayID, err)
	}
	return replay, nil
}

func scanReplay(sc interface{ Scan(...any) error }) (*Replay, error) {
	var (
		r        Replay
		occurred database.NullableTime
		created  database.NullableTime
		complete int
		headers  string
		reqBody  string
		resBody  string
		status   sql.NullInt64
		totalMS  sql.NullInt64
	)
	if err := sc.Scan(&r.ReplayID, &r.RunID, &r.RequestID, &r.JobID, &occurred, &complete,
		&r.Method, &r.URL, &headers, &reqBody, &status, &resBody,
		&r.TransportError, &r.TransportClass, &totalMS, &r.ResolvedSHA256, &created); err != nil {
		return nil, err
	}
	r.Complete = complete != 0
	if occurred.Valid {
		r.OccurredAt = occurred.Time
	}
	if created.Valid {
		r.CreatedAt = created.Time
	}
	if status.Valid {
		v := int(status.Int64)
		r.StatusCode = &v
	}
	if totalMS.Valid {
		v := totalMS.Int64
		r.DurationTotalMS = &v
	}
	if headers != "" {
		_ = json.Unmarshal([]byte(headers), &r.RequestHeaders)
	}
	if reqBody != "" {
		var body BodyDescriptor
		if err := json.Unmarshal([]byte(reqBody), &body); err == nil {
			r.RequestBody = &body
		}
	}
	if resBody != "" {
		var body BodyDescriptor
		if err := json.Unmarshal([]byte(resBody), &body); err == nil {
			r.ResponseBody = &body
		}
	}
	return &r, nil
}

// SanitizeBody applies the store's capture redactor to one replay body.
//
// The replay response comes from a third-party service, not from the SDK, so it
// has never been through the client-side sanitizer. Storing it without this
// would put an unredacted value into the same sink that the rest of capture
// exists to keep clean.
func (s *Store) SanitizeBody(body *BodyDescriptor) *BodyDescriptor {
	out, _ := sanitizeBody(body, s.redactor, s.limits)
	return out
}

func encodeOptionalBody(body *BodyDescriptor) (string, error) {
	if body == nil {
		return "", nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
