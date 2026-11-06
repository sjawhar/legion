package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// appInstallationResponse is GET /api/v1/github/app-installation/{owner}/{repo}'s answer.
type appInstallationResponse struct {
	Installed bool `json:"installed"`
}

// getAppInstallation answers whether Dispatch's GitHub App, the one GitHub sends the webhook
// events of its repositories through, is installed on the path's repository: GitHub's
// installation lookup under the App's JWT, its 404 being false. Any authenticated caller may ask:
// the Legion daemon asks it for every repository it serves, since on a repository the App is not
// installed on GitHub sends Legion none of its events and nothing redelivers them. Dispatch with
// no App key answers 503 GITHUB_APP_UNCONFIGURED; any other failed lookup 502 GITHUB_UNREADABLE,
// naming the repository and GitHub's error.
func (s *server) getAppInstallation(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	repo, err := repoProjectPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	owner, name, _ := strings.Cut(repo, "/")
	installed, err := s.deps.GitHub.Installed(r.Context(), owner, name)
	switch {
	case errors.Is(err, githubapp.ErrNoAppKey):
		writeError(w, "GITHUB_APP_UNCONFIGURED", http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, "GITHUB_UNREADABLE", http.StatusBadGateway, fmt.Sprintf("read the GitHub App's installation on %s: %v", repo, err))
	default:
		WriteJSON(w, http.StatusOK, appInstallationResponse{Installed: installed})
	}
}
