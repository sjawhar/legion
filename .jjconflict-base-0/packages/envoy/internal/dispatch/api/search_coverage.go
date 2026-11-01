package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
)

// searchCoverage answers GET /api/v1/search/coverage: how much of what meaning search ranks has a
// current vector from the embedder search embeds its queries with (embedqueue.Coverage, INFRA-393),
// counted in one statement so every number reads one snapshot. The identity is Deps.Embedder's,
// the client cmd/dispatch builds and never wraps for search, so it is the identity search filters
// candidates by; with no embedder configured it is empty, the report's identity, place and model
// are null and nothing counts as current.
func (s *server) searchCoverage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	identity := ""
	if s.deps.Embedder != nil {
		identity = s.deps.Embedder.Identity()
	}
	report, err := embedqueue.Coverage(r.Context(), s.deps.Store, identity)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, report)
}
