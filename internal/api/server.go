// Package api implements Otter's local HTTP API and the CLI's HTTP client.
//
// The API is the only interface between the daemon, the CLI and running
// job processes. It binds to loopback by default; binding elsewhere
// requires a bearer token, which the daemon enforces at startup.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/state"
	"github.com/tkoizumi/otter/internal/timeline"
)

const (
	maxBodyBytes  = 1 << 20 // 1 MiB
	maxLogMessage = 64 * 1024
)

// ServerConfig configures the HTTP API.
type ServerConfig struct {
	Listen   string
	APIToken string

	// CaptureLimits bounds HTTP capture ingestion. The zero value means
	// inspection.DefaultLimits.
	CaptureLimits inspection.Limits

	// OnReady is called once the listener is bound, with the address the
	// kernel actually chose -- which is not necessarily the configured one
	// when it names port 0. A daemon that will be addressed by other
	// processes needs the resolved value, not the request.
	OnReady func(addr string)
}

// Server serves the Otter HTTP API.
type Server struct {
	cfg           ServerConfig
	backend       Backend
	logger        *logging.Logger
	http          *http.Server
	captureLimits inspection.Limits
}

// NewServer wires the API routes.
func NewServer(cfg ServerConfig, backend Backend, logger *logging.Logger) *Server {
	limits := cfg.CaptureLimits
	if limits.MaxBodyBytes == 0 {
		limits = inspection.DefaultLimits()
	}
	s := &Server{cfg: cfg, backend: backend, logger: logger, captureLimits: limits}
	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return s
}

// Handler returns the fully wrapped HTTP handler. It is exported so tests can
// exercise the API with httptest.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	// Version metadata is public for the same reason /health is: a client must
	// be able to learn the shape before it authenticates to it.
	mux.HandleFunc("GET /v1/version", s.handleVersion)

	// Admin-only: identity changes, daemon configuration and the tokens
	// themselves. None of these is reachable with a scoped token. Capture
	// payloads are not here: a capture-scoped credential may read them (see
	// below), and nothing else on this block.
	mux.Handle("GET /v1/jobs/resolve", s.admin(s.handleResolveJob))
	mux.Handle("POST /v1/jobs", s.admin(s.handleRegisterJob))
	mux.Handle("POST /v1/jobs/{id}/reset", s.admin(s.handleResetJob))
	mux.Handle("POST /v1/jobs/{id}/move", s.admin(s.handleMoveJob))
	mux.Handle("DELETE /v1/jobs/{id}", s.admin(s.handleDeleteJob))
	mux.Handle("POST /v1/reload", s.admin(s.handleReload))

	// Maintenance is admin-only in both directions. A control credential can
	// pause a job; only an admin credential can hold the whole runtime back or
	// activate it, because that decides whether customer work runs at all.
	mux.Handle("GET /v1/runtime/maintenance", s.admin(s.handleGetMaintenance))
	mux.Handle("POST /v1/runtime/maintenance", s.admin(s.handleEnterMaintenance))
	mux.Handle("DELETE /v1/runtime/maintenance", s.admin(s.handleExitMaintenance))
	// Release activation, so a runtime agent promotes through the same surface an
	// operator uses instead of reaching past the API into the data directory.
	mux.Handle("POST /v1/runtime/releases/activate", s.admin(s.handleActivateRelease))
	mux.Handle("POST /v1/tokens", s.admin(s.handleCreateToken))
	mux.Handle("GET /v1/tokens", s.admin(s.handleListTokens))
	mux.Handle("DELETE /v1/tokens/{id}", s.admin(s.handleRevokeToken))

	// Capture payloads: the two routes that return sanitized bodies. They are
	// the one surface a control-scoped credential does not reach, and the
	// reason a `capture` scope exists: an operator who wants a control plane
	// to explain an exchange mints one explicitly, rather than widening every
	// control credential already issued (decisions.md, 2026-10-03).
	mux.Handle("GET /v1/runs/{id}/requests/{request_id}", s.capture(s.handleGetCaptureRequest))
	mux.Handle("GET /v1/requests/{request_id}", s.capture(s.handleGetCaptureRequestByID))

	// The operator read surface: metadata, run output, the merged timeline and
	// capture summaries, reachable with a read-, control- or capture-scoped
	// token or the admin token. Capture *payloads* are not here; they need the
	// capture scope above.
	mux.Handle("GET /v1/jobs", s.scoped(s.handleListJobs))
	mux.Handle("GET /v1/runs", s.scoped(s.handleListRuns))
	mux.Handle("GET /v1/runs/{id}/timeline", s.scoped(s.handleTimeline))
	mux.Handle("GET /v1/runs/{id}/requests", s.scoped(s.handleListCaptureRequests))
	mux.Handle("GET /v1/jobs/{id}/schedules", s.scoped(s.handleListSchedules))
	mux.Handle("GET /v1/schedules/{schedule_id}", s.scoped(s.handleGetSchedule))
	// Configuration is job metadata and is not secret, so reading it needs only
	// a scoped token. Writing it is admin-only for v0.4.0: the control scope's
	// published surface (CL-21) does not include configuration, and widening it
	// silently would break that credential's contract.
	mux.Handle("GET /v1/jobs/{id}/config", s.scoped(s.handleGetJobConfig))

	// Reads a per-run token may also make. The handler narrows it to its own
	// job or run, which is why these admit it and the four above do not.
	mux.Handle("GET /v1/jobs/{id}", s.reader(s.handleGetJob))
	mux.Handle("GET /v1/runs/{id}", s.reader(s.handleGetRun))
	mux.Handle("GET /v1/runs/{id}/logs", s.reader(s.handleGetLogs))

	// Control: the command surface a gateway holds. Read is not enough, and a
	// per-run token is not accepted here.
	mux.Handle("POST /v1/jobs/{id}/pause", s.control(s.handlePauseJob))
	mux.Handle("POST /v1/jobs/{id}/resume", s.control(s.handleResumeJob))
	mux.Handle("PUT /v1/jobs/{id}/schedule", s.control(s.handleSetSchedule))
	mux.Handle("DELETE /v1/jobs/{id}/schedule", s.control(s.handleClearSchedule))
	mux.Handle("POST /v1/jobs/{id}/schedules", s.control(s.handleCreateSchedule))
	mux.Handle("PATCH /v1/schedules/{schedule_id}", s.control(s.handleUpdateSchedule))
	mux.Handle("DELETE /v1/schedules/{schedule_id}", s.control(s.handleDeleteSchedule))
	mux.Handle("POST /v1/schedules/{schedule_id}/pause", s.control(s.handlePauseSchedule))
	mux.Handle("POST /v1/schedules/{schedule_id}/resume", s.control(s.handleResumeSchedule))
	mux.Handle("PUT /v1/jobs/{id}/config", s.admin(s.handleSetJobConfig))
	mux.Handle("POST /v1/jobs/{id}/runs", s.control(s.handleSubmitRun))
	mux.Handle("POST /v1/runs/{id}/cancel", s.control(s.handleCancelRun))

	// Child-facing: the admin token or the run's own per-run token. A named API
	// token is never accepted here, because these read and write the job's own
	// business state.
	mux.Handle("POST /v1/runs/{id}/logs", s.principal(s.handleAppendLog))
	mux.Handle("POST /v1/runs/{id}/requests/events", s.principal(s.handleIngestCapture))
	mux.Handle("GET /v1/jobs/{id}/state", s.principal(s.handleGetAllState))
	mux.Handle("GET /v1/jobs/{id}/state/{key}", s.principal(s.handleGetState))
	mux.Handle("PUT /v1/jobs/{id}/state/{key}", s.principal(s.handleSetState))
	mux.Handle("DELETE /v1/jobs/{id}/state/{key}", s.principal(s.handleDeleteState))

	// Webhooks authenticate with their own per-job token.
	mux.Handle("POST /v1/hooks/{job}", s.webhook(s.handleWebhook))

	mux.HandleFunc("/", s.handleNotFound)

	return s.recoverPanic(s.logRequests(mux))
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("api: listen on %s: %w", s.cfg.Listen, err)
	}

	s.logger.Info("api_listening",
		"listen", listener.Addr().String(),
		"auth_required", s.cfg.APIToken != "")

	if s.cfg.OnReady != nil {
		s.cfg.OnReady(listener.Addr().String())
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(listener) }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api: serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- middleware

