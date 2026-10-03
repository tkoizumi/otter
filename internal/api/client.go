package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// DefaultBaseURL is where a locally running daemon listens.
const DefaultBaseURL = "http://127.0.0.1:7337"

// APIError describes a non-2xx API response.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s, HTTP %d)", e.Message, e.Code, e.StatusCode)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.StatusCode)
}

// IsNotFound reports whether the failure was a 404.
func (e *APIError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsUnauthorized reports whether the failure was an auth failure.
func (e *APIError) IsUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// Client is the CLI's HTTP client for the daemon.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a client. An empty baseURL falls back to the default.
func NewClient(baseURL, token string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// RunsQuery narrows a run listing.
type RunsQuery struct {
	JobID  string
	Status string
	Limit  int
	Offset int
}

// Health returns the daemon health summary.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var out HealthResponse
	if err := c.get(ctx, "/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Version returns the daemon's machine-readable contract document: the schema
// version, the runtime-contract version, the manifest schema, the embedded SDK
// version and the supported platforms.
func (c *Client) Version(ctx context.Context) (*VersionDocument, error) {
	var out VersionDocument
	if err := c.get(ctx, "/v1/version", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListJobs returns every discovered job.
func (c *Client) ListJobs(ctx context.Context) ([]JobView, error) {
	var out struct {
		Jobs []JobView `json:"jobs"`
	}
	if err := c.get(ctx, "/v1/jobs", nil, &out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// GetJob returns a single job.
func (c *Client) GetJob(ctx context.Context, id string) (*JobView, error) {
	var out JobView
	if err := c.get(ctx, "/v1/jobs/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveJob resolves a label, a path or an id to the job it
// names. It is the only reference resolution the CLI performs for daemon
// commands: the daemon owns the registry, so it owns the answer.
func (c *Client) ResolveJob(ctx context.Context, ref string) (*JobView, error) {
	values := url.Values{}
	values.Set("ref", ref)
	var out JobView
	if err := c.get(ctx, "/v1/jobs/resolve", values, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegisterJob registers a source path explicitly.
func (c *Client) RegisterJob(ctx context.Context, path string) (*JobView, error) {
	body, err := json.Marshal(RegisterRequest{Path: path})
	if err != nil {
		return nil, err
	}
	var out JobView
	if err := c.do(ctx, http.MethodPost, "/v1/jobs", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResetJob retires an identity and mints a fresh one at the same path.
func (c *Client) ResetJob(ctx context.Context, ref string) (*ResetView, error) {
	var out ResetView
	path := "/v1/jobs/" + url.PathEscape(ref) + "/reset"
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MoveJob preserves an identity across a daemon-performed rename.
func (c *Client) MoveJob(ctx context.Context, ref, destination string) (*JobView, error) {
	body, err := json.Marshal(MoveRequest{Destination: destination})
	if err != nil {
		return nil, err
	}
	var out JobView
	path := "/v1/jobs/" + url.PathEscape(ref) + "/move"
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PauseJob suspends a job's autonomous triggers: cron stops
// firing and the webhook refuses a trigger. Manual runs are unaffected.
func (c *Client) PauseJob(ctx context.Context, ref string) (*PauseView, error) {
	var out PauseView
	path := "/v1/jobs/" + url.PathEscape(ref) + "/pause"
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResumeJob re-arms the triggers a pause suspended.
func (c *Client) ResumeJob(ctx context.Context, ref string) (*PauseView, error) {
	var out PauseView
	path := "/v1/jobs/" + url.PathEscape(ref) + "/resume"
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetSchedule replaces a job's cadence. An empty cron clears the schedule,
// which means the job never fires on its own and a reload will not restore it.
func (c *Client) SetSchedule(ctx context.Context, ref, cron string) (*ScheduleView, error) {
	body, err := json.Marshal(ScheduleRequest{Cron: cron})
	if err != nil {
		return nil, err
	}
	var out ScheduleView
	path := "/v1/jobs/" + url.PathEscape(ref) + "/schedule"
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSchedules returns every schedule a job holds.
func (c *Client) ListSchedules(ctx context.Context, ref string) ([]ScheduleView, error) {
	var out ScheduleList
	path := "/v1/jobs/" + url.PathEscape(ref) + "/schedules"
	if err := c.get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Schedules, nil
}

// CreateSchedule adds an API-owned schedule to a job. An empty idempotencyKey
// sends no key, which a retry would duplicate; the CLI always supplies one.
func (c *Client) CreateSchedule(ctx context.Context, ref string, req ScheduleCreateRequest, idempotencyKey string) (*ScheduleView, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var headers map[string]string
	if idempotencyKey != "" {
		headers = map[string]string{"Idempotency-Key": idempotencyKey}
	}
	var out ScheduleView
	path := "/v1/jobs/" + url.PathEscape(ref) + "/schedules"
	if err := c.doWithHeaders(ctx, http.MethodPost, path, body, headers, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSchedule returns one schedule by id.
func (c *Client) GetSchedule(ctx context.Context, scheduleID string) (*ScheduleView, error) {
	var out ScheduleView
	path := "/v1/schedules/" + url.PathEscape(scheduleID)
	if err := c.get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSchedule applies a partial change to an API-owned schedule.
func (c *Client) UpdateSchedule(ctx context.Context, scheduleID string, req ScheduleUpdateRequest) (*ScheduleView, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out ScheduleView
	path := "/v1/schedules/" + url.PathEscape(scheduleID)
	if err := c.do(ctx, http.MethodPatch, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSchedule removes an API-owned schedule.
func (c *Client) DeleteSchedule(ctx context.Context, scheduleID string) error {
	path := "/v1/schedules/" + url.PathEscape(scheduleID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// SetSchedulePaused holds one schedule back or releases it.
func (c *Client) SetSchedulePaused(ctx context.Context, scheduleID string, paused bool) (*ScheduleView, error) {
	action := "resume"
	if paused {
		action = "pause"
	}
	var out ScheduleView
	path := "/v1/schedules/" + url.PathEscape(scheduleID) + "/" + action
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateAPIToken mints a named, scoped operator token. It is the only call
// that returns the token itself; the daemon stores only a hash of it.
func (c *Client) CreateAPIToken(ctx context.Context, name string, scope Scope) (*APITokenCreated, error) {
	body, err := json.Marshal(CreateAPITokenRequest{Name: name, Scope: scope})
	if err != nil {
		return nil, err
	}
	var out APITokenCreated
	if err := c.do(ctx, http.MethodPost, "/v1/tokens", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAPITokens lists the named tokens, revoked ones included.
func (c *Client) ListAPITokens(ctx context.Context) (*APITokenListResponse, error) {
	var out APITokenListResponse
	if err := c.get(ctx, "/v1/tokens", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeAPIToken withdraws a token. The next request that presents it is
// refused, with no restart.
func (c *Client) RevokeAPIToken(ctx context.Context, id string) (*RevokeAPITokenResponse, error) {
	var out RevokeAPITokenResponse
	path := "/v1/tokens/" + url.PathEscape(id)
	if err := c.do(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteJob purges an identity's durable artifacts.
func (c *Client) DeleteJob(ctx context.Context, ref string) (*DeletedView, error) {
	var out DeletedView
	path := "/v1/jobs/" + url.PathEscape(ref)
	if err := c.do(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reload asks the daemon to re-read its jobs directory. The daemon
// keeps running: this returns what changed, not a new process.
func (c *Client) Reload(ctx context.Context) (*ReloadResult, error) {
	var out ReloadResult
	if err := c.do(ctx, http.MethodPost, "/v1/reload", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SubmitRun queues a run with default submission options and returns its run id.
func (c *Client) SubmitRun(ctx context.Context, jobID string, body json.RawMessage) (string, error) {
	return c.SubmitRunWithOptions(ctx, jobID, body, "")
}

// SubmitRunWithOptions queues a run and returns its run id.
//
// capture is the HTTP capture policy for the run and travels as a query option
// rather than in the body: the body is the trigger JSON, recorded and handed to
// job code verbatim. An empty value means the daemon's default.
func (c *Client) SubmitRunWithOptions(ctx context.Context, jobID string, body json.RawMessage, capture string) (string, error) {
	path := "/v1/jobs/" + url.PathEscape(jobID) + "/runs"
	if capture != "" {
		path += "?capture=" + url.QueryEscape(capture)
	}

	var payload []byte
	if len(bytes.TrimSpace(body)) > 0 {
		payload = body
	}

	var out SubmitRunResponse
	if err := c.do(ctx, http.MethodPost, path, payload, &out); err != nil {
		return "", err
	}
	return out.RunID, nil
}

// ListCaptureRequests returns a run's captured HTTP exchanges.
//
// The list carries the capture summary and no payloads, so it is safe to render
// without loading bodies, and an empty list can be told apart from a recording
// that was never made.
func (c *Client) ListCaptureRequests(ctx context.Context, runID string, afterID int64, limit int) (*CaptureRequestsResponse, error) {
	values := url.Values{}
	if afterID > 0 {
		values.Set("after_id", strconv.FormatInt(afterID, 10))
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/runs/" + url.PathEscape(runID) + "/requests"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out CaptureRequestsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCaptureRequest returns one captured exchange with its sanitized payloads.
func (c *Client) GetCaptureRequest(ctx context.Context, runID, requestID string) (*CaptureRequestResponse, error) {
	path := "/v1/runs/" + url.PathEscape(runID) + "/requests/" + url.PathEscape(requestID)

	var out CaptureRequestResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCaptureRequestByID returns one captured exchange addressed by request id
// alone. The daemon resolves the owning run from storage, so an id that more
// than one run recorded is reported as a conflict rather than guessed.
func (c *Client) GetCaptureRequestByID(ctx context.Context, requestID string) (*CaptureRequestResponse, error) {
	path := "/v1/requests/" + url.PathEscape(requestID)

	var out CaptureRequestResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Timeline fetches one page of a finished run's merged timeline.
//
// The page carries its own context, capture summary and continuation cursor, so
// a caller that receives no events still learns whether anything was recorded
// and how to ask for the next page.
func (c *Client) Timeline(ctx context.Context, req timeline.Request) (*timeline.Page, error) {
	values := url.Values{}
	if req.After != "" {
		values.Set("after", req.After)
	}
	if req.Limit > 0 {
		values.Set("limit", strconv.Itoa(req.Limit))
	}
	values.Set("include_http", strconv.FormatBool(req.IncludeHTTP))

	path := "/v1/runs/" + url.PathEscape(req.RunID) + "/timeline?" + values.Encode()

	var out timeline.Page
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CancelRun(ctx context.Context, runID string) error {
	return c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/cancel", nil, nil)
}

// ListRuns lists runs.
func (c *Client) ListRuns(ctx context.Context, q RunsQuery) ([]*runs.Run, error) {
	values := url.Values{}
	if q.JobID != "" {
		values.Set("job_id", q.JobID)
	}
	if q.Status != "" {
		values.Set("status", q.Status)
	}
	if q.Limit > 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		values.Set("offset", strconv.Itoa(q.Offset))
	}

	var out struct {
		Runs []*runs.Run `json:"runs"`
	}
	if err := c.get(ctx, "/v1/runs", values, &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}

// GetRun returns a run together with its retry chain.
func (c *Client) GetRun(ctx context.Context, runID string) (*RunView, error) {
	var out RunView
	if err := c.get(ctx, "/v1/runs/"+url.PathEscape(runID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetLogs returns captured output for a run after the given log id.
func (c *Client) GetLogs(ctx context.Context, runID string, afterID int64, limit int) ([]runs.LogEntry, error) {
	values := url.Values{}
	if afterID > 0 {
		values.Set("after_id", strconv.FormatInt(afterID, 10))
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}

	var out struct {
		Logs []runs.LogEntry `json:"logs"`
	}
	if err := c.get(ctx, "/v1/runs/"+url.PathEscape(runID)+"/logs", values, &out); err != nil {
		return nil, err
	}
	return out.Logs, nil
}

// AllState returns every state key for a job.
func (c *Client) AllState(ctx context.Context, jobID string) (map[string]json.RawMessage, error) {
	var out StateResponse
	if err := c.get(ctx, "/v1/jobs/"+url.PathEscape(jobID)+"/state", nil, &out); err != nil {
		return nil, err
	}
	return out.State, nil
}

// GetState returns the raw JSON value of one state key. It returns
// ErrStateNotFound when the key is unset.
func (c *Client) GetState(ctx context.Context, jobID, key string) (json.RawMessage, error) {
	raw, err := c.raw(ctx, http.MethodGet, statePath(jobID, key), nil)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// SetState writes one state key.
func (c *Client) SetState(ctx context.Context, jobID, key string, value json.RawMessage) (*SetStateResponse, error) {
	if !json.Valid(value) {
		return nil, fmt.Errorf("state value must be valid JSON")
	}
	var out SetStateResponse
	if err := c.do(ctx, http.MethodPut, statePath(jobID, key), value, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteState removes one state key.
func (c *Client) DeleteState(ctx context.Context, jobID, key string) error {
	return c.do(ctx, http.MethodDelete, statePath(jobID, key), nil, nil)
}

func statePath(jobID, key string) string {
	return "/v1/jobs/" + url.PathEscape(jobID) + "/state/" + url.PathEscape(key)
}

// ------------------------------------------------------------- transport

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	return c.doWithHeaders(ctx, method, path, body, nil, out)
}

// doWithHeaders is do with extra request headers, which the schedule create
// endpoint uses for its idempotency key.
func (c *Client) doWithHeaders(ctx context.Context, method, path string, body []byte, headers map[string]string, out any) error {
	raw, err := c.rawWithHeaders(ctx, method, path, body, headers)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("api: decode response from %s: %w", path, err)
	}
	return nil
}

func (c *Client) raw(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return c.rawWithHeaders(ctx, method, path, body, nil)
}

func (c *Client) rawWithHeaders(ctx context.Context, method, path string, body []byte, headers map[string]string) ([]byte, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("api: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("cannot reach otterd at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("api: read response from %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeAPIError(resp.StatusCode, payload)
	}
	return payload, nil
}

func decodeAPIError(status int, payload []byte) error {
	var envelope ErrorResponse
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Error.Message != "" {
		return &APIError{StatusCode: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	message := strings.TrimSpace(string(payload))
	if message == "" {
		message = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Message: message}
}
