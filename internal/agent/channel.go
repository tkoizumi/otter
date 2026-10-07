package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The tenant control channel.
//
// A pooled tenant's runtime is deliberately unreachable from Cloud, so the
// agent carries the channel over the SAME outbound connection and runtime
// identity as `desired`. Two things are separated on purpose, because a deploy
// and a log read must never contend for one lock:
//
//   - COMMANDS are durable and deduplicated. "Run job" carries a caller-derived
//     idempotency key; the runtime refuses a second run for the same key, so a
//     redelivered command is safe. An acknowledgement means the runtime ACCEPTED
//     the command, never that the job succeeded.
//   - READS are bounded and short-lived. They take no deployment lock, so
//     viewing a log cannot queue behind a maintenance window or a deploy.

// Command is one queued control action for this runtime.
type Command struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Job    string `json:"job,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	// IdempotencyKey is caller-derived and scoped to one job. It is sent to the
	// runtime, which is where the authoritative deduplication lives.
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Params         map[string]string `json:"params,omitempty"`
	DeadlineMS     int               `json:"deadline_ms,omitempty"`
}

// Read is one bounded read request for this runtime.
type Read struct {
	ID         string `json:"id"`
	Op         string `json:"op"`
	Job        string `json:"job,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
	DeadlineMS int    `json:"deadline_ms,omitempty"`
}

// ControlWork is what one poll of the control channel returns.
type ControlWork struct {
	Commands []Command `json:"commands"`
	Reads    []Read    `json:"reads"`
}

// Command outcomes. A closed set: Cloud decides what to do based on them.
const (
	CommandAccepted = "accepted"
	CommandRejected = "rejected"
	CommandFailed   = "failed"
)

// Read outcomes.
const (
	ReadOK    = "ok"
	ReadError = "error"
)