type principal struct {
	Admin bool

	// Scope and TokenName are set when a named API token authenticated. Scope
	// is empty for the admin token and for a per-run token, which is how the
	// gates below tell the three credential classes apart.
	Scope     Scope
	TokenName string

	Token RunToken
}

// isOperator reports whether the caller holds authority over the whole runtime
// rather than over a single job or run: the admin token, or a named API token.
// What the caller may then do is decided by its scope at the route's gate, not
// here.
func (p principal) isOperator() bool {
	return p.Admin || p.Scope != ""
}

func (p principal) allowsJob(id string) bool {
	return p.isOperator() || (p.Token.JobID != "" && p.Token.JobID == id)
}

func (p principal) allowsRun(id string) bool {
	return p.isOperator() || (p.Token.RunID != "" && p.Token.RunID == id)
}

// canRead reports whether the caller may read runtime metadata and run output:
// the job and run listings, run detail, logs, the timeline and capture
// summaries. It is false for a per-run token, which the reader gate admits
// separately because the handler narrows it to its own run. A capture-scoped
// token reads everything a control token does, and payloads besides.
func (p principal) canRead() bool {
	return p.Admin || p.Scope == ScopeRead || p.Scope == ScopeControl || p.Scope == ScopeCapture
}

// canControl reports whether the caller may command the runtime: submit, cancel,
// pause, resume and schedule. `capture` is a superset of `control`, so it may.
func (p principal) canControl() bool {
	return p.Admin || p.Scope == ScopeControl || p.Scope == ScopeCapture
}

// canReadCapture reports whether the caller may read captured request and
// response bodies. Only the admin token and an explicitly minted `capture`
// credential may: `read` and `control` are refused, which is the boundary the
// scope exists to draw.
func (p principal) canReadCapture() bool {
	return p.Admin || p.Scope == ScopeCapture
}

type principalCtxKey struct{}

func principalFrom(ctx context.Context) principal {
	if p, ok := ctx.Value(principalCtxKey{}).(principal); ok {
		return p
	}
	return principal{}
}

