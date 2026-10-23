package inspection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// Acknowledgment states the agent may report for one pulled export.
const (
	ExportAckSent      = "sent"
	ExportAckAbandoned = "abandoned"
	ExportAckRetry     = "retry"
)

// maxExportAttempts bounds retries of one queue row. After this many failures
// the row is abandoned rather than blocking every later export forever.
const maxExportAttempts = 10

// ExportRef identifies one outbox row. It is the same identity the document is
// keyed by, so an ack can never name a different record than the pull returned.
type ExportRef struct {
	RunID     string     `json:"run_id"`
	Kind      ExportKind `json:"kind"`
	RequestID string     `json:"request_id,omitempty"`
}

// ExportDocument is one assembled document and the row it came from.
type ExportDocument struct {
	RowID     int64           `json:"row_id"`
	RunID     string          `json:"run_id"`
	Kind      ExportKind      `json:"kind"`
	RequestID string          `json:"request_id,omitempty"`
	Document  json.RawMessage `json:"document"`
}

// ExportBatch is what one pull returns. `Cursor` is the last row id in the
// batch; the agent passes it back to continue without re-reading the same rows.
type ExportBatch struct {
	Documents []ExportDocument `json:"documents"`
	Cursor    int64            `json:"cursor"`
}

// WithRuntimeID sets the identity stamped into every exported document. The
// runtime knows it; Cloud otherwise derives the same value from the agent
// credential, and the two must agree.
func (s *Store) WithRuntimeID(runtimeID string) *Store {
	s.runtimeID = runtimeID
	return s
}

// --- the frozen document shape ------------------------------------------------
//
// These structs exist to marshal capture-document-v1. They are not the storage
// shape and they are not the wire shape of the runtime API; they are the exact
// JSON the Cloud schema validates.

