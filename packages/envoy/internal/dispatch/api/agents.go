package api

import (
	"errors"
	"net/http"
	"sort"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
)

func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}

	sessions, err := s.deps.Envoy.Sessions(r.Context())
	if err != nil {
		if errors.Is(err, envoy.ErrUnavailable) {
			writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, err.Error())
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	sort.SliceStable(sessions, func(left, right int) bool {
		if sessions[left].LastSeen != sessions[right].LastSeen {
			return sessions[left].LastSeen > sessions[right].LastSeen
		}
		return sessions[left].SessionID < sessions[right].SessionID
	})
	writeJSON(w, http.StatusOK, sessions)
}
