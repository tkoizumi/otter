package inspection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// Errors the API layer maps onto status codes.
var (
	// ErrNotConfigured means the run has no capture summary. That happens for a
	// run submitted before capture existed, or with capture explicitly off in a
	// way that wrote no row. It is deliberately distinct from "no requests
	// observed": a reader must not conclude the run made no network calls.
	ErrNotConfigured = errors.New("capture is not configured for this run")
	// ErrNotFound means the run has capture but not the requested exchange.
	ErrNotFound = errors.New("capture record not found")
	// ErrAmbiguous means more than one run recorded the same request id. A reader
	// cannot choose between them: request id uniqueness is per run, so the run id
	// has to be supplied to say which exchange is meant.
	ErrAmbiguous = errors.New("request id is recorded in more than one run")
)

// CaptureState is what a reader should conclude from a run's recording.
type CaptureState string

const (
	// CaptureUnavailable means there is no summary row at all.
	CaptureUnavailable CaptureState = "unavailable"
	// CaptureOff means capture was explicitly disabled for the run.
	CaptureOff CaptureState = "off"
	// CapturePending means the run has not finished reporting.
	CapturePending CaptureState = "pending"
	// CaptureComplete means the recording is believed whole.
	CaptureComplete CaptureState = "complete"
	// CaptureIncomplete means capture is known to have lost something.
	CaptureIncomplete CaptureState = "incomplete"
	// CaptureExpired means retention removed the requests but kept the summary.
	CaptureExpired CaptureState = "expired"
)

// RunCapture is the per-run summary. It survives payload retention, which is
// what lets an expired recording be told apart from an empty one.
type RunCapture struct {
	RunID string `json:"run_id"`
	// State is derived on load and is part of the wire representation, so a
	// reader never has to re-derive "recorded nothing" from a zero count.
	State           CaptureState `json:"state"`
	IntegrationID   string       `json:"integration_id"`
	Policy          Policy       `json:"policy"`
	SchemaVersion   int          `json:"schema_version"`
	PolicyVersion   int          `json:"policy_version"`
	Adapters        []string     `json:"adapters"`
	Coverage        string       `json:"coverage"`
	StartedAt       time.Time    `json:"started_at"`
	FinalizedAt     *time.Time   `json:"finalized_at,omitempty"`
	Finalization    Finalization `json:"finalization"`
	RequestCount    int          `json:"request_count"`
	CompletedCount  int          `json:"completed_count"`
	FailedCount     int          `json:"failed_count"`
	IncompleteCount int          `json:"incomplete_count"`
	DroppedEvents   int64        `json:"dropped_events"`
	DroppedBytes    int64        `json:"dropped_bytes"`
	StoredBytes     int64        `json:"stored_bytes"`
	RedactionCount  int64        `json:"redaction_count"`
	LastSequence    int64        `json:"last_sequence"`
	PayloadsExpired bool         `json:"payloads_expired"`
	ExpiredAt       *time.Time   `json:"expired_at,omitempty"`
	Note            string       `json:"note,omitempty"`
}

// UnavailableCapture is the summary reported for a run that has no recording at
// all, so a reader sees an explicit "unavailable" rather than a nil it might
// render as an empty history.
func UnavailableCapture(runID string) *RunCapture {
	return &RunCapture{RunID: runID, State: CaptureUnavailable}
}

// deriveState summarizes the recording for a reader.
func (c *RunCapture) deriveState() CaptureState {
	switch {
	case c == nil:
		return CaptureUnavailable
	case c.Policy == PolicyOff:
		return CaptureOff
	case c.PayloadsExpired:
		return CaptureExpired
	case c.Finalization == FinalizationPending:
		return CapturePending
	case c.Finalization == FinalizationComplete && c.DroppedEvents == 0 && c.IncompleteCount == 0:
		return CaptureComplete
	default:
		return CaptureIncomplete
	}
}

// CaptureSettings is what a run records when it is submitted. Only the server
// resolves these; the child is told the result and cannot widen it.
type CaptureSettings struct {
	RunID         string
	IntegrationID string
	Policy        Policy
	Adapters      []string
	Coverage      string
}

