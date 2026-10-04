// Package requiredchecks reads which checks a pull request's base branch requires from GitHub
// (Required): the one reader both the merger's READY (`legion handoff complete --ready`) and the
// daemon's workflow use, since only a required check decides whether CI is red at a head
// (classify.Judge). What each judges against the set is its own: READY the head's check runs and
// commit statuses as GitHub reports them, the workflow the Envoy settlement that stands for the
// head.
package requiredchecks

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// Required is every check name base requires: the required status checks of the rulesets that
// apply to it, every page of them, and of its branch protection, sorted and each once. A
// repository whose plan has no rulesets has none of the first (rulesetsUnavailable). An empty
// answer is a base that requires no check.
func Required(ctx context.Context, github githubrest.Client, base string) ([]string, error) {
	// A branch name's slashes stay path segments, as GitHub's branch routes take them.
	branch := strings.ReplaceAll(url.PathEscape(base), "%2F", "/")
	type rule struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	rules, err := githubrest.GetPages[rule](ctx, github, "/rules/branches/"+branch)
	if err != nil && !rulesetsUnavailable(err) {
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
	var answer *githubrest.Answer
	return errors.As(err, &answer) && answer.Status == http.StatusForbidden && strings.Contains(answer.Body, "make this repository public to enable this feature")
}