type captureExportBody struct {
	State         string          `json:"state"`
	ContentType   string          `json:"content_type,omitempty"`
	BytesObserved int64           `json:"bytes_observed,omitempty"`
	JSON          json.RawMessage `json:"json,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Redacted      bool            `json:"redacted,omitempty"`
	RedactedCount int             `json:"redacted_count,omitempty"`
}

type captureExportRequest struct {
	Method        string             `json:"method"`
	URL           string             `json:"url"`
	QueryRedacted bool               `json:"query_redacted"`
	Headers       *[]HeaderPair      `json:"headers,omitempty"`
	Body          *captureExportBody `json:"body,omitempty"`
}

type captureExportError struct {
	Code           string `json:"code,omitempty"`
	Message        string `json:"message,omitempty"`
	TransportClass string `json:"transport_class,omitempty"`
}

type captureExportResponse struct {
	StatusCode *int                `json:"status_code"`
	Body       *captureExportBody  `json:"body,omitempty"`
	Error      *captureExportError `json:"error,omitempty"`
}

type captureExportTiming struct {
	TotalMS     *int64 `json:"total_ms"`
	ToHeadersMS *int64 `json:"to_headers_ms,omitempty"`
}

type captureExportDocument struct {
	SchemaVersion int                   `json:"schema_version"`
	Kind          string                `json:"kind"`
	RuntimeID     string                `json:"runtime_id"`
	RunID         string                `json:"run_id"`
	RequestID     string                `json:"request_id"`
	CapturedAt    string                `json:"captured_at"`
	Complete      bool                  `json:"complete"`
	Payloads      string                `json:"payloads"`
	Request       captureExportRequest  `json:"request"`
	Response      captureExportResponse `json:"response"`
	Timing        captureExportTiming   `json:"timing"`
	CallSite      string                `json:"call_site,omitempty"`
	// ReplayOf and ResolvedSHA256 exist only on a replay document. The first
	// names the exchange the replay resent; the second is the digest of the
	// resolved bytes, which Cloud audits without ever holding those bytes.
	ReplayOf       string `json:"replay_of,omitempty"`
	ResolvedSHA256 string `json:"resolved_sha256,omitempty"`
}

type captureManifestRequest struct {
	RequestID     string `json:"request_id"`
	Method        string `json:"method"`
	Host          string `json:"host"`
	Path          string `json:"path"`
	StatusCode    *int   `json:"status_code"`
	Payloads      string `json:"payloads"`
	At            string `json:"at"`
	QueryRedacted bool   `json:"query_redacted"`
	Complete      bool   `json:"complete"`
}

type captureExportManifest struct {
	SchemaVersion int                      `json:"schema_version"`
	RuntimeID     string                   `json:"runtime_id"`
	RunID         string                   `json:"run_id"`
	CapturePolicy string                   `json:"capture_policy"`
	Finalized     bool                     `json:"finalized"`
	Requests      []captureManifestRequest `json:"requests"`
}

func exportTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func exportBodyValue(body *BodyDescriptor) *captureExportBody {
	if body == nil {
		return &captureExportBody{State: string(BodyEmpty)}
	}
	out := &captureExportBody{
		State:         string(body.State),
		ContentType:   body.ContentType,
		BytesObserved: body.BytesObserved,
		Reason:        body.Reason,
		Redacted:      body.Redacted,
		RedactedCount: body.RedactedCount,
	}
	switch body.State {
	case BodyCaptured:
		if len(body.JSON) == 0 {
			// A captured body with no JSON is not a document the schema accepts.
			// Report it as an omission rather than writing a half-body.
			out.State = string(BodyOmitted)
			out.Reason = ReasonIncomplete
			return out
		}
		out.JSON = body.JSON
	case BodyEmpty:
		// Nothing further.
	case BodyOmitted:
		// The reason is already copied.
	}
	return out
}

// Only header NAMES and placeholders leave the runtime. A header value that is
// not a secret placeholder is not exported at all; the schema forbids it, and
// the inspector has no header block to show it in.
func exportHeaderPairs(pairs []HeaderPair) *[]HeaderPair {
	out := make([]HeaderPair, 0, len(pairs))
	for _, pair := range pairs {
		if IsSecretPlaceholder(pair.Value) {
			out = append(out, pair)
		}
	}
	return &out
}

func exportResponseError(e *Exchange) *captureExportError {
	if e.ErrorCode == "" && e.ErrorMessage == "" && e.TransportClass == "" {
		return nil
	}
	return &captureExportError{
		Code:           e.ErrorCode,
		Message:        e.ErrorMessage,
		TransportClass: e.TransportClass,
	}
}

func exportPayloads(e *Exchange, capture *RunCapture) string {
	if e.Payloads != "" {
		return e.Payloads
	}
	if capture != nil && capture.Policy == PolicyMetadata {
		return "metadata"
	}
	return "full"
}

func (s *Store) buildExchangeDocument(ctx context.Context, e *Exchange) ([]byte, error) {
	capture, err := s.Capture(ctx, e.RunID)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return nil, err
	}
	payloads := exportPayloads(e, capture)
	metadataOnly := payloads == "metadata"

	request := captureExportRequest{
		Method:        e.Method,
		URL:           e.URL,
		QueryRedacted: e.QueryRedacted,
	}
	if !metadataOnly {
		request.Headers = exportHeaderPairs(e.RequestHeaders)
		request.Body = exportBodyValue(e.RequestBody)
	}

	response := captureExportResponse{
		StatusCode: e.StatusCode,
		Error:      exportResponseError(e),
	}
	if !metadataOnly {
		response.Body = exportBodyValue(e.ResponseBody)
	}

	capturedAt := e.OccurredAt
	if capturedAt.IsZero() {
		capturedAt = e.IngestedAt
	}

	document := captureExportDocument{
		SchemaVersion: SchemaVersion,
		Kind:          "capture",
		RuntimeID:     s.runtimeID,
		RunID:         e.RunID,
		RequestID:     e.RequestID,
		CapturedAt:    exportTimestamp(capturedAt),
		Complete:      e.Complete,
		Payloads:      payloads,
		Request:       request,
		Response:      response,
		Timing: captureExportTiming{
			TotalMS:     e.DurationTotalMS,
			ToHeadersMS: e.DurationToHeadersMS,
		},
		CallSite: e.CallSite,
	}
	return json.Marshal(document)
}

// buildReplayDocument freezes one replay into capture-document-v1. The request
// side is the captured template (placeholders, not values); the response side is
// what the resend actually returned; resolved_sha256 is the digest of the bytes
// that were sent, which is the only trace of the resolved request Cloud keeps.
func (s *Store) buildReplayDocument(_ context.Context, replay *Replay) ([]byte, error) {
	payloads := "partial"
	if replay.ResponseBody != nil && replay.ResponseBody.State == BodyCaptured {
		payloads = "full"
	}
	response := captureExportResponse{
		StatusCode: replay.StatusCode,
		Body:       exportBodyValue(replay.ResponseBody),
	}
	if replay.TransportError != "" {
		response.Error = &captureExportError{
			Code:           "transport_error",
			Message:        bound(cleanText(replay.TransportError), 512),
			TransportClass: bound(cleanText(replay.TransportClass), 128),
		}
	}
	document := captureExportDocument{
		SchemaVersion: SchemaVersion,
		Kind:          "replay",
		RuntimeID:     s.runtimeID,
		RunID:         replay.RunID,
		RequestID:     replay.ReplayID,
		CapturedAt:    exportTimestamp(replay.OccurredAt),
		Complete:      true,
		Payloads:      payloads,
		Request: captureExportRequest{
			Method:        replay.Method,
			URL:           replay.URL,
			QueryRedacted: false,
			Headers:       exportHeaderPairs(replay.RequestHeaders),
			Body:          exportBodyValue(replay.RequestBody),
		},
		Response:       response,
		Timing:         captureExportTiming{TotalMS: replay.DurationTotalMS},
		ReplayOf:       replay.RequestID,
		ResolvedSHA256: replay.ResolvedSHA256,
	}
	return json.Marshal(document)
}

func (s *Store) buildManifestDocument(ctx context.Context, capture *RunCapture) ([]byte, error) {
	requests := make([]captureManifestRequest, 0, capture.RequestCount)
	after := int64(0)
	for {
		page, err := s.List(ctx, capture.RunID, after, 500)
		if err != nil {
			return nil, err
		}
		for _, item := range page {
			parsed, err := url.Parse(item.URL)
			if err != nil || parsed.Host == "" {
				return nil, fmt.Errorf("inspection: exchange %s has an unusable URL", item.RequestID)
			}
			path := parsed.EscapedPath()
			if path == "" {
				path = "/"
			}
			payloads := item.Payloads
			if payloads == "" {
				if capture.Policy == PolicyMetadata {
					payloads = "metadata"
				} else {
					payloads = "full"
				}
			}
			requests = append(requests, captureManifestRequest{
				RequestID:     item.RequestID,
				Method:        item.Method,
				Host:          parsed.Host,
				Path:          path,
				StatusCode:    item.StatusCode,
				Payloads:      payloads,
				At:            exportTimestamp(item.OccurredAt),
				QueryRedacted: item.QueryRedacted,
				Complete:      item.Complete,
			})
		}
		if len(page) < 500 {
			break
		}
		after = page[len(page)-1].ID
	}

	policy := string(capture.Policy)
	if policy == "" {
		policy = string(PolicyFull)
	}
	manifest := captureExportManifest{
		SchemaVersion: SchemaVersion,
		RuntimeID:     s.runtimeID,
		RunID:         capture.RunID,
		CapturePolicy: policy,
		Finalized:     true,
		Requests:      requests,
	}
	return json.Marshal(manifest)
}

// PullExports returns pending, due export rows assembled into documents.
//
// The cursor is a row id, not a time: two rows created in the same millisecond
// still have a stable order, and a batch is never re-read because a clock moved
// backwards.
func (s *Store) PullExports(ctx context.Context, after int64, limit int) (*ExportBatch, error) {
	if s.runtimeID == "" {
		return nil, fmt.Errorf("inspection: capture export needs a runtime id")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, run_id, kind, request_id
		   FROM capture_exports
		  WHERE state = 'pending' AND next_attempt_at <= ? AND rowid > ?
		  ORDER BY rowid ASC LIMIT ?`,
		time.Now().UTC().UnixMilli(), after, limit)
	if err != nil {
		return nil, fmt.Errorf("inspection: list pending exports: %w", err)
	}
	type queued struct {
		rowID     int64
		runID     string
		kind      string
		requestID string
	}
	var pending []queued
	for rows.Next() {
		var item queued
		if err := rows.Scan(&item.rowID, &item.runID, &item.kind, &item.requestID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("inspection: scan pending export: %w", err)
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("inspection: list pending exports: %w", err)
	}
	rows.Close()

	batch := &ExportBatch{Documents: make([]ExportDocument, 0, len(pending)), Cursor: after}
	for _, item := range pending {
		var document []byte
		switch ExportKind(item.kind) {
		case ExportExchange:
			exchange, err := s.Get(ctx, item.runID, item.requestID)
			if err != nil {
				return nil, err
			}
			document, err = s.buildExchangeDocument(ctx, exchange)
			if err != nil {
				return nil, err
			}
		case ExportReplay:
			replay, err := s.GetReplay(ctx, item.runID, item.requestID)
			if err != nil {
				return nil, err
			}
			document, err = s.buildReplayDocument(ctx, replay)
			if err != nil {
				return nil, err
			}
		case ExportManifest:
			capture, err := s.Capture(ctx, item.runID)
			if err != nil {
				return nil, err
			}
			document, err = s.buildManifestDocument(ctx, capture)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("inspection: unsupported export kind %q", item.kind)
		}
		batch.Documents = append(batch.Documents, ExportDocument{
			RowID:     item.rowID,
			RunID:     item.runID,
			Kind:      ExportKind(item.kind),
			RequestID: item.requestID,
			Document:  document,
		})
		batch.Cursor = item.rowID
	}
	return batch, nil
}

