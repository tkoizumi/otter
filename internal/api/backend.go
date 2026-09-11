package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/otter-runtime/otter/internal/runs"
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

	// SubmitRun queues a new run and returns its run id.
	SubmitRun(ctx context.Context, integrationID string, payload TriggerPayload) (string, error)

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