// gate authenticates the caller and applies exactly one authorization rule.
// Every route names its gate where it is registered, so the authority a
// credential needs is visible in the route table rather than inferred from the
// handler body.
func (s *Server) gate(h http.HandlerFunc, allow func(principal) bool, refusal string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if !allow(p) {
			s.writeError(w, http.StatusForbidden, CodeForbidden, refusal)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalCtxKey{}, p)))
	})
}

// admin admits only the static admin token.
func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return s.gate(h, func(p principal) bool { return p.Admin },
		"this endpoint requires the admin API token")
}

// control admits the admin token or a control-scoped API token. A read-scoped
// token and a per-run token are both refused.
func (s *Server) control(h http.HandlerFunc) http.Handler {
	return s.gate(h, principal.canControl,
		"this endpoint requires the admin API token or a control-scoped token")
}

// scoped is the operator read surface: the admin token, or a read-, control- or
// capture-scoped API token. A per-run token is refused, because these endpoints
// read across jobs and runs rather than narrowing to one.
func (s *Server) scoped(h http.HandlerFunc) http.Handler {
	return s.gate(h, principal.canRead,
		"this endpoint requires a read, control or capture API token")
}

// capture admits the admin token or a capture-scoped API token. A `read` or
// `control` credential is refused: reading the client's traffic is a separate,
// explicitly named authority, not a side effect of commanding the runtime.
func (s *Server) capture(h http.HandlerFunc) http.Handler {
	return s.gate(h, principal.canReadCapture,
		"this endpoint requires the admin API token or a capture-scoped token")
}

// reader admits everything scoped does, plus a per-run token, which the handler
// narrows to its own job or run.
func (s *Server) reader(h http.HandlerFunc) http.Handler {
	return s.gate(h, func(p principal) bool { return p.canRead() || p.Token.RunID != "" },
		"this endpoint requires a read, control or capture API token")
}

// principal is the child-facing gate: the admin token or the run's own token. A
// named API token is deliberately excluded, because these endpoints read and
// write the job's own business state.
func (s *Server) principal(h http.HandlerFunc) http.Handler {
	return s.gate(h, func(p principal) bool { return p.Admin || p.Token.RunID != "" },
		"this endpoint requires the admin API token or a per-run token")
}

// resolvePrincipal identifies the caller without writing a response, so that
// endpoints such as /health can offer a richer answer to an authenticated
// caller and a minimal one to everyone else.
//
// Three credential classes exist. The static admin token has full authority. A
// per-run token is scoped to one run and one job, and is the only class a child
// process ever holds. A named API token carries a Scope and is how a gateway
// commands a runtime without holding authority over the tenant (CL-21).
func (s *Server) resolvePrincipal(r *http.Request) (principal, bool) {
	if token := bearerToken(r); token != "" {
		if s.cfg.APIToken != "" && constantTimeEqual(token, s.cfg.APIToken) {
			return principal{Admin: true}, true
		}
		if scope, ok := s.backend.ResolveRunToken(token); ok {
			return principal{Token: scope}, true
		}
		if named, ok := s.backend.ResolveAPIToken(token); ok {
			return principal{Scope: named.Scope, TokenName: named.Name}, true
		}
		return principal{}, false
	}

	if s.cfg.APIToken != "" {
		return principal{}, false
	}

	// No token configured: a loopback-only deployment trusts local callers.
	// Startup validation guarantees the listener is not reachable remotely.
	if !isLoopbackRemote(r.RemoteAddr) {
		return principal{}, false
	}
	return principal{Admin: true}, true
}

// authenticate resolves the caller and reports the reason on failure. When no
// API token is configured the daemon is loopback-only, so local callers are
// trusted; a non-loopback peer is still rejected defensively.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (principal, bool) {
	if p, ok := s.resolvePrincipal(r); ok {
		return p, true
	}

	if bearerToken(r) != "" {
		s.writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid API token")
		return principal{}, false
	}

	if s.cfg.APIToken == "" {
		s.writeError(w, http.StatusUnauthorized, CodeUnauthorized,
			"requests from non-loopback addresses require an API token")
		return principal{}, false
	}

	s.writeError(w, http.StatusUnauthorized, CodeUnauthorized,
		"missing bearer token: send Authorization: Bearer <OTTER_API_TOKEN>")
	return principal{}, false
}

