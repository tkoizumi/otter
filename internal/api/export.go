package api

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"

	"github.com/tkoizumi/otter/internal/inspection"
)

// The capture export relay's local half.
//
// The agent is the only process allowed to carry a document out of the tenant,
// and it reaches these endpoints over loopback with its agent-scoped runtime
// token. Cloud derives the tenant prefix from the agent credential, never from
// anything here, so these handlers only move rows and documents.

func (s *Server) handlePullCaptureExports(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "after must be a non-negative row id")
			return
		}
		after = parsed
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "limit must be a non-negative integer")
			return
		}
		limit = parsed
	}

	batch, err := s.backend.PullCaptureExports(r.Context(), after, limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if batch == nil {
		batch = &inspection.ExportBatch{Documents: []inspection.ExportDocument{}, Cursor: after}
	}
	if batch.Documents == nil {
		batch.Documents = []inspection.ExportDocument{}
	}
	s.writeJSON(w, http.StatusOK, batch)
}

func (s *Server) handleAckCaptureExports(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Documents []inspection.ExportRef `json:"documents"`
		State     string                 `json:"state"`
		Error     string                 `json:"error,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "malformed request body")
		return
	}
	if len(body.Documents) == 0 {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "documents must not be empty")
		return
	}
	if err := s.backend.AckCaptureExports(r.Context(), body.Documents, body.State, body.Error); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// local admits only a request that arrived on a loopback connection.
//
// The export endpoints move tenant data, and the runtime's API can be bound to
// a non-loopback address for an operator. The agent always reaches it over
// loopback, so refusing a remote connection removes a path that should not
// exist rather than relying on the token alone.
func (s *Server) local(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			s.writeError(w, http.StatusForbidden, CodeForbidden, "capture export is loopback-only")
			return
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			s.writeError(w, http.StatusForbidden, CodeForbidden, "capture export is loopback-only")
			return
		}
		h.ServeHTTP(w, r)
	})
}