// Exchange is one stored HTTP exchange, complete with payloads.
type Exchange struct {
	ID            int64     `json:"id"`
	RunID         string    `json:"run_id"`
	RequestID     string    `json:"request_id"`
	IntegrationID string    `json:"integration_id"`
	ProducerSeq   int64     `json:"producer_seq"`
	OccurredAt    time.Time `json:"occurred_at"`
	IngestedAt    time.Time `json:"ingested_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	Phase    Phase `json:"phase"`
	Complete bool  `json:"complete"`

	Method     string `json:"method"`
	URL        string `json:"url"`
	InitialURL string `json:"initial_url,omitempty"`
	FinalURL   string `json:"final_url,omitempty"`
	CallSite   string `json:"call_site,omitempty"`

	StatusCode     *int   `json:"status_code,omitempty"`
	TransportError string `json:"transport_error,omitempty"`
	TransportClass string `json:"transport_error_class,omitempty"`

	DurationToHeadersMS *int64 `json:"duration_to_headers_ms,omitempty"`
	DurationBodyMS      *int64 `json:"duration_body_ms,omitempty"`
	DurationTotalMS     *int64 `json:"duration_total_ms,omitempty"`

	RequestHeaders  []HeaderPair    `json:"request_headers,omitempty"`
	ResponseHeaders []HeaderPair    `json:"response_headers,omitempty"`
	RequestBody     *BodyDescriptor `json:"request_body,omitempty"`
	ResponseBody    *BodyDescriptor `json:"response_body,omitempty"`

	// Payloads is metadata | partial | full: how much of this exchange's bodies
	// is visible. It is maintained at write time so a list need not load bodies.
	Payloads string `json:"payloads"`
}

// Completeness describes an exchange for a list view.
func (e Exchange) Completeness() string {
	if !e.Complete {
		return "incomplete"
	}
	if e.Payloads == "" {
		return "metadata"
	}
	return e.Payloads
}

// ExchangeSummary is a list row: metadata only, never a payload.
type ExchangeSummary struct {
	ID              int64     `json:"id"`
	RequestID       string    `json:"request_id"`
	Phase           Phase     `json:"phase"`
	Complete        bool      `json:"complete"`
	Method          string    `json:"method"`
	URL             string    `json:"url"`
	StatusCode      *int      `json:"status_code,omitempty"`
	TransportClass  string    `json:"transport_error_class,omitempty"`
	DurationTotalMS *int64    `json:"duration_total_ms,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
	Payloads        string    `json:"payloads"`
}

// Completeness describes a summary row for a list view.
func (s ExchangeSummary) Completeness() string {
	if !s.Complete {
		return "incomplete"
	}
	if s.Payloads == "" {
		return "metadata"
	}
	return s.Payloads
}

// IngestResult reports what one batch did. Partial acceptance is normal: capture
// is diagnostic, so exceeding a quota drops capture rather than failing a run.
type IngestResult struct {
	Accepted      int          `json:"accepted"`
	Duplicates    int          `json:"duplicates"`
	Stale         int          `json:"stale"`
	QuotaRejected int          `json:"quota_rejected"`
	DroppedBytes  int64        `json:"dropped_bytes"`
	Finalization  Finalization `json:"finalization"`
}

// Store persists capture summaries and HTTP exchanges.
type Store struct {
	db       *sql.DB
	redactor *Redactor
	limits   Limits
}

// NewStore wraps a database handle. A nil redactor means the mandatory default
// policy, and zero limits mean DefaultLimits.
func NewStore(db *sql.DB, redactor *Redactor, limits Limits) *Store {
	if redactor == nil {
		redactor = DefaultRedactor()
	}
	if limits.MaxBodyBytes == 0 {
		limits = DefaultLimits()
	}
	return &Store{db: db, redactor: redactor, limits: limits}
}

// Limits returns the limits this store enforces.
func (s *Store) Limits() Limits { return s.limits }