// webhook authenticates a webhook call against the job's own token.
func (s *Server) webhook(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		job := r.PathValue("job")
		if job == "" {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "job is required")
			return
		}

		expected, ok := s.backend.WebhookTokenFor(job)
		if !ok {
			// Deliberately identical for "unknown job" and "webhook
			// disabled" so the endpoint does not enumerate jobs.
			s.writeError(w, http.StatusNotFound, CodeNotFound,
				fmt.Sprintf("job %q does not accept webhook triggers", job))
			return
		}

		presented := r.Header.Get("X-Otter-Token")
		if presented == "" {
			presented = r.URL.Query().Get("token")
		}
		if presented == "" || !constantTimeEqual(presented, expected) {
			s.writeError(w, http.StatusUnauthorized, CodeUnauthorized,
				"invalid or missing webhook token")
			return
		}

		h(w, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("api_panic", fmt.Errorf("panic: %v", rec),
					"method", r.Method, "path", r.URL.Path)
				s.writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.logger.Enabled(logging.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.logger.Debug("api_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// ------------------------------------------------------------------ handlers

// handleHealth always answers 200 so that liveness probes (Docker, systemd)
// work without credentials, but only an authenticated caller sees the
// operational counters. That keeps a token-protected deployment from
// disclosing job and run counts to the network.
//
// The richer signals -- queue age, per-job depth, retry activity, per-job
// freshness and storage pressure -- are guarded by the same check and each
// degrades on its own: a read that fails drops its block and is logged, rather
// than turning a liveness check into an error.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		SchemaVersion: SchemaVersion,
		Status:        "ok",
		Version:       s.backend.Version(),
		UptimeSeconds: time.Since(s.backend.StartedAt()).Seconds(),
	}

	if _, authenticated := s.resolvePrincipal(r); !authenticated {
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	views := s.backend.ListJobs(r.Context())
	counts := &HealthCounts{Total: len(views)}
	for _, v := range views {
		if v.Valid {
			counts.Valid++
		} else {
			counts.Invalid++
		}
	}

	depth, err := s.backend.QueueDepth(r.Context())
	if err != nil {
		s.logger.Warn("health_queue_depth", "error", err.Error())
		depth = 0
	}
	runCounts, err := s.backend.RunCounts(r.Context())
	if err != nil {
		s.logger.Warn("health_run_counts", "error", err.Error())
		runCounts = map[string]int{}
	}

	resp.Jobs = counts
	resp.QueueDepth = &depth
	resp.Runs = runCounts

	now := time.Now().UTC()

	refusals, err := s.backend.AdmissionRefusals(r.Context())
	if err != nil {
		s.logger.Warn("health_admission_refusals", "error", err.Error())
		refusals = nil
	}

	if stats, err := s.backend.QueueStats(r.Context()); err != nil {
		s.logger.Warn("health_queue_stats", "error", err.Error())
	} else {
		queue := &HealthQueue{ByJob: stats.ByJob, Retrying: stats.Retrying, NextRetryAt: stats.NextRetryAt}
		if stats.OldestWaitingAt != nil {
			age := now.Sub(*stats.OldestWaitingAt).Seconds()
			queue.OldestWaitingAt = stats.OldestWaitingAt
			queue.OldestWaitingSeconds = &age
		}
		// The refusal picture belongs beside the depth it was refused at: a
		// depth with no bound reports nothing, and a bound with no refusals
		// reports zeros rather than an absent block, which is itself the
		// statement "this job is not being held back".
		if len(refusals) > 0 {
			queue.ByJobRefused = refusals
			for _, rec := range refusals {
				queue.RefusedTotal += rec.Total
				if rec.LastAt != nil && (queue.LastRefusedAt == nil || rec.LastAt.After(*queue.LastRefusedAt)) {
					at := *rec.LastAt
					queue.LastRefusedAt = &at
				}
			}
		}
		resp.Queue = queue
	}

	if maint, err := s.backend.Maintenance(r.Context()); err != nil {
		s.logger.Warn("health_maintenance", "error", err.Error())
	} else {
		resp.Maintenance = &maint
	}

	if counters, err := s.backend.ScheduleCounters(r.Context()); err != nil {
		s.logger.Warn("health_schedule_counters", "error", err.Error())
	} else if len(counters) > 0 {
		resp.Schedules = counters
	}

	if fresh, err := s.backend.LastSuccessByJob(r.Context()); err != nil {
		s.logger.Warn("health_freshness", "error", err.Error())
	} else {
		resp.Freshness = make([]HealthFreshness, 0, len(views))
		for _, v := range views {
			entry := HealthFreshness{JobID: v.ID, Name: v.Name}
			if at, ok := fresh[v.ID]; ok {
				instant := at
				age := now.Sub(at).Seconds()
				entry.LastSuccessAt = &instant
				entry.AgeSeconds = &age
			}
			resp.Freshness = append(resp.Freshness, entry)
		}
		// A stable order makes the response diffable and the documented
		// example reproducible.
		sort.Slice(resp.Freshness, func(i, j int) bool { return resp.Freshness[i].JobID < resp.Freshness[j].JobID })
	}

	if storage, err := s.backend.StorageStats(r.Context()); err != nil {
		s.logger.Warn("health_storage", "error", err.Error())
	} else {
		resp.Storage = &HealthStorage{
			DBBytes:        storage.DBBytes,
			DiskFreeBytes:  storage.DiskFreeBytes,
			DiskTotalBytes: storage.DiskTotalBytes,
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.backend.ListJobs(r.Context())
	if jobs == nil {
		jobs = []JobView{}
	}
	s.writeJSON(w, http.StatusOK, JobList{SchemaVersion: SchemaVersion, Jobs: jobs})
}

// handleResolveJob resolves a label, path or id reference. It is the
// one place reference resolution lives, so the CLI never has to guess and
// never has to read the registry database itself.
//
// A reference that is ambiguous is a 409 naming every candidate; a reference
// that matches nothing is a 404.
func (s *Server) handleResolveJob(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if strings.TrimSpace(ref) == "" {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "ref is required")
		return
	}
	view, err := s.backend.ResolveJob(ref)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleRegisterJob registers a source directory explicitly.
func (s *Server) handleRegisterJob(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req RegisterRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a JSON object with a path")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "path is required")
		return
	}
	view, err := s.backend.RegisterJob(r.Context(), req.Path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleResetJob retires an identity and mints a fresh one.
func (s *Server) handleResetJob(w http.ResponseWriter, r *http.Request) {
	result, err := s.backend.ResetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// handleMoveJob preserves an identity across a directory rename.
func (s *Server) handleMoveJob(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req MoveRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a JSON object with a destination")
		return
	}
	if strings.TrimSpace(req.Destination) == "" {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "destination is required")
		return
	}
	view, err := s.backend.MoveJob(r.Context(), r.PathValue("id"), req.Destination)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handlePauseJob suspends a job's autonomous triggers: cron
// stops firing and the webhook refuses a trigger. Nothing is retired, so
// `otter run` still runs it on demand.
func (s *Server) handlePauseJob(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, true)
}

// handleResumeJob re-arms the triggers a pause suspended.
func (s *Server) handleResumeJob(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, false)
}

// setPaused is the shared body of pause and resume. The only difference is the
// direction, so the reference resolution and the response shape cannot drift
// apart between them. Neither takes a request body.
func (s *Server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	view, err := s.backend.SetPaused(r.Context(), r.PathValue("id"), paused)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleSetSchedule replaces a job's cadence. The body is {"cron": "..."}, and
// an empty cron clears the schedule.
//
// The schedule is runtime state rather than manifest state: a cadence changes
// far more often than a job's code, and a value that a file and an API can both
// write is a value that eventually disagrees with itself.
func (s *Server) handleSetSchedule(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req ScheduleRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a JSON object with a cron field")
		return
	}
	view, err := s.backend.SetSchedule(r.Context(), r.PathValue("id"), strings.TrimSpace(req.Cron))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleClearSchedule removes a job's cadence.
//
// Clearing is a first-class request rather than "put the manifest's value
// back": it means the job should not fire on its own at all, and a later reload
// must not undo it.
func (s *Server) handleClearSchedule(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.SetSchedule(r.Context(), r.PathValue("id"), "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// idempotencyKey reads the caller's key for a mutating control command. The
// header is optional; without it a retried command creates a second schedule,
// which is why the CLI always sends one.
func idempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

// handleListSchedules returns every schedule a job holds, whatever owns it.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	views, err := s.backend.ListSchedules(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if views == nil {
		views = []ScheduleView{}
	}
	s.writeJSON(w, http.StatusOK, ScheduleList{SchemaVersion: SchemaVersion, Schedules: views})
}

// handleCreateSchedule adds a schedule to a job. A caller-supplied
// Idempotency-Key makes a retry return the existing schedule with changed
// false, so a duplicated "schedule this every 15 minutes" does not produce two.
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req ScheduleCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a JSON object with a cron field")
		return
	}
	req.Cron = strings.TrimSpace(req.Cron)
	if req.Cron == "" {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "cron is required")
		return
	}
	view, err := s.backend.CreateSchedule(r.Context(), r.PathValue("id"), req, idempotencyKey(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if !view.Changed {
		// An idempotent replay returns the existing resource rather than
		// claiming a creation that did not happen.
		status = http.StatusOK
	}
	s.writeJSON(w, status, view)
}

// handleGetSchedule returns one schedule by id.
func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.GetSchedule(r.Context(), r.PathValue("schedule_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleUpdateSchedule applies a partial change to an API-owned schedule. A
// manifest-owned row is refused with 409.
func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req ScheduleUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a JSON object")
		return
	}
	if req.Cron != nil && strings.TrimSpace(*req.Cron) == "" {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "cron may not be empty; delete the schedule instead")
		return
	}
	view, err := s.backend.UpdateSchedule(r.Context(), r.PathValue("schedule_id"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleDeleteSchedule removes an API-owned schedule.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.DeleteSchedule(r.Context(), r.PathValue("schedule_id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePauseSchedule holds one schedule back without touching the job.
func (s *Server) handlePauseSchedule(w http.ResponseWriter, r *http.Request) {
	s.setSchedulePaused(w, r, true)
}

// handleResumeSchedule releases a held-back schedule.
func (s *Server) handleResumeSchedule(w http.ResponseWriter, r *http.Request) {
	s.setSchedulePaused(w, r, false)
}

func (s *Server) setSchedulePaused(w http.ResponseWriter, r *http.Request, paused bool) {
	view, err := s.backend.SetSchedulePaused(r.Context(), r.PathValue("schedule_id"), paused)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleGetJobConfig returns a job's current configuration. Values is always an
// object, `{}` when the job has none.
func (s *Server) handleGetJobConfig(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.GetJobConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view.SchemaVersion = SchemaVersion
	s.writeJSON(w, http.StatusOK, view)
}

// handleSetJobConfig writes a new immutable configuration version. Already
// accepted runs keep the version they pinned, so this changes future work only.
func (s *Server) handleSetJobConfig(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req JobConfigRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a JSON object with a values field")
		return
	}
	if len(req.Values) == 0 {
		// An absent values key is an empty configuration, not a malformed one:
		// it is what "clear the configuration" means.
		req.Values = json.RawMessage("{}")
	}
	view, err := s.backend.SetJobConfig(r.Context(), r.PathValue("id"), req.Values, "admin")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view.SchemaVersion = SchemaVersion
	s.writeJSON(w, http.StatusOK, view)
}

// handleDeleteJob purges an identity's durable artifacts. It is a
// distinct operation from removing the directory: the source files are left
// alone and the path is suppressed so a scan cannot silently re-register it.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	result, err := s.backend.DeleteJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// handleReload re-reads the jobs directory against the running daemon.
// It is admin-only because it changes what the whole runtime knows about, and
// therefore what every other caller can address.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	result, err := s.backend.Reload(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result.SchemaVersion = SchemaVersion
	s.writeJSON(w, http.StatusOK, result)
}

// handleVersion returns the machine-readable contract document: the schema
// version, the runtime-contract version, the manifest schema, the embedded SDK
// version and the supported platforms. It is public, like /health, because a
// client needs to know what shape it is talking to before it holds a token.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, VersionDocumentFor(s.backend.Version()))
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principalFrom(r.Context())
	if !p.allowsJob(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this job")
		return
	}

	view, ok := s.backend.GetJob(id)
	if !ok {
		s.writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("job %q not found", id))
		return
	}
	// The single-job view carries the job's webhook token, which is itself a
	// credential: it triggers runs. Only the admin token may see it, so a
	// read- or control-scoped caller gets the job without it (CL-21).
	if !p.Admin {
		view.Triggers.WebhookToken = ""
	}
	s.writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleSubmitRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}

	payload := TriggerPayload{Type: TriggerManual}
	if len(bytes.TrimSpace(body)) > 0 {
		if !json.Valid(body) {
			s.writeError(w, http.StatusBadRequest, CodeInvalid,
				"request body must be valid JSON (or empty) and is recorded as the trigger body")
			return
		}
		payload.Body = json.RawMessage(body)
	}

	// An absent ?capture= is not "the default": it leaves the choice to the
	// job's manifest and then the deployment default, which is resolved
	// when the run is admitted. Only an explicit value is validated here.
	capture, err := inspection.ParsePolicyOverride(r.URL.Query().Get("capture"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}

	runID, err := s.backend.SubmitRunWithOptions(r.Context(), id, payload, SubmitRunOptions{Capture: capture})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusAccepted, SubmitRunResponse{RunID: runID, Status: string(runs.StatusQueued)})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := runs.Filter{
		JobID:       q.Get("job_id"),
		ParentRunID: q.Get("parent_run_id"),
	}
	if raw := q.Get("status"); raw != "" {
		status := runs.Status(raw)
		if !status.Valid() {
			s.writeError(w, http.StatusBadRequest, CodeInvalid,
				fmt.Sprintf("unknown status %q", raw))
			return
		}
		filter.Status = status
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "limit must be a positive integer")
			return
		}
		filter.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "offset must be a non-negative integer")
			return
		}
		filter.Offset = n
	}

	list, err := s.backend.ListRuns(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []*runs.Run{}
	}
	s.writeJSON(w, http.StatusOK, RunList{SchemaVersion: SchemaVersion, Runs: list})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principalFrom(r.Context())
	if !p.allowsRun(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this run")
		return
	}

	view, err := s.backend.GetRunDetail(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principalFrom(r.Context())
	if !p.allowsRun(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this run")
		return
	}

	afterID := int64(0)
	if raw := r.URL.Query().Get("after_id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "after_id must be a non-negative integer")
			return
		}
		afterID = n
	}

	limit := 1000
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "limit must be a positive integer")
			return
		}
		limit = n
	}

	entries, err := s.backend.RunLogs(r.Context(), id, afterID, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if entries == nil {
		entries = []runs.LogEntry{}
	}
	s.writeJSON(w, http.StatusOK, LogList{SchemaVersion: SchemaVersion, Logs: entries})
}