// CommandResult is the acknowledgement of one command.
type CommandResult struct {
	RuntimeID string `json:"runtime_id"`
	CommandID string `json:"id"`
	Status    string `json:"status"`
	RunID     string `json:"run_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ReadResult is the answer to one read. Body is the runtime's own bounded JSON.
type ReadResult struct {
	RuntimeID string          `json:"runtime_id"`
	ReadID    string          `json:"read_id"`
	Status    string          `json:"status"`
	Body      json.RawMessage `json:"body,omitempty"`
	Reason    string          `json:"reason,omitempty"`
}

// ControlChannel is the Cloud side of the tenant control channel.
type ControlChannel interface {
	// Control returns the work queued for this runtime. An empty answer is not an
	// error: it means there is nothing to do.
	Control(ctx context.Context) (*ControlWork, error)
	CommandResult(ctx context.Context, r CommandResult) error
	ReadResult(ctx context.Context, r ReadResult) error
}

// RuntimeCommands is the local runtime surface the agent is allowed to execute.
//
// It is deliberately narrow. The agent holds a credential scoped to this
// allowlist, and a command outside it is REJECTED rather than attempted, so a
// control plane cannot talk the agent into an operation its credential would
// refuse anyway.
type RuntimeCommands interface {
	SubmitRun(ctx context.Context, job, idempotencyKey string) (string, error)
	CancelRun(ctx context.Context, runID string) error
	PauseJob(ctx context.Context, job string) error
	ResumeJob(ctx context.Context, job string) error
	// Read performs one allowlisted read and returns the runtime's own bounded
	// JSON body, unmodified.
	Read(ctx context.Context, op, job, runID string, limit int, cursor string) (json.RawMessage, error)
}

// Control implements ControlChannel over the agent's outbound HTTP connection.
func (c *HTTPControlPlane) Control(ctx context.Context) (*ControlWork, error) {
	var out ControlWork
	err := c.post(ctx, "/agent/v1/control", map[string]string{"runtime_id": c.RuntimeID}, &out)
	if err == errNoDesiredWork {
		// 204 is "nothing queued", the same shape `desired` uses. It is not an
		// error and must not be retried as one.
		return &ControlWork{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CommandResult reports the outcome of one command.
func (c *HTTPControlPlane) CommandResult(ctx context.Context, r CommandResult) error {
	if r.RuntimeID == "" {
		r.RuntimeID = c.RuntimeID
	}
	return c.post(ctx, "/agent/v1/command-result", r, nil)
}

// ReadResult reports the answer to one read.
func (c *HTTPControlPlane) ReadResult(ctx context.Context, r ReadResult) error {
	if r.RuntimeID == "" {
		r.RuntimeID = c.RuntimeID
	}
	return c.post(ctx, "/agent/v1/read-result", r, nil)
}

// SubmitRun starts one run of a job, carrying the command's idempotency key so
// the runtime can refuse a duplicate. An empty key is sent as no key, which is
// the historical no-dedup behaviour and is only reachable for a command Cloud
// queued without one.
func (r *RuntimeHTTP) SubmitRun(ctx context.Context, job, idempotencyKey string) (string, error) {
	if strings.TrimSpace(job) == "" {
		return "", fmt.Errorf("agent: run command names no job")
	}
	var headers map[string]string
	if idempotencyKey != "" {
		headers = map[string]string{"Idempotency-Key": idempotencyKey}
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	// An empty body is valid and is recorded as the trigger body by the runtime.
	var body any
	if err := r.doHeaders(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(job)+"/runs", body, headers, &out); err != nil {
		return "", err
	}
	if out.RunID == "" {
		return "", fmt.Errorf("agent: runtime accepted a run but named no run id")
	}
	return out.RunID, nil
}

func (r *RuntimeHTTP) CancelRun(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("agent: cancel command names no run")
	}
	return r.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/cancel", nil, nil)
}

func (r *RuntimeHTTP) PauseJob(ctx context.Context, job string) error {
	if strings.TrimSpace(job) == "" {
		return fmt.Errorf("agent: pause command names no job")
	}
	return r.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(job)+"/pause", nil, nil)
}

func (r *RuntimeHTTP) ResumeJob(ctx context.Context, job string) error {
	if strings.TrimSpace(job) == "" {
		return fmt.Errorf("agent: resume command names no job")
	}
	return r.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(job)+"/resume", nil, nil)
}

// Read performs one allowlisted read. An op outside the list is refused here, so
// the allowlist is enforced on the agent as well as on Cloud.
func (r *RuntimeHTTP) Read(ctx context.Context, op, job, runID string, limit int, cursor string) (json.RawMessage, error) {
	path, err := readPath(op, job, runID, limit, cursor)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := r.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func readPath(op, job, runID string, limit int, cursor string) (string, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	qs := ""
	if len(query) > 0 {
		qs = "?" + query.Encode()
	}
	switch op {
	case "jobs.list":
		return "/v1/jobs" + qs, nil
	case "runs.list":
		if job != "" {
			query.Set("job_id", job)
			qs = "?" + query.Encode()
		}
		return "/v1/runs" + qs, nil
	case "run.get":
		if runID == "" {
			return "", fmt.Errorf("agent: %s needs a run id", op)
		}
		return "/v1/runs/" + url.PathEscape(runID), nil
	case "run.logs":
		if runID == "" {
			return "", fmt.Errorf("agent: %s needs a run id", op)
		}
		return "/v1/runs/" + url.PathEscape(runID) + "/logs" + qs, nil
	case "run.timeline":
		if runID == "" {
			return "", fmt.Errorf("agent: %s needs a run id", op)
		}
		return "/v1/runs/" + url.PathEscape(runID) + "/timeline", nil
	case "requests.list":
		if runID == "" {
			return "", fmt.Errorf("agent: %s needs a run id", op)
		}
		return "/v1/runs/" + url.PathEscape(runID) + "/requests" + qs, nil
	default:
		return "", fmt.Errorf("agent: read %q is not on the allowlist", op)
	}
}

// ControlLoop carries the tenant control channel.
//
// It is a SEPARATE loop from the apply loop on purpose. The apply loop can sit
// in a drain for minutes; if reads were executed inline there, a log view would
// wait behind a deploy. The two share the outbound connection and credential,
// not a lock.
type ControlLoop struct {
	Control   ControlChannel
	Runtime   RuntimeCommands
	RuntimeID string
	Logger    *slog.Logger
	// Interval between polls. Zero means the default.
	Interval time.Duration
}

func (l *ControlLoop) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

func (l *ControlLoop) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	// The control plane is expected to long-poll, so a short fixed interval is
	// only the fallback for a control plane that answers immediately.
	return 2 * time.Second
}

// Run polls until the context is cancelled. A control-plane error is logged and
// retried: a partition must not stop the runtime serving, and it must not stop
// the agent asking again.
func (l *ControlLoop) Run(ctx context.Context) error {
	for {
		if err := l.Once(ctx); err != nil {
			l.logger().Warn("agent_control_failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(l.interval()):
		}
	}
}

// Once performs one poll and executes whatever it returned. It is exported so
// `--once` and tests take the same path as the real loop.
func (l *ControlLoop) Once(ctx context.Context) error {
	if l.Control == nil || l.Runtime == nil {
		return fmt.Errorf("agent: control loop needs a control plane and a runtime")
	}
	work, err := l.Control.Control(ctx)
	if err != nil {
		return err
	}
	for _, cmd := range work.Commands {
		l.executeCommand(ctx, cmd)
	}
	for _, read := range work.Reads {
		l.executeRead(ctx, read)
	}
	return nil
}

// executeCommand runs one command and reports its acknowledgement. A failure to
// report is logged, not retried here: the command is durable on Cloud's side and
// a redelivery is the retry mechanism.
func (l *ControlLoop) executeCommand(ctx context.Context, cmd Command) {
	result := CommandResult{RuntimeID: l.RuntimeID, CommandID: cmd.ID}
	switch cmd.Action {
	case "run":
		runID, err := l.Runtime.SubmitRun(ctx, cmd.Job, cmd.IdempotencyKey)
		switch {
		case err != nil:
			result.Status = CommandFailed
			result.Reason = err.Error()
		default:
			result.Status = CommandAccepted
			result.RunID = runID
		}
	case "cancel":
		if err := l.Runtime.CancelRun(ctx, cmd.RunID); err != nil {
			result.Status = CommandFailed
			result.Reason = err.Error()
		} else {
			result.Status = CommandAccepted
		}
	case "pause":
		if err := l.Runtime.PauseJob(ctx, cmd.Job); err != nil {
			result.Status = CommandFailed
			result.Reason = err.Error()
		} else {
			result.Status = CommandAccepted
		}
	case "resume":
		if err := l.Runtime.ResumeJob(ctx, cmd.Job); err != nil {
			result.Status = CommandFailed
			result.Reason = err.Error()
		} else {
			result.Status = CommandAccepted
		}
	default:
		// Refused rather than guessed. An unknown action is a control-plane bug,
		// and reporting it is how the bug becomes visible.
		result.Status = CommandRejected
		result.Reason = fmt.Sprintf("unsupported action %q", cmd.Action)
	}
	if err := l.Control.CommandResult(ctx, result); err != nil {
		l.logger().Warn("agent_command_result_failed", "command_id", cmd.ID, "error", err.Error())
	}
}

// executeRead runs one bounded read and reports its body.
func (l *ControlLoop) executeRead(ctx context.Context, read Read) {
	result := ReadResult{RuntimeID: l.RuntimeID, ReadID: read.ID}
	body, err := l.Runtime.Read(ctx, read.Op, read.Job, read.RunID, read.Limit, read.Cursor)
	if err != nil {
		result.Status = ReadError
		result.Reason = err.Error()
	} else {
		result.Status = ReadOK
		result.Body = body
	}
	if err := l.Control.ReadResult(ctx, result); err != nil {
		l.logger().Warn("agent_read_result_failed", "read_id", read.ID, "error", err.Error())
	}
}
