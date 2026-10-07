package api

import (
	"encoding/json"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// Trigger types accepted by SubmitRun.
const (
	TriggerManual  = runs.TriggerManual
	TriggerCron    = runs.TriggerCron
	TriggerWebhook = runs.TriggerWebhook
)

// TriggerPayload is what caused a run. The body and headers are recorded on
// the run so job code can read ctx.trigger.body / .headers.
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

// JobView is the API representation of a job manifest.
type JobView struct {
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

	// LastSuccessAt is when this job last completed a run successfully. It is
	// the freshness signal, and the schedule view renders it, so a cron job
	// that quietly stopped succeeding is visible without a run listing per
	// job. It is absent for a job that has never succeeded.
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`

	// Capture is the HTTP capture policy a new run of this job would
	// use: the job's declared policy, or the deployment default when the
	// manifest does not declare one.
	Capture string `json:"capture"`

	// Generation is the identity generation, bumped whenever authority is
	// revoked (reset, move, retirement, deletion). Status is the ownership
	// lifecycle. Both belong to the identity, not to the label.
	Generation int64  `json:"generation,omitempty"`
	Status     string `json:"status,omitempty"`
}

// RetryView describes a job's retry policy.
type RetryView struct {
	Attempts     int    `json:"attempts"`
	MaxAttempts  int    `json:"max_attempts"`
	Backoff      string `json:"backoff"`
	InitialDelay string `json:"initial_delay"`
	MaxDelay     string `json:"max_delay"`
}

// TriggerView describes how a job can be started.
type TriggerView struct {
	Cron           string `json:"cron,omitempty"`
	WebhookEnabled bool   `json:"webhook_enabled"`
	WebhookURL     string `json:"webhook_url,omitempty"`

	// WebhookToken is only populated on the single-job endpoint, so
	// that listing jobs never spills credentials.
	WebhookToken string `json:"webhook_token,omitempty"`

	// Paused suspends every autonomous trigger of this job: cron stops
	// firing and the webhook refuses a trigger. Manual runs are unaffected, so
	// a paused job can still be run on demand. PausedAt answers "since
	// when".
	Paused   bool       `json:"paused"`
	PausedAt *time.Time `json:"paused_at,omitempty"`
}

// PauseView reports a job's trigger state after a pause or resume.
//
// Changed distinguishes a fresh transition from a repeat, which is what lets a
// deploy script call pause unconditionally and still tell whether it did
// anything.
type PauseView struct {
	JobID   string     `json:"job_id"`
	Name    string     `json:"name,omitempty"`
	Paused  bool       `json:"paused"`
	Changed bool       `json:"changed"`
	Since   *time.Time `json:"since,omitempty"`
}

// ScheduleRequest is the body of a schedule change. Cron is a standard
// five-field expression; an empty value clears the schedule.
//
// It belongs to the deprecated single-cadence endpoint. New callers use
// ScheduleCreateRequest and ScheduleUpdateRequest against /v1/schedules.
type ScheduleRequest struct {
	Cron string `json:"cron"`
}

// ScheduleCreateRequest is the body of POST /v1/jobs/{id}/schedules.
//
// Cron is required. Timezone defaults to UTC; Payload defaults to an empty
// object and must be a JSON object of at most 64 KiB. MissedPolicy defaults to
// skip and is recorded for forward compatibility.
type ScheduleCreateRequest struct {
	Cron         string          `json:"cron"`
	Timezone     string          `json:"timezone,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	MissedPolicy string          `json:"missed_policy,omitempty"`
}

// ScheduleUpdateRequest is the body of PATCH /v1/schedules/{schedule_id}. A nil
// field is left alone; a caller cannot clear a schedule with an empty cron.
type ScheduleUpdateRequest struct {
	Cron         *string          `json:"cron,omitempty"`
	Timezone     *string          `json:"timezone,omitempty"`
	Payload      *json.RawMessage `json:"payload,omitempty"`
	MissedPolicy *string          `json:"missed_policy,omitempty"`
}

// ScheduleList is the response of GET /v1/jobs/{id}/schedules.
type ScheduleList struct {
	// SchemaVersion versions the JSON shape itself; see compatibility.md.
	SchemaVersion int            `json:"schema_version"`
	Schedules     []ScheduleView `json:"schedules"`
}

// JobConfigView is a job's configuration: the immutable version a new run would
// pin, and its values.
//
// Values is always an object, `{}` when the job has none, so a reader never has
// to tell "no configuration" from "null". ConfigVersion is empty in that case.
// Configuration values are not secrets and must never hold one.
type JobConfigView struct {
	SchemaVersion int             `json:"schema_version"`
	JobID         string          `json:"job_id"`
	Name          string          `json:"name,omitempty"`
	ConfigVersion string          `json:"config_version,omitempty"`
	Values        json.RawMessage `json:"values"`
	UpdatedAt     *time.Time      `json:"updated_at,omitempty"`
	UpdatedBy     string          `json:"updated_by,omitempty"`
	Changed       bool            `json:"changed"`
}

// JobConfigRequest is the body of PUT /v1/jobs/{id}/config. Values is the new
// configuration object; setting the current values again is a no-op.
type JobConfigRequest struct {
	Values json.RawMessage `json:"values"`
}

// JobList is the response of GET /v1/jobs. A list response is an object, never
// a bare array, so it can carry the schema version.
type JobList struct {
	SchemaVersion int       `json:"schema_version"`
	Jobs          []JobView `json:"jobs"`
}

// RunList is the response of GET /v1/runs.
type RunList struct {
	SchemaVersion int         `json:"schema_version"`
	Runs          []*runs.Run `json:"runs"`
}

// LogList is the response of GET /v1/runs/{id}/logs.
type LogList struct {
	SchemaVersion int             `json:"schema_version"`
	Logs          []runs.LogEntry `json:"logs"`
}

// ScheduleView reports one schedule.
//
// Origin names what owns the row: a manifest-owned schedule refuses PATCH and
// DELETE with 409, while an api-owned one is the caller's to change. Cron is
// empty only for a cadence cleared through the deprecated endpoint. NextRunAt is
// absent when nothing is armed -- a cleared cron, a paused schedule, or a paused
// job -- so a caller cannot mistake a stored cadence for a trigger that will
// actually fire.
type ScheduleView struct {
	ID           string          `json:"id,omitempty"`
	JobID        string          `json:"job_id"`
	Name         string          `json:"name,omitempty"`
	Cron         string          `json:"cron"`
	Timezone     string          `json:"timezone,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	MissedPolicy string          `json:"missed_policy,omitempty"`
	Origin       string          `json:"origin,omitempty"`
	NextRunAt    *time.Time      `json:"next_run_at,omitempty"`
	LastFiredAt  *time.Time      `json:"last_fired_at,omitempty"`
	Paused       bool            `json:"paused,omitempty"`
	PausedAt     *time.Time      `json:"paused_at,omitempty"`
	Changed      bool            `json:"changed"`
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

// RegisterRequest is the body of POST /v1/jobs.
type RegisterRequest struct {
	Path string `json:"path"`
}

// MoveRequest is the body of POST /v1/jobs/{id}/move.
type MoveRequest struct {
	Destination string `json:"destination"`
}

// RunView augments a run with the state of its whole retry chain.
type RunView struct {
	*runs.Run

	RootRunID    string      `json:"root_run_id"`
	LatestStatus runs.Status `json:"latest_status"`
	Attempts     []*runs.Run `json:"attempts"`

	// MaxAttempts is the retry ceiling the job's manifest currently
	// allows, including the first attempt. It is 0 when the job is no
	// longer registered, so a caller watching a failed chain can tell "the
	// policy has given up" (the newest attempt is at the ceiling) apart from
	// "a retry is still coming" -- a distinction the statuses alone cannot make
	// until the successor attempt exists.
	MaxAttempts int `json:"max_attempts,omitempty"`

	// CollapsedOccurrences is how many schedule occurrences this attempt
	// stands for beyond its own, recorded when a coalesce policy folded them
	// into it. Zero for an ordinary run, and omitted then, so the common case
	// is unchanged. It is how a run that represents five fires says so.
	CollapsedOccurrences int `json:"collapsed_occurrences,omitempty"`
}

// CollapsedOccurrences reads the folded-occurrence count a coalesce recorded in
// a run's trigger metadata. It is exported so the daemon and any other reader
// agree on the shape: metadata["coalesced"]["count"] on an uptime fold, and
// metadata["coalesced"]["missed"] on one that absorbed a downtime window.
func CollapsedOccurrences(metadata json.RawMessage) int {
	if len(metadata) == 0 {
		return 0
	}
	var meta struct {
		Coalesced struct {
			Count  *int `json:"count"`
			Missed *int `json:"missed"`
		} `json:"coalesced"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return 0
	}
	if meta.Coalesced.Count != nil {
		return *meta.Coalesced.Count
	}
	if meta.Coalesced.Missed != nil {
		return *meta.Coalesced.Missed
	}
	return 0
}

