package api

import (
	"errors"
	"net/http"
)

// handleInstallRelease accepts a portable release package and installs it.
//
// Admin-scoped and gated on maintenance, exactly as activation is: installing
// changes which releases exist, and doing it while the runtime serves could alter
// what a running attempt resolves.
//
// The body is the package directly rather than multipart or JSON-wrapped, because
// the bytes are the identity: any wrapper would have to be excluded from the
// digest, and that exclusion is a place for the verified content and the executed
// content to diverge.
//
// Every refusal is mapped to a STATUS the uploader can act on. The daemon owns
// the release semantics; this layer only turns them into HTTP.
func (s *Server) handleInstallRelease(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.InstallRelease(r.Context(), r.Body)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalid):
			s.writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		case errors.Is(err, ErrConflict):
			s.writeError(w, http.StatusConflict, CodeConflict, err.Error())
		default:
			s.writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		}
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}
