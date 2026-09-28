package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// githubRemote is a GitHub repository's clone URL as a workspace's origin names it.
var githubRemote = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// readyChecks refuses a READY whose head GitHub will not merge for its checks: every check the
// base branch requires - its rulesets' required status checks and its branch protection's - must
// have succeeded on the pull request's head. A head whose push skipped CI when it should not have
// (legion push's rule) reports none of them; it is refused here, naming the head and the check,
// rather than left for GitHub to block the human merge.
//
// A base branch that requires no check has nothing to refuse, and READY is published. It says so
// on stdout rather than reading like a head whose every required check was read and passed: a
// private repository on the free plan can define no ruleset, so this is the ordinary state of the
// smoke sandbox, and a merger there has no check-based gate on the head at all.
func readyChecks(ctx context.Context, workspace string, issue paneIssue, stdout io.Writer) error {
	if issue.PullRequest == nil || issue.PullRequest.Number <= 0 {
		return fmt.Errorf("the daemon records no pull request for %s", os.Getenv("LEGION_ISSUE"))
	}
	repository, err := workspaceRepository(workspace)
	if err != nil {
		return err
	}
	response, err := redeemGrant(ctx, "/legion/v1/gh-token")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var credential githubTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&credential); err != nil || credential.Token == "" {
		return fmt.Errorf("the daemon returned no GitHub token")
	}
	github := githubREST{token: credential.Token, base: repositoryAPI(repository)}
	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := github.get(ctx, fmt.Sprintf("/pulls/%d", issue.PullRequest.Number), &pull); err != nil {
		return err
	}
	required, err := requiredChecks(ctx, github, pull.Base.Ref)
	if err != nil {
		return err
	}
	if len(required) == 0 {
		fmt.Fprintf(stdout, "[handoff] no check is required on %q of %s, so READY was published without reading the head's checks\n", pull.Base.Ref, repository)
		return nil
	}
	results, err := headCheckResults(ctx, github, pull.Head.SHA)
	if err != nil {
		return err
	}
	head := pull.Head.SHA
	if len(head) > 12 {
		head = head[:12]
	}
	for _, name := range required {
		switch result, reported := results[name]; {
		case !reported:
			return fmt.Errorf("head %s of pull request #%d has no result for the required check %q: its push may have skipped CI when it should not have; tell the architect", head, issue.PullRequest.Number, name)
		case result == "pending":
			return fmt.Errorf("the required check %q is still running on head %s of pull request #%d: wait for it to finish", name, head, issue.PullRequest.Number)
		case result != "success":
			return fmt.Errorf("the required check %q ended %s on head %s of pull request #%d", name, result, head, issue.PullRequest.Number)
		}
	}
	return nil
}

// workspaceRepository is the GitHub repository the workspace's origin names.
func workspaceRepository(workspace string) (ghrepo.Repository, error) {
	remotes, err := pushJJ(os.Getenv("LEGION_JJ_PATH"), workspace, "git", "remote", "list")
	if err != nil {
		return ghrepo.Repository{}, err
	}
	for _, line := range strings.Split(remotes, "\n") {
		name, remote, _ := strings.Cut(line, " ")
		if name != "origin" {
			continue
		}
		match := githubRemote.FindStringSubmatch(strings.TrimSpace(remote))
		if match == nil {
			return ghrepo.Repository{}, fmt.Errorf("the workspace's origin %q is not a GitHub repository", remote)
		}
		return ghrepo.Parse("the workspace's origin", match[1])
	}
	return ghrepo.Repository{}, fmt.Errorf("the workspace has no origin remote")
}

// requiredChecks is every check name base requires: the required status checks of the rulesets
// that apply to it, and of its branch protection. A repository whose plan has no rulesets has none
// of the first (rulesetsUnavailable).
func requiredChecks(ctx context.Context, github githubREST, base string) ([]string, error) {
	// A branch name's slashes stay path segments, as GitHub's branch routes take them.
	branch := strings.ReplaceAll(url.PathEscape(base), "%2F", "/")
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := github.get(ctx, "/rules/branches/"+branch, &rules); err != nil && !rulesetsUnavailable(err) {
		return nil, err
	}
	var protected struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := github.get(ctx, "/branches/"+branch, &protected); err != nil {
		return nil, err
	}
	var names []string
	for _, rule := range rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, check := range rule.Parameters.RequiredStatusChecks {
			names = append(names, check.Context)
		}
	}
	names = append(names, protected.Protection.RequiredStatusChecks.Contexts...)
	for _, check := range protected.Protection.RequiredStatusChecks.Checks {
		names = append(names, check.Context)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// headCheckResults is each check and commit status reported on sha, by name: "success", "pending",
// or the failing conclusion or state. A check run that ended neutral or skipped counts as a
// success, as GitHub counts it for a required check.
func headCheckResults(ctx context.Context, github githubREST, sha string) (map[string]string, error) {
	results := map[string]string{}
	for page := 1; ; page++ {
		var runs struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := github.get(ctx, fmt.Sprintf("/commits/%s/check-runs?per_page=100&page=%d", sha, page), &runs); err != nil {
			return nil, err
		}
		for _, run := range runs.CheckRuns {
			switch {
			case run.Status != "completed":
				results[run.Name] = "pending"
			case run.Conclusion == "success" || run.Conclusion == "neutral" || run.Conclusion == "skipped":
				results[run.Name] = "success"
			default:
				results[run.Name] = run.Conclusion
			}
		}
		if len(runs.CheckRuns) == 0 || page*100 >= runs.TotalCount {
			break
		}
	}
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := github.get(ctx, fmt.Sprintf("/commits/%s/status?per_page=100", sha), &combined); err != nil {
		return nil, err
	}
	for _, status := range combined.Statuses {
		results[status.Context] = status.State
	}
	return results, nil
}

// repositoryAPI is the REST base of a repository: LEGION_GITHUB_API_URL (tests), else api.github.com.
func repositoryAPI(repository ghrepo.Repository) string {
	endpoint := os.Getenv("LEGION_GITHUB_API_URL")
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return strings.TrimRight(endpoint, "/") + "/repos/" + repository.String()
}

// githubREST reads one repository's GitHub REST API with an installation token.
type githubREST struct{ token, base string }

func (g githubREST) get(ctx context.Context, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+g.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return githubAnswer{path: path, status: response.StatusCode, body: strings.TrimSpace(string(body))}
	}
	return json.Unmarshal(body, into)
}

// githubAnswer is a GitHub REST answer other than 200.
type githubAnswer struct {
	path   string
	status int
	body   string
}

func (a githubAnswer) Error() string {
	return fmt.Sprintf("GitHub answered GET %s with %d: %s", a.path, a.status, a.body)
}

// rulesetsUnavailable is GitHub's answer to the rulesets read of a private repository whose plan
// has no rulesets: 403, its message ending "make this repository public to enable this feature"
// (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md, observed on
// sjawhar/legion-smoke). Such a repository can define no ruleset, so no ruleset requires a check.
func rulesetsUnavailable(err error) bool {
	var answer githubAnswer
	return errors.As(err, &answer) && answer.status == http.StatusForbidden && strings.Contains(answer.body, "make this repository public to enable this feature")
}
