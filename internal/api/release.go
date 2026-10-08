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
// handleActiveReleases reports what each job is currently serving.
//
// Read-only and admin-scoped like the rest of the release surface: it names the
// code a tenant is running, which is operator information.
func (s *Server) handleActiveReleases(w http.ResponseWriter, r *http.Request) {
	views, err := s.backend.ActiveReleases(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if views == nil {
		// An empty list, not null: a caller iterating it should not have to guard
		// against a null that means the same thing.
		views = []ReleaseView{}
	}
	// The Cloud-managed set travels WITH the active releases rather than on its
	// own endpoint, and `managed_reconciliation` is the capability marker: a
	// runtime that does not send it cannot be asked to remove a managed job, and
	// an agent that reads its absence keeps the single-release behaviour instead
	// of removing releases nothing asked it to remove.
	managed, err := s.backend.ManagedReleaseJobs(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if managed == nil {
		managed = []string{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"active":                 views,
		"managed_jobs":           managed,
		"managed_reconciliation": true,
	})
}

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
