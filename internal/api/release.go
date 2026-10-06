package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

// handleActivateRelease promotes a release by digest.
//
// Admin-only, because activating a release decides which code a tenant runs.
// The runtime must be in maintenance: activation during serving would swap the
// active release under running work, and the refusal is explicit so a caller
// learns the ordering requirement from the error rather than from a corrupt
// deployment.
func (s *Server) handleActivateRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "malformed request body")
		return
	}
	view, err := s.backend.ActivateRelease(r.Context(), body.Digest)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalid):
			s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		case errors.Is(err, ErrNotFound):
			s.writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
		case errors.Is(err, ErrConflict):
			s.writeError(w, http.StatusConflict, CodeConflict, err.Error())
		default:
			s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		}
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}
