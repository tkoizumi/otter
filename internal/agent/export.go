package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// The capture relay.
//
// The runtime assembles documents; the agent carries them out. The two halves
// are deliberately separate loops: an export outage must never delay a deploy or
// a read, and a deploy drain must never block the outbox. This loop runs beside
// the apply and control loops, over the same credentials and no shared lock.

// ExportDocument is one assembled capture document and the row it came from.
type ExportDocument struct {
	RowID     int64           `json:"row_id"`
	RunID     string          `json:"run_id"`
	Kind      string          `json:"kind"`
	RequestID string          `json:"request_id,omitempty"`
	Document  json.RawMessage `json:"document"`
}

// ExportRef identifies one row for an ack.
type ExportRef struct {
	RunID     string `json:"run_id"`
	Kind      string `json:"kind"`
	RequestID string `json:"request_id,omitempty"`
}

// Acknowledgment states the runtime's outbox understands.
const (
	ExportAckSent      = "sent"
	ExportAckAbandoned = "abandoned"
	ExportAckRetry     = "retry"
)

// ExportSource is the local runtime's export surface.
type ExportSource interface {
	PullExports(ctx context.Context, after int64, limit int) ([]ExportDocument, int64, error)
	AckExports(ctx context.Context, refs []ExportRef, state, message string) error
}

// CaptureUploader is the Cloud side of the relay. S5 implements the route; a
// failure here is what "a stopped Cloud backs up the outbox" means.
type CaptureUploader interface {
	UploadCapture(ctx context.Context, documents []ExportDocument) error
}

// PullExports implements ExportSource over the runtime's loopback API.
func (r *RuntimeHTTP) PullExports(ctx context.Context, after int64, limit int) ([]ExportDocument, int64, error) {
	var out struct {
		Documents []ExportDocument `json:"documents"`
		Cursor    int64            `json:"cursor"`
	}
	path := fmt.Sprintf("/v1/capture/exports?after=%d&limit=%d", after, limit)
	if err := r.do(ctx, "GET", path, nil, &out); err != nil {
		return nil, after, err
	}
	if out.Documents == nil {
		out.Documents = []ExportDocument{}
	}
	return out.Documents, out.Cursor, nil
}

// AckExports implements ExportSource over the runtime's loopback API.
func (r *RuntimeHTTP) AckExports(ctx context.Context, refs []ExportRef, state, message string) error {
	body := struct {
		Documents []ExportRef `json:"documents"`
		State     string      `json:"state"`
		Error     string      `json:"error,omitempty"`
	}{Documents: refs, State: state, Error: message}
	return r.do(ctx, "POST", "/v1/capture/exports/ack", body, nil)
}

// UploadCapture implements CaptureUploader over the agent's Cloud credential.
// The runtime id is sent for a cheap cross-check; Cloud derives the authoritative
// one from the credential and never trusts this field.
func (c *HTTPControlPlane) UploadCapture(ctx context.Context, documents []ExportDocument) error {
	if len(documents) == 0 {
		return nil
	}
	body := struct {
		RuntimeID string           `json:"runtime_id"`
		Documents []ExportDocument `json:"documents"`
	}{RuntimeID: c.RuntimeID, Documents: documents}
	err := c.post(ctx, "/agent/v1/capture", body, nil)
	if err == errNoDesiredWork {
		// A 204 from the ingest route is "accepted, nothing to say", not
		// "nothing was queued" in the sense that applies to desired work.
		return nil
	}
	return err
}

// ExportLoop drains the runtime's outbox to Cloud.
//
// Its failure policy is the same as the apply loop's: keep the data and try
// again. A failed upload is acked as a retry, so the runtime can back the row
// off and report the reason; the job that produced it has already finished.
type ExportLoop struct {
	Source   ExportSource
	Uploader CaptureUploader
	Logger   *slog.Logger
	// BatchSize caps one pull/upload. Zero means 100.
	BatchSize int
	// Interval is the idle poll. Zero means 15 seconds.
	Interval time.Duration
	// now, backoffBase and jitter are injectable for tests.
	now         func() time.Time
	backoffBase time.Duration
	jitter      func(time.Duration) time.Duration

	cursor int64
}

func (l *ExportLoop) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

func (l *ExportLoop) base() time.Duration {
	if l.backoffBase > 0 {
		return l.backoffBase
	}
	return 5 * time.Second
}

func (l *ExportLoop) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	return 15 * time.Second
}

func (l *ExportLoop) batchSize() int {
	if l.BatchSize > 0 {
		return l.BatchSize
	}
	return 100
}

func (l *ExportLoop) applyJitter(d time.Duration) time.Duration {
	if l.jitter != nil {
		return l.jitter(d)
	}
	return d
}

func (l *ExportLoop) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Run drains until the context is cancelled.
func (l *ExportLoop) Run(ctx context.Context) error {
	if l.Source == nil || l.Uploader == nil {
		return fmt.Errorf("agent: export loop needs a source and an uploader")
	}
	backoff := l.base()
	for {
		if ctx.Err() != nil {
			return nil
		}
		progressed, err := l.once(ctx)
		if err != nil {
			l.logger().Warn("agent_export_failed", "error", err.Error(), "retry_in", l.applyJitter(backoff).String())
			if !sleepCtx(ctx, l.applyJitter(backoff)) {
				return nil
			}
			backoff = bump(backoff)
			continue
		}
		backoff = l.base()
		if !progressed {
			if !sleepCtx(ctx, l.interval()) {
				return nil
			}
		}
	}
}

// Once performs one pull/upload/ack cycle. It is exported so `--once` and tests
// take the same path as the real loop.
func (l *ExportLoop) Once(ctx context.Context) error {
	_, err := l.once(ctx)
	return err
}

// once returns whether it uploaded a non-empty batch, so Run can tell "idle"
// from "there is probably more waiting" without a second pull.
func (l *ExportLoop) once(ctx context.Context) (bool, error) {
	documents, cursor, err := l.Source.PullExports(ctx, l.cursor, l.batchSize())
	if err != nil {
		return false, fmt.Errorf("pull capture exports: %w", err)
	}
	if len(documents) == 0 {
		return false, nil
	}
	refs := make([]ExportRef, 0, len(documents))
	for _, document := range documents {
		refs = append(refs, ExportRef{
			RunID:     document.RunID,
			Kind:      document.Kind,
			RequestID: document.RequestID,
		})
	}

	if err := l.Uploader.UploadCapture(ctx, documents); err != nil {
		// Tell the runtime why; it owns the backoff and may abandon a row that
		// will never succeed. A failed ack is only logged: the rows stay
		// pending, which is the safe direction.
		if ackErr := l.Source.AckExports(ctx, refs, ExportAckRetry, err.Error()); ackErr != nil {
			l.logger().Warn("agent_export_ack_failed", "error", ackErr.Error())
		}
		return false, fmt.Errorf("upload capture exports: %w", err)
	}
	if err := l.Source.AckExports(ctx, refs, ExportAckSent, ""); err != nil {
		return false, fmt.Errorf("ack capture exports: %w", err)
	}
	l.cursor = cursor
	return true, nil
}