// HealthResponse is returned by GET /health.
//
// Jobs, QueueDepth, Runs, Queue, Freshness and Storage are present only for an
// authenticated caller: an unauthenticated liveness probe receives status,
// version and uptime alone.
type HealthResponse struct {
	// SchemaVersion versions the JSON shape itself; see compatibility.md.
	SchemaVersion int            `json:"schema_version"`
	Status        string         `json:"status"`
	Version       string         `json:"version"`
	UptimeSeconds float64        `json:"uptime_seconds"`
	Jobs          *HealthCounts  `json:"jobs,omitempty"`
	QueueDepth    *int           `json:"queue_depth,omitempty"`
	Runs          map[string]int `json:"runs,omitempty"`

	// Queue, Freshness and Storage answer what the counts cannot: how long
	// the oldest claimable run has waited, which job is backing up, whether
	// each job is still succeeding, and whether the disk is filling.
	Queue     *HealthQueue      `json:"queue,omitempty"`
	Freshness []HealthFreshness `json:"freshness,omitempty"`
	Storage   *HealthStorage    `json:"storage,omitempty"`

	// Maintenance is present for an authenticated caller so a monitor can tell
	// "held back on purpose" from "broken" without a second request.
	Maintenance *MaintenanceView `json:"maintenance,omitempty"`

	// Schedules reports the per-schedule counters that are not derivable from
	// runs: how often a schedule folded occurrences, and how much a bounded
	// catch-up declined to replay. Only schedules with a non-zero counter
	// appear, so the block is absent on a runtime that never uses the
	// non-default policies.
	Schedules []HealthSchedule `json:"schedules,omitempty"`
}

