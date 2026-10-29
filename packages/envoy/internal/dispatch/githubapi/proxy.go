// Package githubapi answers the web app's GitHub reads: a pull request's or issue's state, and a
// commit's checks and the workflow runs that made them, read as the GitHub App installation that
// covers the repository.
package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// Prefix is the route the web app reads GitHub under: /api/github/rest/<GitHub API path>.
const Prefix = "/api/github/rest/"

// readable are the GitHub API paths the web app reads, and the only ones the proxy forwards: a
// pull request, an issue (which also answers for a pull request), the check runs of a commit, and
// the workflow runs (`actions/runs`, group 3) whose query names one commit, held to
// workflowRunsQuery. The App can read more of a repository than that, and none of the rest is
// shown anywhere.
var readable = regexp.MustCompile(`^repos/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(?:pulls/[0-9]+|issues/[0-9]+|commits/[0-9A-Fa-f]{7,64}/check-runs|(actions/runs))$`)

// workflowRunsQuery is each parameter a workflow-runs read may carry, as sent: the commit's full
// sha and the page. The read must name head_sha, and no parameter may repeat.
var workflowRunsQuery = regexp.MustCompile(`^(head_sha)=[0-9A-Fa-f]{40}$|^(per_page|page)=[0-9]+$`)

// workflowRunsResponseLimit bounds GitHub's workflow-runs answer, which the proxy trims to three
// fields per run before forwarding. GitHub writes each run's repository, head repository and head
// commit (about 12.8 KB a run on sjawhar/legion), so a page of 100 runs passes the client's 1 MiB
// default; this leaves room for 100 runs whose shared head commit message is long.
const workflowRunsResponseLimit = 16 << 20

// workflowRuns is the part of GitHub's workflow-runs answer the web app reads, and all of it the
// proxy passes on: the count and, per run in GitHub's order, its check suite, triggering event and
// workflow name. The rest of a run (its head commit's message and author, the actor, the
// repository) is never shown, so it never leaves the server.
type workflowRuns struct {
	TotalCount   int `json:"total_count"`
	WorkflowRuns []struct {
		CheckSuiteID int64  `json:"check_suite_id"`
		Event        string `json:"event"`
		Name         string `json:"name"`
	} `json:"workflow_runs"`
}

// workflowRunsOfOneCommit reports whether rawQuery reads the workflow runs of one commit and
// nothing else: head_sha as a full sha, and at most per_page and page beside it.
func workflowRunsOfOneCommit(rawQuery string) bool {
	seen := map[string]bool{}
	for _, parameter := range strings.Split(rawQuery, "&") {
		match := workflowRunsQuery.FindStringSubmatch(parameter)
		if match == nil {
			return false
		}
		name := match[1] + match[2]
		if seen[name] {
			return false
		}
		seen[name] = true
	}
	return seen["head_sha"]
}

// ProxyREST answers GET Prefix<path> with GitHub's answer to GET /<path>, read with an
// installation token of the App installation covering the path's repository. A workflow-runs read
// GitHub answers 200 is trimmed to workflowRuns, and a body that does not decode as workflow runs
// is 502 GITHUB_UPSTREAM; every other answer, and a workflow-runs answer of any other status,
// passes through as GitHub sent it. Any other method is 405; a path the web app does not read, or
// a workflow-runs read whose query is not one commit's head_sha and its page, is 404
// GITHUB_PATH_REFUSED; a repository the App is not installed on, or a deployment with no App key,
// is 503 GITHUB_TOKEN_UNAVAILABLE. The caller has already established that a person is asking.
func ProxyREST(w http.ResponseWriter, r *http.Request, client *githubapp.Client) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		api.WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "the GitHub proxy reads only: use GET", "code": "METHOD_NOT_ALLOWED"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	match := readable.FindStringSubmatch(path)
	if match == nil || match[1] == "." || match[1] == ".." || match[2] == "." || match[2] == ".." ||
		(match[3] != "" && !workflowRunsOfOneCommit(r.URL.RawQuery)) {
		api.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "the GitHub proxy reads a pull request, an issue, a commit's check runs or a commit's workflow runs only", "code": "GITHUB_PATH_REFUSED"})
		return
	}
	owner, repo := match[1], match[2]
	token, err := client.RepositoryToken(r.Context(), owner, repo)
	switch {
	case errors.Is(err, githubapp.ErrNoAppKey):
		unavailable(w, "the GitHub App is not configured")
		return
	case errors.Is(err, githubapp.ErrNoInstallation):
		unavailable(w, "the GitHub App is not installed on "+owner+"/"+repo)
		return
	case err != nil:
		slog.Warn("dispatch: GitHub proxy could not mint an installation token", "repository", owner+"/"+repo, "error", err)
		api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub installation token: " + err.Error(), "code": "GITHUB_UPSTREAM"})
		return
	}
	target := "/" + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	read := client.Read
	if match[3] != "" {
		read = func(ctx context.Context, token, path string) ([]byte, int, http.Header, error) {
			return client.ReadWithin(ctx, token, path, workflowRunsResponseLimit)
		}
	}
	body, status, header, err := read(r.Context(), token, target)
	if err != nil {
		slog.Warn("dispatch: GitHub proxy read failed", "path", target, "error", err)
		var tooLarge *githubapp.ResponseTooLargeError
		if errors.As(err, &tooLarge) {
			api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": tooLarge.Error(), "code": "GITHUB_UPSTREAM"})
			return
		}
		api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub read failed", "code": "GITHUB_UPSTREAM"})
		return
	}
	if match[3] != "" && status == http.StatusOK {
		var runs workflowRuns
		if err := json.Unmarshal(body, &runs); err != nil {
			slog.Warn("dispatch: GitHub proxy could not decode the workflow runs", "path", target, "error", err)
			api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub answered the workflow runs with a body that does not decode", "code": "GITHUB_UPSTREAM"})
			return
		}
		api.WriteJSON(w, http.StatusOK, runs)
		return
	}
	if contentType := header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func unavailable(w http.ResponseWriter, message string) {
	api.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message, "code": "GITHUB_TOKEN_UNAVAILABLE"})
}
