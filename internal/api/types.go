package api

import (
	"encoding/json"
	"time"

	"github.com/tkoizumi/otter/internal/runs"
)

// Trigger types accepted by SubmitRun.
const (
	TriggerManual  = runs.TriggerManual
	TriggerCron    = runs.TriggerCron
	TriggerWebhook = runs.TriggerWebhook
)

// TriggerPayload is what caused a run. The body and headers are recorded on
// the run so integration code can read ctx.trigger.body / .headers.
type TriggerPayload struct {
	Type    string              `json:"type"`
	Body    json.RawMessage     `json:"body,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`

	// ScheduledAt is set for cron runs.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`

	// WebhookToken is the token presented by the caller, used only for
	// authentication and never recorded on the run.
	WebhookToken string `json:"-"`
}

// IntegrationView is the API representation of an integration manifest.
type IntegrationView struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	Path             string            `json:"path"`
	Entrypoint       string            `json:"entrypoint"`
	PythonExecutable string            `json:"python_executable"`
	PythonMode       string            `json:"python_mode,omitempty"`
	PythonPath       []string          `json:"python_path,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	Concurrency      int               `json:"concurrency"`
	Retry            RetryView         `json:"retry"`
	Env              map[string]string `json:"env,omitempty"`
	Secrets          []string          `json:"secrets,omitempty"`
	Triggers         TriggerView       `json:"triggers"`
	Valid            bool              `json:"valid"`
	Error            string            `json:"error,omitempty"`
	NextRunAt        *time.Time        `json:"next_run_at,omitempty"`

	// Generation is the identity generation, bumped whenever authority is
	// revoked (reset, move, retirement, deletion). Status is the ownership
	// lifecycle. Both belong to the identity, not to the label.
	Generation int64  `json:"generation,omitempty"`
	Status     string `json:"status,omitempty"`
}

// RetryView describes an integration's retry policy.
type RetryView struct {
	Attempts     int    `json:"attempts"`
	MaxAttempts  int    `json:"max_attempts"`
	Backoff      string `json:"backoff"`
	InitialDelay string `json:"initial_delay"`
	MaxDelay     string `json:"max_delay"`
}

// TriggerView describes how an integration can be started.
type TriggerView struct {
	Cron           string `json:"cron,omitempty"`
	WebhookEnabled bool   `json:"webhook_enabled"`
	WebhookURL     string `json:"webhook_url,omitempty"`

	// WebhookToken is only populated on the single-integration endpoint, so
	// that listing integrations never spills credentials.
	WebhookToken string `json:"webhook_token,omitempty"`
}

// ResetView reports the identity change a reset performed. The old identity is
// retired but its data is kept until an explicit delete.
type ResetView struct {
	OldID string `json:"old_id"`
	NewID string `json:"new_id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}

// DeletedView reports an identity that was purged. The source directory is
// left in place and its path is suppressed, so the name is included for the
// operator even though the id is what was deleted.
type DeletedView struct {
	Deleted bool   `json:"deleted"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
}

// RegisterRequest is the body of POST /v1/integrations.
type RegisterRequest struct {
	Path string `json:"path"`
}

// MoveRequest is the body of POST /v1/integrations/{id}/move.
type MoveRequest struct {
	Destination string `json:"destination"`
}

// RunView augments a run with the state of its whole retry chain.
type RunView struct {
	*runs.Run

	RootRunID    string      `json:"root_run_id"`
	LatestStatus runs.Status `json:"latest_status"`
	Attempts     []*runs.Run `json:"attempts"`
}

// HealthResponse is returned by GET /health.
//
// Integrations, QueueDepth and Runs are present only for an authenticated
// caller: an unauthenticated liveness probe receives status, version and
// uptime alone.
type HealthResponse struct {
	Status        string         `json:"status"`
	Version       string         `json:"version"`
	UptimeSeconds float64        `json:"uptime_seconds"`
	Integrations  *HealthCounts  `json:"integrations,omitempty"`
	QueueDepth    *int           `json:"queue_depth,omitempty"`
	Runs          map[string]int `json:"runs,omitempty"`
}

// HealthCounts summarises discovered integrations.
type HealthCounts struct {
	Total   int `json:"total"`
	Valid   int `json:"valid"`
	Invalid int `json:"invalid"`
}

// RunToken is the scope granted to a per-run token handed to a child process.
// It is deliberately narrow: it can read its own run, read and write state
// for its own integration and append its own logs, nothing more.
//
// Generation is the identity generation the run was authorized against. It is
// carried through the authorization boundary into state mutation so that a
// token issued before a reset, move, retirement or deletion cannot write to
// state that now belongs to a different instance.
type RunToken struct {
	RunID         string `json:"run_id"`
	IntegrationID string `json:"integration_id"`
	Generation    int64  `json:"generation,omitempty"`
}

// StateResponse is returned by the whole-state endpoint.
type StateResponse struct {
	State map[string]json.RawMessage `json:"state"`
}

// SetStateResponse is returned by PUT state.
type SetStateResponse struct {
	IntegrationID string          `json:"integration_id"`
	Key           string          `json:"key"`
	Value         json.RawMessage `json:"value"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// DeleteStateResponse is returned by DELETE state.
type DeleteStateResponse struct {
	IntegrationID string `json:"integration_id"`
	Key           string `json:"key"`
	Deleted       bool   `json:"deleted"`
}

// SubmitRunResponse is returned when a run is accepted.
type SubmitRunResponse struct {
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// CancelRunResponse is returned when a cancellation is accepted.
type CancelRunResponse struct {
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ReloadResult reports what a reload changed.
//
// It is returned rather than only logged because an operator who edited one
// manifest wants to see that one integration changed, not merely that the
// whole directory was re-read.
type ReloadResult struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
	Invalid []string `json:"invalid"`

	// Total and Valid describe the integration set after the reload.
	Total int `json:"total"`
	Valid int `json:"valid"`

	// RunsCancelled counts the queued runs of removed integrations that were
	// ended, because a removed integration can never execute them.
	RunsCancelled int `json:"runs_cancelled"`
}

// AppendLogRequest is the body of POST /v1/runs/{id}/logs.
type AppendLogRequest struct {
	Stream  string         `json:"stream"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// ErrorResponse is the body of every error the API returns.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes a failure.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes used by the API.
const (
	CodeNotFound     = "not_found"
	CodeInvalid      = "invalid_request"
	CodeConflict     = "conflict"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeInternal     = "internal_error"
	CodeUnavailable  = "unavailable"
)