// HealthCounts summarises discovered jobs.
type HealthCounts struct {
	Total   int `json:"total"`
	Valid   int `json:"valid"`
	Invalid int `json:"invalid"`
}

// HealthQueue is the queue's age and retry picture. The total depth stays in
// queue_depth so an existing reader keeps working; this adds what one integer
// cannot express.
type HealthQueue struct {
	// OldestWaitingAt is the submission time of the oldest run that is
	// claimable now, and OldestWaitingSeconds its age. Both are absent when
	// nothing is claimable.
	OldestWaitingAt      *time.Time `json:"oldest_waiting_at,omitempty"`
	OldestWaitingSeconds *float64   `json:"oldest_waiting_seconds,omitempty"`

	// ByJob is the queue depth per job, so "which job is backing up?" has an
	// answer.
	ByJob map[string]int `json:"by_job,omitempty"`

	// Retrying counts runs parked by retry backoff, and NextRetryAt is when
	// the soonest of them becomes claimable. They are deliberately separate
	// from the age above: a retry is waiting on a clock, not on capacity.
	Retrying    int        `json:"retrying"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`

	// Refusals is the max_queue_depth picture: a refusal is a run that did not
	// happen, so it is reported beside the depth it was refused at rather than
	// only in the journal. Absent until a job has refused something.
	RefusedTotal  int64                       `json:"refused_total,omitempty"`
	LastRefusedAt *time.Time                  `json:"last_refused_at,omitempty"`
	ByJobRefused  map[string]AdmissionRefusal `json:"refusals_by_job,omitempty"`
}

