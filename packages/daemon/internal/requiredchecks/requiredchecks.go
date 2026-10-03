// Package requiredchecks reads which checks a pull request's base branch requires from GitHub
// (Required): the one reader both the merger's READY (`legion handoff complete --ready`) and the
// daemon's workflow use, since only a required check decides whether CI is red at a head
// (classify.Judge). What each judges against the set is its own: READY the head's check runs and
// commit statuses as GitHub reports them, the workflow the Envoy settlement that stands for the
// head.
package requiredchecks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// RepositoryAPI is repository's REST base under endpoint, GitHub's API root; empty is
// https://api.github.com.
func RepositoryAPI(endpoint string, repository ghrepo.Repository) string {
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return strings.TrimRight(endpoint, "/") + "/repos/" + repository.String()
}

// GitHub reads one repository's GitHub REST API with an installation token: API is the
// repository's REST base (RepositoryAPI).
type GitHub struct {
	Token string
	API   string
}

// Get reads path under the repository's REST base into into. An answer other than 200 is an
// *Answer.
func (g GitHub) Get(ctx context.Context, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.API+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+g.Token)
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
		return &Answer{Path: path, Status: response.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return json.Unmarshal(body, into)
}

// Answer is a GitHub REST answer other than 200.
type Answer struct {
	Path   string
	Status int
	Body   string
}

func (a *Answer) Error() string {
	return fmt.Sprintf("GitHub answered GET %s with %d: %s", a.Path, a.Status, a.Body)
}

// Required is every check name base requires: the required status checks of the rulesets that
// apply to it, and of its branch protection, sorted and each once. A repository whose plan has no
// rulesets has none of the first (rulesetsUnavailable). An empty answer is a base that requires no
// check.
func Required(ctx context.Context, github GitHub, base string) ([]string, error) {
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
	if err := github.Get(ctx, "/rules/branches/"+branch, &rules); err != nil && !rulesetsUnavailable(err) {
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
	if err := github.Get(ctx, "/branches/"+branch, &protected); err != nil {
		return nil, err
	}
	names := []string{}
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

// rulesetsUnavailable is GitHub's answer to the rulesets read of a private repository whose plan
// has no rulesets: 403, its message ending "make this repository public to enable this feature"
// (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md, observed on
// sjawhar/legion-smoke). Such a repository can define no ruleset, so no ruleset requires a check.
func rulesetsUnavailable(err error) bool {
	var answer *Answer
	return errors.As(err, &answer) && answer.Status == http.StatusForbidden && strings.Contains(answer.Body, "make this repository public to enable this feature")
}