// Begin records the resolved capture policy for a run. It is called when the run
// is submitted, before any child starts, so that a run which observes nothing is
// still distinguishable from a run that predates capture.
//
// It is idempotent: re-submitting the same run id keeps the first row.
func (s *Store) Begin(ctx context.Context, settings CaptureSettings) error {
	if settings.RunID == "" {
		return fmt.Errorf("inspection: capture settings need a run id")
	}
	if !settings.Policy.Valid() {
		return fmt.Errorf("inspection: invalid capture policy %q", settings.Policy)
	}
	adapters := settings.Adapters
	if len(adapters) == 0 && settings.Policy.Enabled() {
		adapters = []string{AdapterURLLib}
	}
	if adapters == nil {
		adapters = []string{}
	}
	coverage := settings.Coverage
	if coverage == "" && settings.Policy.Enabled() {
		coverage = CoverageFor(adapters)
	}
	encoded, err := json.Marshal(adapters)
	if err != nil {
		return fmt.Errorf("inspection: encode adapters: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO run_capture
		   (run_id, integration_id, policy, schema_version, policy_version,
		    adapters, coverage, started_at, finalization)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id) DO NOTHING`,
		settings.RunID, settings.IntegrationID, string(settings.Policy),
		SchemaVersion, PolicyVersion, string(encoded), coverage,
		database.FormatTime(time.Now().UTC()), string(FinalizationPending))
	if err != nil {
		return fmt.Errorf("inspection: begin capture for %s: %w", settings.RunID, err)
	}
	return nil
}

// Capture returns a run's capture summary, or ErrNotConfigured when the run has
// no summary at all.
func (s *Store) Capture(ctx context.Context, runID string) (*RunCapture, error) {
	capture, err := scanCapture(s.db.QueryRowContext(ctx,
		`SELECT `+captureColumns+` FROM run_capture WHERE run_id = ?`, runID))
	if err != nil {
		return nil, err
	}
	return capture, nil
}

// Ingest applies one validated batch of events.
//
// Ordering is by producer sequence within an exchange: an event older than what
// is already stored is stale and ignored, and a finalized exchange is immutable
// so a late event can never rewrite a result. Re-delivering the same batch is
// therefore harmless, which is what makes the SDK's bounded retries safe.
func (s *Store) Ingest(ctx context.Context, runID string, batch EventBatch) (*IngestResult, error) {
	now := time.Now().UTC()
	result := &IngestResult{}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("inspection: begin ingest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	capture, err := scanCapture(tx.QueryRowContext(ctx,
		`SELECT `+captureColumns+` FROM run_capture WHERE run_id = ?`, runID))
	if errors.Is(err, ErrNotConfigured) {
		return nil, fmt.Errorf("%w: %s", ErrNotConfigured, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("inspection: load capture for %s: %w", runID, err)
	}
	if batch.Policy != capture.Policy {
		return nil, fmt.Errorf("inspection: batch policy %q does not match the run's recorded policy %q",
			batch.Policy, capture.Policy)
	}

	var (
		pendingBytes   int64
		newRequests    int
		redactionDelta int64
	)

	for _, raw := range batch.Events {
		event, redactions := SanitizeEvent(raw, s.redactor, s.limits)

		existing, found, err := loadExchangeTx(ctx, tx, runID, event.RequestID)
		if err != nil {
			return nil, err
		}
		if found && event.ProducerSeq < existing.ProducerSeq {
			result.Stale++
			continue
		}
		if found && (existing.Complete || event.ProducerSeq == existing.ProducerSeq) {
			// The same producer sequence for the same request is the same event,
			// so a bounded retry that re-delivers it changes nothing. A finalized
			// exchange is immutable for the same reason: no later event may
			// rewrite a result that has already been reported.
			result.Duplicates++
			continue
		}

		merged, isNew := mergeExchange(existing, event, capture, now)
		rowBytes := exchangeRowBytes(merged)

		if isNew {
			if capture.RequestCount+newRequests+1 > s.limits.MaxRunRequests {
				result.QuotaRejected++
				result.DroppedBytes += rowBytes
				continue
			}
		}
		delta := rowBytes
		if found {
			delta = rowBytes - exchangeRowBytes(existing)
		}
		if delta > 0 && capture.StoredBytes+pendingBytes+delta > s.limits.MaxRunBytes {
			result.QuotaRejected++
			result.DroppedBytes += delta
			continue
		}

		if isNew {
			if err := insertExchangeTx(ctx, tx, merged); err != nil {
				return nil, err
			}
			newRequests++
		} else if err := updateExchangeTx(ctx, tx, merged); err != nil {
			return nil, err
		}
		pendingBytes += delta
		redactionDelta += int64(redactions)
		result.Accepted++
	}

	finalization := nextFinalization(capture.Finalization, batch.Finalization)
	var finalizedAt *time.Time
	if finalization != FinalizationPending {
		at := now
		if capture.FinalizedAt != nil {
			at = *capture.FinalizedAt
		}
		finalizedAt = &at
	}
	note := capture.Note
	if batch.Note != "" {
		note = bound(cleanText(batch.Note), 512)
	}

	// The child is the only place that knows which optional adapters were
	// importable, so its report is what makes coverage accurate. Merging keeps a
	// later partial report from narrowing it.
	adapters, adaptersChanged := mergeAdapters(capture.Adapters, batch.Adapters)
	var adaptersUpdate []string
	if adaptersChanged {
		adaptersUpdate = adapters
	}

	if err := refreshCaptureTx(ctx, tx, runID, refreshUpdate{
		redactionDelta: redactionDelta,
		droppedEvents:  batch.DroppedEvents,
		droppedBytes:   batch.DroppedBytes,
		finalization:   finalization,
		finalizedAt:    finalizedAt,
		note:           note,
		adapters:       adaptersUpdate,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("inspection: commit ingest: %w", err)
	}
	result.Finalization = finalization
	return result, nil
}

// List returns exchange summaries for a run with an id greater than afterID,
// oldest first. Payload columns are not read, so a list never loads bodies.
func (s *Store) List(ctx context.Context, runID string, afterID int64, limit int) ([]ExchangeSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, request_id, phase, complete, method, sanitized_url,
		        status_code, transport_error_class, duration_total_ms, occurred_at, payloads
		   FROM http_exchanges
		  WHERE run_id = ? AND id > ?
		  ORDER BY id ASC LIMIT ?`,
		runID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("inspection: list exchanges for %s: %w", runID, err)
	}
	defer rows.Close()

	out := make([]ExchangeSummary, 0, 16)
	for rows.Next() {
		var (
			item     ExchangeSummary
			phase    string
			complete int
			status   sql.NullInt64
			duration sql.NullInt64
			occurred database.NullableTime
		)
		if err := rows.Scan(&item.ID, &item.RequestID, &phase, &complete, &item.Method,
			&item.URL, &status, &item.TransportClass, &duration, &occurred, &item.Payloads); err != nil {
			return nil, fmt.Errorf("inspection: scan exchange for %s: %w", runID, err)
		}
		item.Phase = Phase(phase)
		item.Complete = complete != 0
		if status.Valid {
			v := int(status.Int64)
			item.StatusCode = &v
		}
		if duration.Valid {
			v := duration.Int64
			item.DurationTotalMS = &v
		}
		if occurred.Valid {
			item.OccurredAt = occurred.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Get returns one exchange with its payloads.
func (s *Store) Get(ctx context.Context, runID, requestID string) (*Exchange, error) {
	exchange, err := scanExchange(s.db.QueryRowContext(ctx,
		`SELECT `+exchangeColumns+` FROM http_exchanges WHERE run_id = ? AND request_id = ?`,
		runID, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inspection: get exchange %s/%s: %w", runID, requestID, err)
	}
	return exchange, nil
}

// FindByRequestID returns every stored exchange with this request id, oldest
// first, across all runs.
//
// It returns a slice rather than one exchange on purpose. request_id is
// SDK-supplied metadata whose uniqueness is only enforced per run, so a caller
// that gets more than one row has to treat the id as ambiguous instead of
// silently picking one.
func (s *Store) FindByRequestID(ctx context.Context, requestID string) ([]*Exchange, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+exchangeColumns+` FROM http_exchanges WHERE request_id = ? ORDER BY id ASC`,
		requestID)
	if err != nil {
		return nil, fmt.Errorf("inspection: find exchange %s: %w", requestID, err)
	}
	defer rows.Close()

	var out []*Exchange
	for rows.Next() {
		exchange, err := scanExchange(rows)
		if err != nil {
			return nil, fmt.Errorf("inspection: scan exchange %s: %w", requestID, err)
		}
		out = append(out, exchange)
	}
	return out, rows.Err()
}

// Finalize records the terminal state of a run's recording. The daemon calls it
// when the child exits: the SDK cannot be trusted to report its own death, and a
// process killed by a signal never gets to report at all.
func (s *Store) Finalize(ctx context.Context, runID string, reported Finalization, note string) error {
	if !ValidFinalization(reported) || reported == FinalizationPending {
		return fmt.Errorf("inspection: finalization must be terminal, got %q", reported)
	}
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("inspection: begin finalize: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	capture, err := scanCapture(tx.QueryRowContext(ctx,
		`SELECT `+captureColumns+` FROM run_capture WHERE run_id = ?`, runID))
	if errors.Is(err, ErrNotConfigured) {
		return nil // capture was never configured; nothing to finalize
	}
	if err != nil {
		return fmt.Errorf("inspection: load capture for %s: %w", runID, err)
	}

	resolvedNote := capture.Note
	if note != "" {
		resolvedNote = bound(cleanText(note), 512)
	}
	at := now
	if err := refreshCaptureTx(ctx, tx, runID, refreshUpdate{
		finalization: nextFinalization(capture.Finalization, reported),
		finalizedAt:  &at,
		note:         resolvedNote,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("inspection: commit finalize: %w", err)
	}
	return nil
}

// FinalizeUnreported marks a recording whose process exited without ever
// reporting a finalization. A run that did report is left untouched: the daemon
// has nothing to add to a clean report, and guessing would turn a complete
// recording into an incomplete one.
func (s *Store) FinalizeUnreported(ctx context.Context, runID, note string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE run_capture SET
		   finalization = ?,
		   finalized_at = ?,
		   note = ?,
		   incomplete_count = (SELECT COUNT(*) FROM http_exchanges e
		                        WHERE e.run_id = run_capture.run_id AND e.complete = 0)
		 WHERE run_id = ? AND finalization = ?`,
		string(FinalizationIncomplete), database.FormatTime(time.Now().UTC()),
		bound(cleanText(note), 512), runID, string(FinalizationPending))
	if err != nil {
		return fmt.Errorf("inspection: mark unreported %s: %w", runID, err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return nil // already reported, or no capture configured
	}
	return nil
}

// DeleteForRun removes every capture record for one run.
func (s *Store) DeleteForRun(ctx context.Context, runID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("inspection: begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM http_exchanges WHERE run_id = ?`, runID)
	if err != nil {
		return 0, fmt.Errorf("inspection: delete exchanges for %s: %w", runID, err)
	}
	removed, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_capture WHERE run_id = ?`, runID); err != nil {
		return 0, fmt.Errorf("inspection: delete capture for %s: %w", runID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("inspection: commit delete: %w", err)
	}
	return removed, nil
}

// DeleteForIntegration removes every capture record owned by one integration
// identity, for `otter delete`.
func (s *Store) DeleteForIntegration(ctx context.Context, integrationID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("inspection: begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM http_exchanges WHERE integration_id = ?`, integrationID)
	if err != nil {
		return 0, fmt.Errorf("inspection: delete exchanges for %s: %w", integrationID, err)
	}
	removed, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_capture WHERE integration_id = ?`, integrationID); err != nil {
		return 0, fmt.Errorf("inspection: delete capture for %s: %w", integrationID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("inspection: commit delete: %w", err)
	}
	return removed, nil
}

// ExpireOlderThan removes the payloads of recordings older than cutoff but keeps
// their summaries, so an expired recording is distinguishable from one that
// observed nothing. At most maxRuns runs are handled per call, one transaction
// each: cleanup must never hold the daemon's single connection for long.
func (s *Store) ExpireOlderThan(ctx context.Context, cutoff time.Time, maxRuns int) (int64, int64, error) {
	if maxRuns <= 0 {
		maxRuns = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id FROM run_capture
		  WHERE payloads_expired = 0 AND policy != ? AND started_at < ?
		  ORDER BY started_at ASC LIMIT ?`,
		string(PolicyOff), database.FormatTime(cutoff), maxRuns)
	if err != nil {
		return 0, 0, fmt.Errorf("inspection: select expired runs: %w", err)
	}
	runIDs := make([]string, 0, maxRuns)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("inspection: scan expired run: %w", err)
		}
		runIDs = append(runIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("inspection: read expired runs: %w", err)
	}
	rows.Close()

	now := database.FormatTime(time.Now().UTC())
	var runs, deleted int64
	for _, runID := range runIDs {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return runs, deleted, fmt.Errorf("inspection: begin expiry: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM http_exchanges WHERE run_id = ?`, runID)
		if err != nil {
			tx.Rollback()
			return runs, deleted, fmt.Errorf("inspection: expire exchanges for %s: %w", runID, err)
		}
		n, _ := res.RowsAffected()
		if _, err := tx.ExecContext(ctx,
			`UPDATE run_capture SET payloads_expired = 1, expired_at = ?, stored_bytes = 0 WHERE run_id = ?`,
			now, runID); err != nil {
			tx.Rollback()
			return runs, deleted, fmt.Errorf("inspection: mark expired %s: %w", runID, err)
		}
		if err := tx.Commit(); err != nil {
			return runs, deleted, fmt.Errorf("inspection: commit expiry: %w", err)
		}
		runs++
		deleted += n
	}
	return runs, deleted, nil
}

