package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
)

// Sentinel errors a Backend returns so the API layer can choose a status
// code. The daemon wraps these rather than leaking HTTP concepts inward.
var (
	ErrNotFound  = errors.New("not found")
	ErrInvalid   = errors.New("invalid request")
	ErrConflict  = errors.New("conflict")
	ErrForbidden = errors.New("forbidden")
)

// Backend is everything the HTTP API needs from the daemon. Keeping it as an
// interface means the api package never imports the daemon, so the daemon is
// free to import the api package to start the server.
type Backend interface {
	// Version and StartedAt describe the running daemon.
	Version() string
	StartedAt() time.Time

	// ListIntegrations returns every discovered integration. Webhook tokens
	// must be omitted here.
	ListIntegrations() []IntegrationView

	// GetIntegration returns one integration, including its webhook token.
	GetIntegration(id string) (IntegrationView, bool)

	// IntegrationGeneration reports the current identity generation of an
	// integration, so a per-run token issued against an older generation can
	// be refused at the point of mutation.
	IntegrationGeneration(id string) (int64, bool)

	// ResolveIntegration resolves a label, a path or an id to the integration
	// it names. It is how the CLI learns the durable identity of a target
	// without reading the registry itself.
	ResolveIntegration(ref string) (IntegrationView, error)

	// RegisterIntegration registers a source path explicitly, clearing any
	// deletion suppression. It never adopts a supplied marker into existing
	// state.
	RegisterIntegration(ctx context.Context, path string) (IntegrationView, error)

	// ResetIntegration retires an identity and mints a fresh one at the same
	// path, preserving the old data for explicit deletion.
	ResetIntegration(ctx context.Context, ref string) (ResetView, error)

	// DeleteIntegration purges an identity's state, history, tokens, releases
	// and owned environments. Source files are left in place. A retired or
	// already-deleted identity is still a legitimate target when named by id.
	DeleteIntegration(ctx context.Context, ref string) (DeletedView, error)

	// MoveIntegration preserves an identity across a same-filesystem rename.
	MoveIntegration(ctx context.Context, ref, destination string) (IntegrationView, error)

	// Reload re-reads the integrations directory and applies what it finds to
	// the running daemon, without stopping it. Executing runs and unchanged
	// cron schedules are left alone.
	Reload(ctx context.Context) (ReloadResult, error)

	// SubmitRun queues a new run with default submission options and returns
	// its run id.
	SubmitRun(ctx context.Context, integrationID string, payload TriggerPayload) (string, error)

	// SubmitRunWithOptions queues a new run with explicit options, such as the
	// HTTP capture policy.
	SubmitRunWithOptions(ctx context.Context, integrationID string, payload TriggerPayload, opts SubmitRunOptions) (string, error)

	// CancelRun cancels a queued or running run.
	CancelRun(ctx context.Context, runID string) error

	// GetRunDetail returns a run together with its retry chain.
	GetRunDetail(ctx context.Context, runID string) (*RunView, error)

	// ListRuns lists runs matching the filter.
	ListRuns(ctx context.Context, f runs.Filter) ([]*runs.Run, error)

	// RunLogs returns captured output for a run.
	RunLogs(ctx context.Context, runID string, afterID int64, limit int) ([]runs.LogEntry, error)

	// AppendRunLog appends a line of output, used by the Python SDK.
	AppendRunLog(ctx context.Context, runID, stream, message string, fields map[string]any) error

	// IngestCaptureEvents applies one batch of HTTP capture events submitted by a
	// running child. The integration identity is inferred from the run record and
	// never from the caller, so a run token cannot attribute traffic elsewhere.
	IngestCaptureEvents(ctx context.Context, runID string, batch inspection.EventBatch) (*inspection.IngestResult, error)

	// CaptureSummary returns a run's capture summary. A run that exists but has
	// no recording yields a summary whose State is "unavailable" rather than an
	// error, so a reader can tell "not recorded" apart from "recorded nothing".
	CaptureSummary(ctx context.Context, runID string) (*inspection.RunCapture, error)

	// ListCaptureRequests returns request summaries for a run, oldest first,
	// without loading any payload.
	ListCaptureRequests(ctx context.Context, runID string, afterID int64, limit int) ([]inspection.ExchangeSummary, error)

	// GetCaptureRequest returns one request together with its sanitized payloads.
	GetCaptureRequest(ctx context.Context, runID, requestID string) (*inspection.Exchange, error)

	// GetCaptureRequestByID returns one request together with its sanitized
	// payloads, resolving the owning run from storage rather than from the
	// caller. It reports inspection.ErrAmbiguous when more than one run recorded
	// the id, because the id alone does not identify a run.
	GetCaptureRequestByID(ctx context.Context, requestID string) (*inspection.Exchange, error)

	// GetState reads one state key.
	GetState(ctx context.Context, integrationID, key string) (json.RawMessage, error)

	// SetState writes one state key.
	SetState(ctx context.Context, integrationID, key string, value json.RawMessage) (time.Time, error)

	// DeleteState removes one state key.
	DeleteState(ctx context.Context, integrationID, key string) (bool, error)

	// AllState returns every state key for an integration.
	AllState(ctx context.Context, integrationID string) (map[string]json.RawMessage, error)

	// QueueDepth reports how many runs are waiting.
	QueueDepth(ctx context.Context) (int, error)

	// RunCounts reports how many runs exist per status.
	RunCounts(ctx context.Context) (map[string]int, error)

	// ResolveRunToken validates a per-run token issued to a child process.
	ResolveRunToken(token string) (RunToken, bool)

	// WebhookTokenFor returns the webhook token of an integration.
	WebhookTokenFor(integrationID string) (string, bool)
}
