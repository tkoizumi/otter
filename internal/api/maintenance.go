package api

import (
	"encoding/json"
	"net/http"
)

// handleGetMaintenance reports the runtime's maintenance state to an admin
// caller. It is separate from /health because an operator scripted against a
// maintenance window should not have to parse a liveness document to find out
// whether the runtime is accepting work.
func (s *Server) handleGetMaintenance(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.Maintenance(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// enterMaintenanceRequest is the optional body of a POST. An empty body is
// legal: a reason is useful for the audit trail but must not be required, or a
// deploy script that just needs the gate would have to invent one.
type enterMaintenanceRequest struct {
	Reason string `json:"reason,omitempty"`
}

// handleEnterMaintenance closes every path that admits work.
//
// The response is the resulting state rather than a bare success, because the
// interesting fact is *which* mode the runtime landed in: a runtime with work
// still executing is draining, not yet in maintenance, and an operator waiting
// on a window needs to see that difference rather than assume it.
func (s *Server) handleEnterMaintenance(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var req enterMaintenanceRequest
	if len(body) > 0 {
		if !json.Valid(body) {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, "request body must be a JSON object or empty")
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
			return
		}
	}

	view, err := s.backend.EnterMaintenance(r.Context(), req.Reason)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleExitMaintenance activates the runtime.
//
// This is the audited activation step: it is the only way out of maintenance,
// it is admin-only, and it is deliberately explicit rather than automatic on
// some health signal. A runtime that activated itself would defeat the point of
// starting gated.
func (s *Server) handleExitMaintenance(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.ExitMaintenance(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}