// MarkStalePending marks recordings left pending by a previous process as
// abrupt. It runs at startup: a daemon that was killed cannot have finalized its
// children, and reporting those recordings as pending forever would imply they
// might still complete.
func (s *Store) MarkStalePending(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE run_capture SET
		   finalization = ?,
		   finalized_at = ?,
		   incomplete_count = (SELECT COUNT(*) FROM http_exchanges e
		                        WHERE e.run_id = run_capture.run_id AND e.complete = 0)
		 WHERE finalization = ? AND started_at < ?`,
		string(FinalizationAbrupt), database.FormatTime(time.Now().UTC()),
		string(FinalizationPending), database.FormatTime(olderThan))
	if err != nil {
		return 0, fmt.Errorf("inspection: mark stale recordings: %w", err)
	}
	affected, _ := res.RowsAffected()
	return affected, nil
}

// ------------------------------------------------------------------ internals

const captureColumns = `run_id, integration_id, policy, schema_version, policy_version,
	adapters, coverage, started_at, finalized_at, finalization,
	request_count, completed_count, failed_count, incomplete_count,
	dropped_events, dropped_bytes, stored_bytes, redaction_count,
	last_sequence, payloads_expired, expired_at, note`

const exchangeColumns = `id, run_id, request_id, integration_id, producer_seq,
	occurred_at, ingested_at, updated_at, phase, complete,
	method, sanitized_url, initial_url, final_url, call_site,
	status_code, transport_error, transport_error_class,
	duration_to_headers_ms, duration_body_ms, duration_total_ms,
	request_headers, response_headers, request_body, response_body, payloads`

func scanCapture(sc interface{ Scan(...any) error }) (*RunCapture, error) {
	var (
		c               RunCapture
		policy          string
		adapters        string
		started         database.NullableTime
		finalized       database.NullableTime
		finalization    string
		expiredAt       database.NullableTime
		payloadsExpired int
	)
	err := sc.Scan(&c.RunID, &c.IntegrationID, &policy, &c.SchemaVersion, &c.PolicyVersion,
		&adapters, &c.Coverage, &started, &finalized, &finalization,
		&c.RequestCount, &c.CompletedCount, &c.FailedCount, &c.IncompleteCount,
		&c.DroppedEvents, &c.DroppedBytes, &c.StoredBytes, &c.RedactionCount,
		&c.LastSequence, &payloadsExpired, &expiredAt, &c.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, fmt.Errorf("inspection: scan capture: %w", err)
	}
	c.Policy = Policy(policy)
	c.Finalization = Finalization(finalization)
	if started.Valid {
		c.StartedAt = started.Time
	}
	c.FinalizedAt = finalized.Ptr()
	c.ExpiredAt = expiredAt.Ptr()
	c.PayloadsExpired = payloadsExpired != 0
	if adapters != "" {
		_ = json.Unmarshal([]byte(adapters), &c.Adapters)
	}
	c.State = c.deriveState()
	return &c, nil
}

func scanExchange(sc interface{ Scan(...any) error }) (*Exchange, error) {
	var (
		e          Exchange
		occurred   database.NullableTime
		ingested   database.NullableTime
		updated    database.NullableTime
		phase      string
		complete   int
		status     sql.NullInt64
		toHeaders  sql.NullInt64
		bodyMS     sql.NullInt64
		totalMS    sql.NullInt64
		reqHeaders string
		resHeaders string
		reqBody    string
		resBody    string
	)
	err := sc.Scan(&e.ID, &e.RunID, &e.RequestID, &e.IntegrationID, &e.ProducerSeq,
		&occurred, &ingested, &updated, &phase, &complete,
		&e.Method, &e.URL, &e.InitialURL, &e.FinalURL, &e.CallSite,
		&status, &e.TransportError, &e.TransportClass,
		&toHeaders, &bodyMS, &totalMS,
		&reqHeaders, &resHeaders, &reqBody, &resBody, &e.Payloads)
	if err != nil {
		return nil, err
	}
	e.Phase = Phase(phase)
	e.Complete = complete != 0
	if occurred.Valid {
		e.OccurredAt = occurred.Time
	}
	if ingested.Valid {
		e.IngestedAt = ingested.Time
	}
	if updated.Valid {
		e.UpdatedAt = updated.Time
	}
	if status.Valid {
		v := int(status.Int64)
		e.StatusCode = &v
	}
	e.DurationToHeadersMS = int64Ptr(toHeaders)
	e.DurationBodyMS = int64Ptr(bodyMS)
	e.DurationTotalMS = int64Ptr(totalMS)
	if reqHeaders != "" {
		_ = json.Unmarshal([]byte(reqHeaders), &e.RequestHeaders)
	}
	if resHeaders != "" {
		_ = json.Unmarshal([]byte(resHeaders), &e.ResponseHeaders)
	}
	if reqBody != "" {
		var body BodyDescriptor
		if err := json.Unmarshal([]byte(reqBody), &body); err == nil {
			e.RequestBody = &body
		}
	}
	if resBody != "" {
		var body BodyDescriptor
		if err := json.Unmarshal([]byte(resBody), &body); err == nil {
			e.ResponseBody = &body
		}
	}
	return &e, nil
}

func int64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

func loadExchangeTx(ctx context.Context, tx *sql.Tx, runID, requestID string) (*Exchange, bool, error) {
	exchange, err := scanExchange(tx.QueryRowContext(ctx,
		`SELECT `+exchangeColumns+` FROM http_exchanges WHERE run_id = ? AND request_id = ?`,
		runID, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspection: load exchange %s: %w", requestID, err)
	}
	return exchange, true, nil
}

// mergeExchange combines an event into an existing exchange. Fields the event
// does not mention are preserved, which is what makes the three-update lifecycle
// (started, response, completed) work without the client resending everything.
func mergeExchange(existing *Exchange, event RequestEvent, capture *RunCapture, now time.Time) (*Exchange, bool) {
	isNew := existing == nil
	out := Exchange{}
	if isNew {
		out = Exchange{
			RunID:         capture.RunID,
			RequestID:     event.RequestID,
			IntegrationID: capture.IntegrationID, // authoritative, never the client's
			OccurredAt:    event.OccurredAt,
			IngestedAt:    now,
		}
		if out.OccurredAt.IsZero() {
			out.OccurredAt = now
		}
	} else {
		out = *existing
	}
	out.UpdatedAt = now
	if event.ProducerSeq > out.ProducerSeq {
		out.ProducerSeq = event.ProducerSeq
	}

	if v := event.Method; v != "" {
		out.Method = v
	}
	if v := event.URL; v != "" {
		out.URL = v
	}
	if v := event.InitialURL; v != "" {
		out.InitialURL = v
	}
	if v := event.FinalURL; v != "" {
		out.FinalURL = v
	}
	if v := event.CallSite; v != "" {
		out.CallSite = v
	}
	if event.StatusCode != nil {
		v := *event.StatusCode
		out.StatusCode = &v
	}
	if v := event.TransportError; v != "" {
		out.TransportError = v
	}
	if v := event.TransportClass; v != "" {
		out.TransportClass = v
	}
	if event.DurationToHeadersMS != nil {
		v := *event.DurationToHeadersMS
		out.DurationToHeadersMS = &v
	}
	if event.DurationBodyMS != nil {
		v := *event.DurationBodyMS
		out.DurationBodyMS = &v
	}
	if event.DurationTotalMS != nil {
		v := *event.DurationTotalMS
		out.DurationTotalMS = &v
	}
	if len(event.RequestHeaders) > 0 {
		out.RequestHeaders = event.RequestHeaders
	}
	if len(event.ResponseHeaders) > 0 {
		out.ResponseHeaders = event.ResponseHeaders
	}
	if event.RequestBody != nil {
		out.RequestBody = event.RequestBody
	}
	if event.ResponseBody != nil {
		out.ResponseBody = event.ResponseBody
	}

	if event.Kind == EventCompleted {
		out.Phase = PhaseFor(event)
		out.Complete = true
	} else if out.Phase == "" {
		out.Phase = PhaseInProgress
	}
	out.Payloads = PayloadCoverage(capture.Policy, out.RequestBody, out.ResponseBody)
	return &out, isNew
}

func insertExchangeTx(ctx context.Context, tx *sql.Tx, e *Exchange) error {
	reqHeaders, err := headerJSON(e.RequestHeaders)
	if err != nil {
		return fmt.Errorf("inspection: encode request headers: %w", err)
	}
	resHeaders, err := headerJSON(e.ResponseHeaders)
	if err != nil {
		return fmt.Errorf("inspection: encode response headers: %w", err)
	}
	reqBody, err := bodyJSON(e.RequestBody)
	if err != nil {
		return fmt.Errorf("inspection: encode request body: %w", err)
	}
	resBody, err := bodyJSON(e.ResponseBody)
	if err != nil {
		return fmt.Errorf("inspection: encode response body: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO http_exchanges
		   (run_id, request_id, integration_id, producer_seq, occurred_at, ingested_at, updated_at,
		    phase, complete, method, sanitized_url, initial_url, final_url, call_site,
		    status_code, transport_error, transport_error_class,
		    duration_to_headers_ms, duration_body_ms, duration_total_ms,
		    request_headers, response_headers, request_body, response_body, payloads)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, e.RequestID, e.IntegrationID, e.ProducerSeq,
		database.FormatTime(e.OccurredAt), database.FormatTime(e.IngestedAt), database.FormatTime(e.UpdatedAt),
		string(e.Phase), boolInt(e.Complete), e.Method, e.URL, e.InitialURL, e.FinalURL, e.CallSite,
		database.NullableInt(e.StatusCode), e.TransportError, e.TransportClass,
		nullableInt64(e.DurationToHeadersMS), nullableInt64(e.DurationBodyMS), nullableInt64(e.DurationTotalMS),
		reqHeaders, resHeaders, reqBody, resBody, e.Payloads)
	if err != nil {
		return fmt.Errorf("inspection: insert exchange %s: %w", e.RequestID, err)
	}
	return nil
}

func updateExchangeTx(ctx context.Context, tx *sql.Tx, e *Exchange) error {
	reqHeaders, err := headerJSON(e.RequestHeaders)
	if err != nil {
		return fmt.Errorf("inspection: encode request headers: %w", err)
	}
	resHeaders, err := headerJSON(e.ResponseHeaders)
	if err != nil {
		return fmt.Errorf("inspection: encode response headers: %w", err)
	}
	reqBody, err := bodyJSON(e.RequestBody)
	if err != nil {
		return fmt.Errorf("inspection: encode request body: %w", err)
	}
	resBody, err := bodyJSON(e.ResponseBody)
	if err != nil {
		return fmt.Errorf("inspection: encode response body: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE http_exchanges SET
		   integration_id = ?, producer_seq = ?, occurred_at = ?, updated_at = ?,
		   phase = ?, complete = ?, method = ?, sanitized_url = ?, initial_url = ?,
		   final_url = ?, call_site = ?, status_code = ?, transport_error = ?,
		   transport_error_class = ?, duration_to_headers_ms = ?, duration_body_ms = ?,
		   duration_total_ms = ?, request_headers = ?, response_headers = ?,
		   request_body = ?, response_body = ?, payloads = ?
		 WHERE run_id = ? AND request_id = ?`,
		e.IntegrationID, e.ProducerSeq, database.FormatTime(e.OccurredAt), database.FormatTime(e.UpdatedAt),
		string(e.Phase), boolInt(e.Complete), e.Method, e.URL, e.InitialURL,
		e.FinalURL, e.CallSite, database.NullableInt(e.StatusCode), e.TransportError,
		e.TransportClass, nullableInt64(e.DurationToHeadersMS), nullableInt64(e.DurationBodyMS),
		nullableInt64(e.DurationTotalMS), reqHeaders, resHeaders,
		reqBody, resBody, e.Payloads,
		e.RunID, e.RequestID)
	if err != nil {
		return fmt.Errorf("inspection: update exchange %s: %w", e.RequestID, err)
	}
	return nil
}

