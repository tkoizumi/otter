package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tkoizumi/otter/internal/replay"
)

// maxReplayRequestBytes bounds the loopback request. It carries two ids; a body
// larger than this is a client bug, not a request to parse.
const maxReplayRequestBytes = 64 * 1024

// handleReplay resends one captured exchange.
//
// Authority is decided by Cloud (session, owner/operator, per-runtime flag,
// audit, rate limit). This route is the runtime's execution half: it is
// loopback-only and admits the runtime's own agent, and it re-applies the
// capture guards itself so a compromised control plane cannot talk it into
// resending something that cannot be reconstructed exactly.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunID     string `json:"run_id"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReplayRequestBytes)).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "replay body must be {\"run_id\": ..., \"request_id\": ...}")
		return
	}
	runID := strings.TrimSpace(body.RunID)
	requestID := strings.TrimSpace(body.RequestID)
	if runID == "" || requestID == "" {
		s.writeError(w, http.StatusBadRequest, CodeInvalid, "replay needs run_id and request_id")
		return
	}

	result, err := s.backend.ReplayCapturedRequest(r.Context(), runID, requestID)
	if err != nil {
		var refusal *replay.Refusal
		if errors.As(err, &refusal) {
			// A refusal is a state, not a malformed request: the exchange is
			// real, it just cannot be resent exactly as captured.
			s.writeError(w, http.StatusConflict, CodeConflict, refusal.Reason)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