// HealthSchedule is one schedule's missed-occurrence accounting. Both counters
// are lifetime totals for this process, so they answer "is this schedule
// folding work away, or did catch-up truncate?" without reading the journal.
// A schedule with nothing to report is absent rather than present as zeros.
type HealthSchedule struct {
	ScheduleID string `json:"schedule_id"`
	JobID      string `json:"job_id"`
	Name       string `json:"name,omitempty"`
	// CoalescedTotal counts occurrences folded into a run that was already
	// pending, or absorbed by a coalesce after downtime.
	CoalescedTotal int64 `json:"coalesced_total"`
	// CatchUpSkippedTotal counts occurrences a bounded catch-up declined to
	// replay, so a truncated backlog is visible rather than implied.
	CatchUpSkippedTotal int64 `json:"catch_up_skipped_total"`
}

// HealthFreshness is one job's last success. LastSuccessAt and AgeSeconds are
// absent when the job has never succeeded, which is itself the signal: a cron
// job that has never succeeded is not a job that is working.
type HealthFreshness struct {
	JobID         string     `json:"job_id"`
	Name          string     `json:"name,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	AgeSeconds    *float64   `json:"age_seconds,omitempty"`
}

// HealthStorage reports the database's size and the data directory's free
// space. It is computed daemon-side so a remote operator sees it through the
// API instead of needing a shell on the host.
//
// DBBytes is page_count * page_size, so it excludes the WAL file. The disk
// fields are 0 when the platform cannot report them.
type HealthStorage struct {
	DBBytes        int64 `json:"db_bytes"`
	DiskFreeBytes  int64 `json:"disk_free_bytes"`
	DiskTotalBytes int64 `json:"disk_total_bytes"`
}

// QueueStats is the raw queue data the daemon reports to the API. The times
// stay instants here; the API layer turns them into ages, because "now" belongs
// to the response being written, not to the store that was read.
type QueueStats struct {
	ByJob           map[string]int
	OldestWaitingAt *time.Time
	Retrying        int
	NextRetryAt     *time.Time
}

// AdmissionRefusal is one job's max_queue_depth accounting: how many autonomous
// triggers it refused since the daemon started, and when the most recent one
// happened. It is deliberately not persisted -- a restart is a new process, and
// the queue it was protecting may have drained in between -- so this answers
// "is a job being held back right now?" rather than "how often, ever?".
type AdmissionRefusal struct {
	Total int64 `json:"refused_total"`
	// LastAt is when the most recent refusal happened. Absent when Total is 0.
	LastAt *time.Time `json:"last_refused_at,omitempty"`
}

// MaintenanceView is what GET /v1/runtime/maintenance returns and what /health
// embeds. It answers the three questions an operator has during a window:
// whether the runtime is accepting work, whether anything is still running, and
// how long it has been held back.
type MaintenanceView struct {
	// Mode is serving, startup, draining or maintenance.
	Mode string `json:"mode"`
	// AcceptingWork is the question callers actually ask, stated directly so a
	// reader does not have to know that draining and maintenance both refuse.
	AcceptingWork bool `json:"accepting_work"`
	// Running is how many runs are still executing. It is what separates a
	// drain that is progressing from one that is stuck.
	Running int `json:"running"`
	// Explicit is true when an operator decided this state, as distinct from a
	// process that started gated and has not been activated.
	Explicit bool `json:"explicit"`
	// Since is when the current state began, so "held back for 40 minutes" is
	// readable rather than derived from a log search.
	Since string `json:"since,omitempty"`
	// Reason is the operator's own text, echoed back for the audit trail.
	Reason string `json:"reason,omitempty"`
}

// StorageStats is the raw storage data the daemon reports to the API.
type StorageStats struct {
	DBBytes        int64
	DiskFreeBytes  int64
	DiskTotalBytes int64
}

// RunToken is the scope granted to a per-run token handed to a child process.
// It is deliberately narrow: it can read its own run, read and write state
// for its own job and append its own logs, nothing more.
//
// Generation is the identity generation the run was authorized against. It is
// carried through the authorization boundary into state mutation so that a
// token issued before a reset, move, retirement or deletion cannot write to
// state that now belongs to a different instance.
type RunToken struct {
	RunID      string `json:"run_id"`
	JobID      string `json:"job_id"`
	Generation int64  `json:"generation,omitempty"`
}

// StateResponse is returned by the whole-state endpoint.
type StateResponse struct {
	State map[string]json.RawMessage `json:"state"`
}

// SetStateResponse is returned by PUT state.
type SetStateResponse struct {
	JobID     string          `json:"job_id"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// DeleteStateResponse is returned by DELETE state.
type DeleteStateResponse struct {
	JobID   string `json:"job_id"`
	Key     string `json:"key"`
	Deleted bool   `json:"deleted"`
}

// SubmitRunResponse is returned when a run is accepted.
type SubmitRunResponse struct {
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// SubmitRunOptions carries submission settings that are not part of the trigger.
//
// They are deliberately not smuggled through the request body: that body is the
// trigger JSON, recorded verbatim and handed to job code, so mixing
// runtime options into it would corrupt both.
type SubmitRunOptions struct {
	// Capture is the HTTP capture policy for the run. Empty means the default.
	Capture inspection.Policy

	// Metadata merges into the run's trigger metadata alongside the payload.
	// It is how a runtime-side scheduling decision records what it decided --
	// the bounds of a coalesced window, for instance -- so the run explains
	// itself instead of the accounting living only in a log line.
	Metadata map[string]any

	// IdempotencyKey is a caller-derived key scoped to one job. A second
	// submission with the same key returns the FIRST run's id instead of
	// creating another run, which is what makes an at-least-once control
	// delivery safe. Empty means no deduplication, which is the historical
	// behaviour for a trigger that has no caller to retry it.
	IdempotencyKey string
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
// manifest wants to see that one job changed, not merely that the
// whole directory was re-read.
type ReloadResult struct {
	// SchemaVersion versions the JSON shape itself; see compatibility.md.
	SchemaVersion int `json:"schema_version"`

	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
	Invalid []string `json:"invalid"`

	// Total and Valid describe the job set after the reload.
	Total int `json:"total"`
	Valid int `json:"valid"`

	// RunsCancelled counts the queued runs of removed jobs that were
	// ended, because a removed job can never execute them.
	RunsCancelled int `json:"runs_cancelled"`
}

// AppendLogRequest is the body of POST /v1/runs/{id}/logs.
type AppendLogRequest struct {
	Stream  string         `json:"stream"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// CaptureRequestsResponse is the body of GET /v1/runs/{id}/requests.
//
// The capture summary travels with the list because it is what tells a reader
// whether an empty list means "no requests" or "nothing was recorded".
type CaptureRequestsResponse struct {
	SchemaVersion int                          `json:"schema_version"`
	Capture       *inspection.RunCapture       `json:"capture"`
	Requests      []inspection.ExchangeSummary `json:"requests"`
}

// CaptureRequestResponse is the body of GET /v1/runs/{id}/requests/{request_id}.
type CaptureRequestResponse struct {
	SchemaVersion int                    `json:"schema_version"`
	Capture       *inspection.RunCapture `json:"capture"`
	Request       *inspection.Exchange   `json:"request"`
}

// TimelineResponse is the body of GET /v1/runs/{id}/timeline.
//
// It is the timeline page as read. The page always carries its context, its
// capture summary and its framing, so a caller that receives no events can still
// tell "this run recorded nothing" apart from "this run was never recorded", and
// can see how to ask for the rest. Events are metadata only -- no header, body or
// trigger payload travels here.
type TimelineResponse = timeline.Page

// ErrorResponse is the body of every error the API returns.
type ErrorResponse struct {
	// SchemaVersion versions the JSON shape itself; see compatibility.md.
	SchemaVersion int       `json:"schema_version"`
	Error         ErrorBody `json:"error"`
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
	// CodeOverloaded refuses an autonomous trigger because the job's own queue
	// depth bound is reached. It is distinct from CodeUnavailable: this is a
	// backpressure answer with a known relief (the queue drains), not a job that
	// has stopped accepting work.
	CodeOverloaded = "overloaded"
)

// ReleaseView is the result of activating a release.
//
// It names the job the release belongs to as well as the digest, because a
// pull-based deploy is told "run release abc123" and the caller that asked does
// not necessarily know which job that is -- the job is resolved from the release
// metadata, which is what makes the digest the identifier.
type ReleaseView struct {
	Job    string `json:"job"`
	Digest string `json:"digest"`
	// Digest_ is the digest recorded in the release metadata, echoed so a caller
	// can detect a metadata/digest disagreement rather than trusting the request.
	Digest_ string `json:"recorded_digest,omitempty"`
}