type refreshUpdate struct {
	redactionDelta int64
	droppedEvents  int64
	droppedBytes   int64
	finalization   Finalization
	finalizedAt    *time.Time
	note           string
	// adapters, when non-nil, replaces the run's reported adapter set and the
	// coverage derived from it. A nil value leaves both as they were.
	adapters []string
}

// refreshCaptureTx recomputes the run summary from the rows that actually exist.
// Deriving the counters rather than accumulating them means a dropped event, a
// stale update or a retried batch can never leave the summary disagreeing with
// the records a reader can see.
func refreshCaptureTx(ctx context.Context, tx *sql.Tx, runID string, update refreshUpdate) error {
	var adaptersArg, coverageArg any
	if update.adapters != nil {
		encoded, err := json.Marshal(update.adapters)
		if err != nil {
			return fmt.Errorf("inspection: encode adapters for %s: %w", runID, err)
		}
		adaptersArg = string(encoded)
		coverageArg = CoverageFor(update.adapters)
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE run_capture SET
		   request_count    = (SELECT COUNT(*) FROM http_exchanges WHERE run_id = ?),
		   completed_count  = (SELECT COUNT(*) FROM http_exchanges WHERE run_id = ? AND complete = 1 AND phase = ?),
		   failed_count     = (SELECT COUNT(*) FROM http_exchanges WHERE run_id = ? AND complete = 1 AND phase = ?),
		   incomplete_count = (SELECT COUNT(*) FROM http_exchanges WHERE run_id = ? AND complete = 0),
		   stored_bytes     = (SELECT COALESCE(SUM(
		                          length(request_headers) + length(response_headers) +
		                          length(request_body) + length(response_body) +
		                          length(sanitized_url) + length(call_site) + length(transport_error)), 0)
		                       FROM http_exchanges WHERE run_id = ?),
		   last_sequence    = (SELECT COALESCE(MAX(producer_seq), 0) FROM http_exchanges WHERE run_id = ?),
		   redaction_count  = redaction_count + ?,
		   dropped_events   = MAX(dropped_events, ?),
		   dropped_bytes    = MAX(dropped_bytes, ?),
		   finalization     = ?,
		   finalized_at     = ?,
		   note             = ?,
		   adapters         = COALESCE(?, adapters),
		   coverage         = COALESCE(?, coverage)
		 WHERE run_id = ?`,
		runID,
		runID, string(PhaseCompleted),
		runID, string(PhaseFailed),
		runID,
		runID,
		runID,
		update.redactionDelta,
		update.droppedEvents,
		update.droppedBytes,
		string(update.finalization),
		database.FormatNullable(update.finalizedAt),
		update.note,
		adaptersArg,
		coverageArg,
		runID)
	if err != nil {
		return fmt.Errorf("inspection: refresh capture for %s: %w", runID, err)
	}
	return nil
}

// mergeAdapters unions the adapters a run has reported with what is already
// recorded. Coverage can only widen: an adapter is installed for the life of the
// process, so a later report that omits one is a partial report, not a
// downgrade. It reports whether the stored value changes.
func mergeAdapters(current, reported []string) ([]string, bool) {
	if len(reported) == 0 {
		return nil, false
	}
	present := make(map[string]bool, len(current)+len(reported))
	for _, adapter := range current {
		if ValidAdapter(adapter) {
			present[adapter] = true
		}
	}
	for _, adapter := range reported {
		if ValidAdapter(adapter) {
			present[adapter] = true
		}
	}
	merged := make([]string, 0, len(present))
	for _, adapter := range AllAdapters() {
		if present[adapter] {
			merged = append(merged, adapter)
		}
	}
	if len(merged) == len(current) {
		same := true
		for i := range merged {
			if merged[i] != current[i] {
				same = false
				break
			}
		}
		if same {
			return nil, false
		}
	}
	return merged, true
}

// nextFinalization keeps the worst outcome seen. A recording can be discovered
// to be incomplete after the child claimed success, but a later clean report
// must never overwrite known loss.
func nextFinalization(current, reported Finalization) Finalization {
	if reported == "" {
		return current
	}
	if finalizationSeverity(reported) > finalizationSeverity(current) {
		return reported
	}
	return current
}

func finalizationSeverity(f Finalization) int {
	switch f {
	case FinalizationComplete:
		return 0
	case FinalizationIncomplete:
		return 1
	case FinalizationAbrupt:
		return 2
	default:
		return -1
	}
}

func exchangeRowBytes(e *Exchange) int64 {
	total := int64(len(e.Method) + len(e.URL) + len(e.InitialURL) + len(e.FinalURL) +
		len(e.CallSite) + len(e.TransportError))
	for _, pair := range e.RequestHeaders {
		total += int64(len(pair.Name) + len(pair.Value))
	}
	for _, pair := range e.ResponseHeaders {
		total += int64(len(pair.Name) + len(pair.Value))
	}
	total += bodyBytes(e.RequestBody)
	total += bodyBytes(e.ResponseBody)
	return total
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