func (s *Server) handleAppendLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principalFrom(r.Context())
	if !p.allowsRun(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this run")
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}

	var req AppendLogRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a JSON object with message and optional stream/fields")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "message must not be empty")
		return
	}
	if len(req.Message) > maxLogMessage {
		req.Message = truncateUTF8(req.Message, maxLogMessage)
	}

	stream := req.Stream
	if stream == "" {
		stream = runs.StreamOtter
	}
	switch stream {
	case runs.StreamStdout, runs.StreamStderr, runs.StreamOtter:
	default:
		s.writeError(w, http.StatusBadRequest, CodeInvalid,
			fmt.Sprintf("stream must be one of %q, %q, %q", runs.StreamStdout, runs.StreamStderr, runs.StreamOtter))
		return
	}

	if err := s.backend.AppendRunLog(r.Context(), id, stream, req.Message, req.Fields); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"status": "recorded"})
}

// handleIngestCapture accepts one bounded batch of HTTP capture events from a
// running child.
//
// A rejected batch is a 400: the submission is malformed and retrying it will
// not help. A quota rejection is not an error at all -- it is diagnostic loss,
// reported in the response body, because capture must never fail a job.
func (s *Server) handleIngestCapture(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !principalFrom(r.Context()).allowsRun(runID) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this run")
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}

	var batch inspection.EventBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid,
			"body must be a capture event batch with schema_version, policy and events")
		return
	}
	if err := inspection.ValidateBatch(batch, s.captureLimits); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}

	result, err := s.backend.IngestCaptureEvents(r.Context(), runID, batch)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, result)
}

