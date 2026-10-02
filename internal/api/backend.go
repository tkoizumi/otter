package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// Sentinel errors a Backend returns so the API layer can choose a status
// code. The daemon wraps these rather than leaking HTTP concepts inward.
var (
	ErrNotFound  = errors.New("not found")
	ErrInvalid   = errors.New("invalid request")
	ErrConflict  = errors.New("conflict")
	ErrForbidden = errors.New("forbidden")

	// ErrPaused refuses an autonomous trigger -- cron or webhook -- for an
	// job the operator paused. It is deliberately separate from
	// ErrConflict so the API can answer "temporarily not accepting triggers"
	// rather than "the request contradicts the job's state". A manual
	// run is never refused with this error.
	ErrPaused = errors.New("paused")
)

// Backend is everything the HTTP API needs from the daemon. Keeping it as an
// interface means the api package never imports the daemon, so the daemon is
// free to import the api package to start the server.
type Backend interface {
	// Version and StartedAt describe the running daemon.
	Version() string
	StartedAt() time.Time

	// ListJobs returns every discovered job, each carrying its last successful
	// completion so a caller can render freshness without a run listing per
	// job. Webhook tokens must be omitted here.
	ListJobs(ctx context.Context) []JobView

	// GetJob returns one job, including its webhook token.
	GetJob(id string) (JobView, bool)

	// JobGeneration reports the current identity generation of an
	// job, so a per-run token issued against an older generation can
	// be refused at the point of mutation.
	JobGeneration(id string) (int64, bool)

	// ResolveJob resolves a label, a path or an id to the job
	// it names. It is how the CLI learns the durable identity of a target
	// without reading the registry itself.
	ResolveJob(ref string) (JobView, error)

	// RegisterJob registers a source path explicitly, clearing any
	// deletion suppression. It never adopts a supplied marker into existing
	// state.
	RegisterJob(ctx context.Context, path string) (JobView, error)

	// ResetJob retires an identity and mints a fresh one at the same
	// path, preserving the old data for explicit deletion.
	ResetJob(ctx context.Context, ref string) (ResetView, error)

	// DeleteJob purges an identity's state, history, tokens, releases
	// and owned environments. Source files are left in place. A retired or
	// already-deleted identity is still a legitimate target when named by id.
	DeleteJob(ctx context.Context, ref string) (DeletedView, error)

	// MoveJob preserves an identity across a same-filesystem rename.
	MoveJob(ctx context.Context, ref, destination string) (JobView, error)

	// SetPaused suspends or re-arms a job's autonomous triggers. It
	// accepts a reference, so `otter pause` works from the job's own
	// directory. Manual runs are unaffected in both directions.
	SetPaused(ctx context.Context, ref string, paused bool) (PauseView, error)

	// SetSchedule replaces a job's cadence, or clears it when cron is empty.
	// A cleared schedule is a deliberate "never fire on its own" and does not
	// fall back to the manifest, so a reload cannot resurrect it. Manual runs
	// are unaffected.
	SetSchedule(ctx context.Context, ref, cron string) (ScheduleView, error)

	// Reload re-reads the jobs directory and applies what it finds to
	// the running daemon, without stopping it. Executing runs and unchanged
	// cron schedules are left alone.
	Reload(ctx context.Context) (ReloadResult, error)

	// SubmitRun queues a new run with default submission options and returns
	// its run id.
	SubmitRun(ctx context.Context, jobID string, payload TriggerPayload) (string, error)

	// SubmitRunWithOptions queues a new run with explicit options, such as the
	// HTTP capture policy.
	SubmitRunWithOptions(ctx context.Context, jobID string, payload TriggerPayload, opts SubmitRunOptions) (string, error)

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
	// running child. The job identity is inferred from the run record and
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

	// TimelinePage assembles one page of a finished run's merged timeline. It
	// refuses an attempt that has not finished, so a reader is never handed a
	// chronology that is still changing underneath it.
	TimelinePage(ctx context.Context, req timeline.Request) (*timeline.Page, error)

	// GetState reads one state key.
	GetState(ctx context.Context, jobID, key string) (json.RawMessage, error)

	// SetState writes one state key.
	SetState(ctx context.Context, jobID, key string, value json.RawMessage) (time.Time, error)

	// DeleteState removes one state key.
	DeleteState(ctx context.Context, jobID, key string) (bool, error)

	// AllState returns every state key for a job.
	AllState(ctx context.Context, jobID string) (map[string]json.RawMessage, error)

	// QueueDepth reports how many runs are waiting.
	QueueDepth(ctx context.Context) (int, error)

	// QueueStats reports queue age, per-job depth and retry activity for the
	// authenticated health view.
	QueueStats(ctx context.Context) (QueueStats, error)

	// LastSuccessByJob reports each job's most recent successful completion,
	// keyed by job id. A job absent from the map has never succeeded.
	LastSuccessByJob(ctx context.Context) (map[string]time.Time, error)

	// StorageStats reports the database size and the data directory's
	// free and total bytes.
	StorageStats(ctx context.Context) (StorageStats, error)

	// RunCounts reports how many runs exist per status.
	RunCounts(ctx context.Context) (map[string]int, error)

	// ResolveRunToken validates a per-run token issued to a child process.
	ResolveRunToken(token string) (RunToken, bool)

	// WebhookTokenFor returns the webhook token of a job.
	WebhookTokenFor(jobID string) (string, bool)
}