func exportBackoff(attempts int) time.Duration {
	delay := 5 * time.Second
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= 5*time.Minute {
			return 5 * time.Minute
		}
	}
	return delay
}

// AckExports records the agent's outcome for rows it was given.
//
// `retry` increments the attempt count and sets the next eligible time; at the
// limit the row is abandoned rather than blocking every later export. A missing
// row is ignored: an ack is idempotent, and a row that retention already removed
// is not an error the agent can act on.
func (s *Store) AckExports(ctx context.Context, refs []ExportRef, state, message string) error {
	if len(refs) == 0 {
		return nil
	}
	switch state {
	case ExportAckSent, ExportAckAbandoned, ExportAckRetry:
	default:
		return fmt.Errorf("inspection: unknown export ack state %q", state)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("inspection: begin export ack: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	for _, ref := range refs {
		if ref.Kind != ExportExchange && ref.Kind != ExportReplay && ref.Kind != ExportManifest {
			return fmt.Errorf("inspection: unknown export kind %q", ref.Kind)
		}
		switch state {
		case ExportAckSent:
			if _, err := tx.ExecContext(ctx,
				`UPDATE capture_exports
				    SET state = 'sent', attempts = attempts + 1, last_error = '', updated_at = ?
				  WHERE run_id = ? AND kind = ? AND request_id = ?`,
				now.UnixMilli(), ref.RunID, string(ref.Kind), ref.RequestID); err != nil {
				return fmt.Errorf("inspection: ack export sent: %w", err)
			}
		case ExportAckAbandoned:
			if _, err := tx.ExecContext(ctx,
				`UPDATE capture_exports
				    SET state = 'abandoned', last_error = ?, updated_at = ?
				  WHERE run_id = ? AND kind = ? AND request_id = ?`,
				bound(cleanText(message), 512), now.UnixMilli(),
				ref.RunID, string(ref.Kind), ref.RequestID); err != nil {
				return fmt.Errorf("inspection: ack export abandoned: %w", err)
			}
		case ExportAckRetry:
			var attempts int
			err := tx.QueryRowContext(ctx,
				`SELECT attempts FROM capture_exports
				  WHERE run_id = ? AND kind = ? AND request_id = ?`,
				ref.RunID, string(ref.Kind), ref.RequestID).Scan(&attempts)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspection: load export attempts: %w", err)
			}
			next := attempts + 1
			nextState := "pending"
			if next >= maxExportAttempts {
				nextState = "abandoned"
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE capture_exports
				    SET state = ?, attempts = ?, last_error = ?, next_attempt_at = ?, updated_at = ?
				  WHERE run_id = ? AND kind = ? AND request_id = ?`,
				nextState, next, bound(cleanText(message), 512),
				now.Add(exportBackoff(next)).UnixMilli(), now.UnixMilli(),
				ref.RunID, string(ref.Kind), ref.RequestID); err != nil {
				return fmt.Errorf("inspection: ack export retry: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("inspection: commit export ack: %w", err)
	}
	return nil
}