// handleListCaptureRequests returns a run's request summaries. Operator-only:
// payload inspection is not part of a run token's narrow scope.
func (s *Server) handleListCaptureRequests(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	afterID := int64(0)
	if raw := r.URL.Query().Get("after_id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "after_id must be a non-negative integer")
			return
		}
		afterID = n
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "limit must be a positive integer")
			return
		}
		limit = n
	}

	summary, err := s.backend.CaptureSummary(r.Context(), runID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	requests, err := s.backend.ListCaptureRequests(r.Context(), runID, afterID, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if requests == nil {
		requests = []inspection.ExchangeSummary{}
	}
	s.writeJSON(w, http.StatusOK, CaptureRequestsResponse{SchemaVersion: SchemaVersion, Capture: summary, Requests: requests})
}

// handleGetCaptureRequest returns one exchange with its sanitized payloads.
func (s *Server) handleGetCaptureRequest(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	requestID := r.PathValue("request_id")

	summary, err := s.backend.CaptureSummary(r.Context(), runID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	exchange, err := s.backend.GetCaptureRequest(r.Context(), runID, requestID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, CaptureRequestResponse{SchemaVersion: SchemaVersion, Capture: summary, Request: exchange})
}

// handleGetCaptureRequestByID returns one exchange addressed by request id
// alone, learning its run from storage.
//
// The id does not identify a run: uniqueness is enforced per run, so an id that
// two runs recorded is answered with a conflict rather than an arbitrary match.
// The caller can then retry naming the run.
func (s *Server) handleGetCaptureRequestByID(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("request_id")

	exchange, err := s.backend.GetCaptureRequestByID(r.Context(), requestID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	summary, err := s.backend.CaptureSummary(r.Context(), exchange.RunID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, CaptureRequestResponse{SchemaVersion: SchemaVersion, Capture: summary, Request: exchange})
}

// handleTimeline returns one page of a finished run's merged timeline.
//
// Operator-only, matching the request-list read: the merged view exposes the
// same metadata, so it must not widen access to it. The read is bounded by the
// backend's internal budget; a read that cannot finish answers 503 rather than a
// partial page, because half a chronology is worse than a clear failure.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	query := r.URL.Query()
	limit := timeline.DefaultLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > timeline.MaxLimit {
			s.writeError(w, http.StatusBadRequest, CodeInvalid,
				fmt.Sprintf("limit must be an integer between 1 and %d", timeline.MaxLimit))
			return
		}
		limit = n
	}

	// A capture exchange is business data, so HTTP entries default on only for
	// the admin token and cannot be requested by anyone else (CL-21). A
	// scoped caller still gets the whole log-derived timeline.
	p := principalFrom(r.Context())
	includeHTTP := p.Admin
	if raw := query.Get("include_http"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "include_http must be true or false")
			return
		}
		if value && !p.Admin {
			s.writeError(w, http.StatusForbidden, CodeForbidden,
				"include_http requires the admin API token")
			return
		}
		includeHTTP = value
	}

	page, err := s.backend.TimelinePage(r.Context(), timeline.Request{
		RunID:       runID,
		IncludeHTTP: includeHTTP,
		After:       query.Get("after"),
		Limit:       limit,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.backend.CancelRun(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, CancelRunResponse{RunID: id, Status: "cancelled"})
}

// generationAllows reports whether a per-run principal may still act on an
// job. An admin always may; a run token may only while the generation
// it was issued against is still the current one, so a token minted before a
// reset, move, retirement or deletion cannot write to state that now belongs
// to a different instance.
func (s *Server) generationAllows(p principal, id string) bool {
	if p.Admin || p.Token.Generation == 0 {
		return true
	}
	current, ok := s.backend.JobGeneration(id)
	return ok && current == p.Token.Generation
}

func (s *Server) handleGetAllState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !principalFrom(r.Context()).allowsJob(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this job")
		return
	}

	if !s.generationAllows(principalFrom(r.Context()), id) {
		s.writeError(w, http.StatusConflict, CodeConflict, "the job's identity changed since this run was authorized")
		return
	}

	all, err := s.backend.AllState(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	s.writeJSON(w, http.StatusOK, StateResponse{State: all})
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key := r.PathValue("key")
	if !principalFrom(r.Context()).allowsJob(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this job")
		return
	}

	if !s.generationAllows(principalFrom(r.Context()), id) {
		s.writeError(w, http.StatusConflict, CodeConflict, "the job's identity changed since this run was authorized")
		return
	}

	value, err := s.backend.GetState(r.Context(), id, key)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// The raw JSON value is returned so that a client sees exactly what was
	// stored, without an envelope it would have to unwrap.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(value)
}

func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key := r.PathValue("key")
	if !principalFrom(r.Context()).allowsJob(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this job")
		return
	}

	if !s.generationAllows(principalFrom(r.Context()), id) {
		s.writeError(w, http.StatusConflict, CodeConflict, "the job's identity changed since this run was authorized")
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if len(bytes.TrimSpace(body)) == 0 || !json.Valid(body) {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "body must be a valid JSON value")
		return
	}

	updatedAt, err := s.backend.SetState(r.Context(), id, key, json.RawMessage(body))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusOK, SetStateResponse{
		JobID:     id,
		Key:       key,
		Value:     json.RawMessage(body),
		UpdatedAt: updatedAt,
	})
}

