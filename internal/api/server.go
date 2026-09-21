// Package api implements Otter's local HTTP API and the CLI's HTTP client.
//
// The API is the only interface between the daemon, the CLI and running
// integration processes. It binds to loopback by default; binding elsewhere
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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/state"
)

const (
	maxBodyBytes  = 1 << 20 // 1 MiB
	maxLogMessage = 64 * 1024
)

// ServerConfig configures the HTTP API.
type ServerConfig struct {
	Listen   string
	APIToken string

	// OnReady is called once the listener is bound, with the address the
	// kernel actually chose -- which is not necessarily the configured one
	// when it names port 0. A daemon that will be addressed by other
	// processes needs the resolved value, not the request.
	OnReady func(addr string)
}

// Server serves the Otter HTTP API.
type Server struct {
	cfg     ServerConfig
	backend Backend
	logger  *logging.Logger
	http    *http.Server
}

// NewServer wires the API routes.
func NewServer(cfg ServerConfig, backend Backend, logger *logging.Logger) *Server {
	s := &Server{cfg: cfg, backend: backend, logger: logger}
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

	// Admin-only: these act on the whole runtime.
	mux.Handle("GET /v1/integrations", s.admin(s.handleListIntegrations))
	mux.Handle("POST /v1/reload", s.admin(s.handleReload))
	mux.Handle("POST /v1/integrations/{id}/runs", s.admin(s.handleSubmitRun))
	mux.Handle("GET /v1/runs", s.admin(s.handleListRuns))
	mux.Handle("POST /v1/runs/{id}/cancel", s.admin(s.handleCancelRun))

	// Reachable with either the admin token or a per-run token; the handler
	// narrows the scope further.
	mux.Handle("GET /v1/integrations/{id}", s.principal(s.handleGetIntegration))
	mux.Handle("GET /v1/runs/{id}", s.principal(s.handleGetRun))
	mux.Handle("GET /v1/runs/{id}/logs", s.principal(s.handleGetLogs))
	mux.Handle("POST /v1/runs/{id}/logs", s.principal(s.handleAppendLog))
	mux.Handle("GET /v1/integrations/{id}/state", s.principal(s.handleGetAllState))
	mux.Handle("GET /v1/integrations/{id}/state/{key}", s.principal(s.handleGetState))
	mux.Handle("PUT /v1/integrations/{id}/state/{key}", s.principal(s.handleSetState))
	mux.Handle("DELETE /v1/integrations/{id}/state/{key}", s.principal(s.handleDeleteState))

	// Webhooks authenticate with their own per-integration token.
	mux.Handle("POST /v1/hooks/{integration}", s.webhook(s.handleWebhook))

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
	Token RunToken
}

func (p principal) allowsIntegration(id string) bool {
	return p.Admin || (p.Token.IntegrationID != "" && p.Token.IntegrationID == id)
}

func (p principal) allowsRun(id string) bool {
	return p.Admin || (p.Token.RunID != "" && p.Token.RunID == id)
}

type principalCtxKey struct{}

func principalFrom(ctx context.Context) principal {
	if p, ok := ctx.Value(principalCtxKey{}).(principal); ok {
		return p
	}
	return principal{}
}

func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if !p.Admin {
			s.writeError(w, http.StatusForbidden, CodeForbidden,
				"this endpoint requires the admin API token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalCtxKey{}, p)))
	})
}

func (s *Server) principal(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalCtxKey{}, p)))
	})
}

// resolvePrincipal identifies the caller without writing a response, so that
// endpoints such as /health can offer a richer answer to an authenticated
// caller and a minimal one to everyone else.
func (s *Server) resolvePrincipal(r *http.Request) (principal, bool) {
	if token := bearerToken(r); token != "" {
		if s.cfg.APIToken != "" && constantTimeEqual(token, s.cfg.APIToken) {
			return principal{Admin: true}, true
		}
		if scope, ok := s.backend.ResolveRunToken(token); ok {
			return principal{Token: scope}, true
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

// webhook authenticates a webhook call against the integration's own token.
func (s *Server) webhook(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		integration := r.PathValue("integration")
		if integration == "" {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "integration is required")
			return
		}

		expected, ok := s.backend.WebhookTokenFor(integration)
		if !ok {
			// Deliberately identical for "unknown integration" and "webhook
			// disabled" so the endpoint does not enumerate integrations.
			s.writeError(w, http.StatusNotFound, CodeNotFound,
				fmt.Sprintf("integration %q does not accept webhook triggers", integration))
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
// disclosing integration and run counts to the network.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:        "ok",
		Version:       s.backend.Version(),
		UptimeSeconds: time.Since(s.backend.StartedAt()).Seconds(),
	}

	if _, authenticated := s.resolvePrincipal(r); !authenticated {
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	views := s.backend.ListIntegrations()
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

	resp.Integrations = counts
	resp.QueueDepth = &depth
	resp.Runs = runCounts

	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"integrations": s.backend.ListIntegrations()})
}

// handleReload re-reads the integrations directory against the running daemon.
// It is admin-only because it changes what the whole runtime knows about, and
// therefore what every other caller can address.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	result, err := s.backend.Reload(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetIntegration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principalFrom(r.Context())
	if !p.allowsIntegration(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this integration")
		return
	}

	view, ok := s.backend.GetIntegration(id)
	if !ok {
		s.writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("integration %q not found", id))
		return
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

	runID, err := s.backend.SubmitRun(r.Context(), id, payload)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusAccepted, SubmitRunResponse{RunID: runID, Status: string(runs.StatusQueued)})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := runs.Filter{
		IntegrationID: q.Get("integration_id"),
		ParentRunID:   q.Get("parent_run_id"),
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
	s.writeJSON(w, http.StatusOK, map[string]any{"runs": list})
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
	s.writeJSON(w, http.StatusOK, map[string]any{"logs": entries})
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

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.backend.CancelRun(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, CancelRunResponse{RunID: id, Status: "cancelled"})
}

func (s *Server) handleGetAllState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !principalFrom(r.Context()).allowsIntegration(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this integration")
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
	if !principalFrom(r.Context()).allowsIntegration(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this integration")
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
	if !principalFrom(r.Context()).allowsIntegration(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this integration")
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
		IntegrationID: id,
		Key:           key,
		Value:         json.RawMessage(body),
		UpdatedAt:     updatedAt,
	})
}

func (s *Server) handleDeleteState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key := r.PathValue("key")
	if !principalFrom(r.Context()).allowsIntegration(id) {
		s.writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this integration")
		return
	}

	deleted, err := s.backend.DeleteState(r.Context(), id, key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !deleted {
		s.writeError(w, http.StatusNotFound, CodeNotFound,
			fmt.Sprintf("state key %q is not set for integration %q", key, id))
		return
	}
	s.writeJSON(w, http.StatusOK, DeleteStateResponse{IntegrationID: id, Key: key, Deleted: true})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	integration := r.PathValue("integration")

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

	runID, err := s.backend.SubmitRun(r.Context(), integration, TriggerPayload{
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
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, runs.ErrNotFound), errors.Is(err, state.ErrNotFound):
		s.writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, ErrInvalid), errors.Is(err, state.ErrInvalidKey):
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
	case errors.Is(err, state.ErrInvalidValue):
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
	case errors.Is(err, ErrConflict):
		s.writeError(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.Is(err, ErrForbidden):
		s.writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
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
	body, err := json.Marshal(ErrorResponse{Error: ErrorBody{Code: code, Message: message}})
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
