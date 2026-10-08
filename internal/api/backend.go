package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

	// ErrGated refuses *every* path that would admit work because the runtime
	// itself is under maintenance. It is deliberately not ErrPaused: a pause
	// is one job's triggers and a manual run still gets through, while
	// maintenance is the whole runtime and nothing does. 503 with a
	// machine-readable reason is the answer, and the reason tells the operator
	// how to activate the runtime.
	ErrGated = errors.New("maintenance")

	// ErrOverloaded refuses an autonomous trigger because the job's own
	// max_queue_depth is already reached. Like ErrPaused it describes a
	// temporary condition, but the answer differs: the queue drains on its own,
	// so this maps to 429 with a retry hint rather than 503. A manual run is
	// never refused with this error -- an operator asking for one run is not
	// the backlog the bound exists to cap.
	ErrOverloaded = errors.New("overloaded")
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

	// SetSchedule replaces a job's single cadence, or clears it when cron is
	// empty. It is the deprecated v0.3.0 surface, retained so existing clients
	// keep working; new callers use the schedule-id endpoints below.
	SetSchedule(ctx context.Context, ref, cron string) (ScheduleView, error)

	// ListSchedules returns every schedule a job holds, whatever its origin.
	ListSchedules(ctx context.Context, ref string) ([]ScheduleView, error)

	// CreateSchedule adds an API-owned schedule to a job. When idempotencyKey
	// is non-empty and already exists, the existing schedule is returned with
	// Changed false rather than a second schedule being created.
	CreateSchedule(ctx context.Context, ref string, req ScheduleCreateRequest, idempotencyKey string) (ScheduleView, error)

	// GetSchedule returns one schedule by id.
	GetSchedule(ctx context.Context, scheduleID string) (ScheduleView, error)

	// UpdateSchedule changes an API-owned schedule. A manifest-owned row is
	// refused with ErrConflict.
	UpdateSchedule(ctx context.Context, scheduleID string, req ScheduleUpdateRequest) (ScheduleView, error)

	// DeleteSchedule removes an API-owned schedule. A manifest-owned row is
	// refused with ErrConflict.
	DeleteSchedule(ctx context.Context, scheduleID string) error

	// SetSchedulePaused pauses or resumes one schedule. Pausing is an operator
	// control, so it is accepted on a manifest-owned row too.
	SetSchedulePaused(ctx context.Context, scheduleID string, paused bool) (ScheduleView, error)

	// GetJobConfig returns a job's current configuration: the version a new run
	// would pin, and its values. A job with none yields an empty object.
	GetJobConfig(ctx context.Context, ref string) (JobConfigView, error)

	// SetJobConfig writes a new immutable configuration version and points the
	// job at it. Already-accepted runs keep the version they pinned. by is the
	// audit label of the caller.
	SetJobConfig(ctx context.Context, ref string, values json.RawMessage, by string) (JobConfigView, error)

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

	// AdmissionRefusals reports, per job, how many autonomous triggers a job's
	// max_queue_depth refused and when the most recent refusal happened. The
	// counters are in-process, so they are empty after a restart.
	AdmissionRefusals(ctx context.Context) (map[string]AdmissionRefusal, error)

	// Maintenance reports the runtime's maintenance state: whether it accepts
	// work, how much is still running, and why it is held back.
	Maintenance(ctx context.Context) (MaintenanceView, error)

	// EnterMaintenance closes every path that admits work. It returns the
	// resulting state, so a caller learns what mode it landed in rather than
	// assuming.
	EnterMaintenance(ctx context.Context, reason string) (MaintenanceView, error)

	// ExitMaintenance activates the runtime: maintenance is cleared and work is
	// accepted again. It must not be reachable by a read, control or capture
	// credential, only by an admin one.
	ExitMaintenance(ctx context.Context) (MaintenanceView, error)

	// ActivateRelease makes a verified release active by digest, so a runtime
	// agent can promote a release through the API an operator uses rather than
	// reaching past it into the data directory. It refuses while the runtime is
	// serving: swapping the active release under running work is the failure the
	// maintenance gate exists to prevent.
	ActivateRelease(ctx context.Context, digest string) (ReleaseView, error)

	// InstallRelease verifies and installs a portable release package.
	//
	// It takes the package bytes rather than a path, because VERIFICATION is the
	// runtime's job: Cloud stores and distributes releases but does not define
	// release identity, and an uploaded package must not land in the store under a
	// name that was merely asserted.
	InstallRelease(ctx context.Context, r io.Reader) (ReleaseView, error)

	// ActiveReleases reports which release each job is CURRENTLY serving.
	//
	// It exists because the control plane's unknown-outcome rule needs evidence,
	// and before this there was no way to ask a runtime what it was actually
	// running: activation was write-only, so an agent could only CLAIM what it
	// had applied. A claim is not evidence -- that distinction is the whole point
	// of the rule -- so the runtime has to be able to answer.
	//
	// A SET rather than one digest, because the active release is per JOB and one
	// runtime manages several. A single value could not express that, and
	// answering with an arbitrary job's release would be worse than not answering.
	ActiveReleases(ctx context.Context) ([]ReleaseView, error)

	// ManagedReleaseJobs reports the jobs this runtime holds ONLY in its release
	// store: those with no source in the jobs directory, which is the shape a
	// Cloud deploy leaves behind on a pooled tenant.
	//
	// It is the runtime's own answer to "which jobs does a control plane own
	// here", and it is what lets an agent reconcile a deleted job away WITHOUT
	// touching a job whose source lives in the workspace. A job with a source is
	// never in this list, so the agent never has to guess.
	ManagedReleaseJobs(ctx context.Context) ([]string, error)

	// ScheduleCounters reports the per-schedule missed-occurrence accounting:
	// occurrences folded by coalesce, and occurrences a bounded catch-up
	// declined to replay. Only schedules with a non-zero counter are returned.
	// In-process, so a restart starts the counts again.
	ScheduleCounters(ctx context.Context) ([]HealthSchedule, error)

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

	// CreateAPIToken mints a named, scoped operator token. The plaintext is
	// returned once, here; only its hash is stored.
	CreateAPIToken(ctx context.Context, name string, scope Scope) (APITokenCreated, error)

	// ListAPITokens returns every named token, newest first, including revoked
	// ones. It never reveals a token.
	ListAPITokens(ctx context.Context) ([]APITokenView, error)

	// RevokeAPIToken withdraws a token, reporting whether the id is known.
	// Revoking an already-revoked token succeeds.
	RevokeAPIToken(ctx context.Context, id string) (bool, error)

	// ResolveAPIToken validates a presented named token. A revoked token
	// resolves to false on the next call, which is what makes revocation
	// immediate.
	ResolveAPIToken(token string) (APIToken, bool)
}