func (s *Server) handleDeleteState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key := r.PathValue("key")
	if !principalFrom(r.Context()).allowsJob(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this job")
		return
	}

	if !s.generationAllows(principalFrom(r.Context()), id) {
		s.writeError(w, http.StatusConflict, CodeConflict, "the job's identity changed since this run was authorized")
		return
	}

	deleted, err := s.backend.DeleteState(r.Context(), id, key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !deleted {
		s.writeError(w, http.StatusNotFound, CodeNotFound,
			fmt.Sprintf("state key %q is not set for job %q", key, id))
		return
	}
	s.writeJSON(w, http.StatusOK, DeleteStateResponse{JobID: id, Key: key, Deleted: true})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	job := r.PathValue("job")

	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}

	// Webhook bodies are not required to be JSON; anything else is preserved
	// verbatim as a JSON string so ctx.trigger.body still works.
	var raw json.RawMessage
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
		if json.Valid(trimmed) {
			raw = json.RawMessage(trimmed)
		} else {
			quoted, _ := json.Marshal(string(body))
			raw = quoted
		}
	}

	headers := make(map[string][]string, len(r.Header))
	for name, values := range r.Header {
		switch strings.ToLower(name) {
		case "authorization", "x-otter-token":
			continue // never record credentials on the run
		}
		headers[name] = values
	}

	runID, err := s.backend.SubmitRun(r.Context(), job, TriggerPayload{
		Type:    TriggerWebhook,
		Body:    raw,
		Headers: headers,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusAccepted, SubmitRunResponse{RunID: runID, Status: string(runs.StatusQueued)})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path))
}

// ------------------------------------------------------------------- helpers

// truncateUTF8 cuts a string to at most limit bytes without splitting a
// multi-byte rune, which would produce invalid UTF-8 that strict JSON clients
// reject.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " …(truncated)"
}

// fail maps a backend error onto an HTTP response.
// handleCreateToken mints a named, scoped API token. It is the only moment the
// token itself is readable; the store keeps a hash and nothing else.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req CreateAPITokenRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid,
			`body must be a JSON object with "name" and "scope"`)
		return
	}
	created, err := s.backend.CreateAPIToken(r.Context(), req.Name, req.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, created)
}

// handleListTokens lists the named tokens. Revoked ones are included so an
// operator can see what was withdrawn and when.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.backend.ListAPITokens(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, APITokenListResponse{SchemaVersion: SchemaVersion, Tokens: tokens})
}

// handleRevokeToken withdraws a token. The next request presenting it is
// refused, which is what "revoked" means here: there is no cache to expire.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	known, err := s.backend.RevokeAPIToken(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !known {
		s.writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("token %q not found", id))
		return
	}
	s.writeJSON(w, http.StatusOK, RevokeAPITokenResponse{ID: id, Revoked: true})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, runs.ErrNotFound), errors.Is(err, state.ErrNotFound),
		errors.Is(err, inspection.ErrNotFound), errors.Is(err, timeline.ErrRunNotFound):
		s.writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, ErrInvalid), errors.Is(err, state.ErrInvalidKey),
		errors.Is(err, timeline.ErrCursorInvalid):
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
	case errors.Is(err, state.ErrInvalidValue):
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
	case errors.Is(err, ErrConflict), errors.Is(err, inspection.ErrNotConfigured),
		errors.Is(err, inspection.ErrAmbiguous), errors.Is(err, timeline.ErrRunNotTerminal),
		errors.Is(err, timeline.ErrEvidenceChanged):
		s.writeError(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.Is(err, ErrPaused):
		// A paused job is temporarily not accepting autonomous
		// triggers. 503 is that statement: the route exists and the caller is
		// authorized, but this job is not taking work right now. No
		// Retry-After is offered because a pause has no known end.
		s.writeError(w, http.StatusServiceUnavailable, CodeUnavailable, err.Error())
	case errors.Is(err, ErrOverloaded):
		// The job's own queue bound is reached. 429 is the backpressure answer
		// and unlike a pause there is a known relief: the queue drains. A
		// manual run never reaches this branch, because an operator asking for
		// one run is not the backlog the bound exists to cap.
		w.Header().Set("Retry-After", "30")
		s.writeError(w, http.StatusTooManyRequests, CodeOverloaded, err.Error())
	case errors.Is(err, ErrGated):
		// The runtime is under maintenance, so every path that admits work is
		// closed and this is the answer on all of them. 503 rather than 429:
		// unlike a queue bound there is no automatic relief, only an operator
		// activating the runtime, and the reason says how. No Retry-After is
		// offered for the same reason a pause offers none.
		s.writeError(w, http.StatusServiceUnavailable, CodeUnavailable, err.Error())
	case errors.Is(err, ErrForbidden):
		s.writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
	case errors.Is(err, timeline.ErrReadDeadline):
		s.writeError(w, http.StatusServiceUnavailable, CodeUnavailable, err.Error())
	default:
		s.logger.Error("api_request_failed", err, "method", r.Method, "path", r.URL.Path)
		s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		s.logger.Error("api_marshal_failed", err)
		s.writeError(w, http.StatusInternalServerError, CodeInternal, "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(ErrorResponse{
		SchemaVersion: SchemaVersion,
		Error:         ErrorBody{Code: code, Message: message},
	})
	if err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("cannot read request body (limit %d bytes): %w", maxBodyBytes, err)
	}
	return body, nil
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
