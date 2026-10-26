// packages/envoy/cmd/agent-secrets/narrow.go
//
// The exec form's --github-owner, --github-repo and --github-read-only: a narrowing of the
// release's GitHub App tokens (POST /v1/grants/{id}/values's github_app), which the broker applies
// to every github-app secret the grant holds, and which the exec form checks the broker applied
// before it runs anything.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// repositoryFlag is --github-repo's value: every name it was given, in order.
type repositoryFlag []string

func (r *repositoryFlag) String() string { return strings.Join(*r, ",") }

func (r *repositoryFlag) Set(name string) error {
	*r = append(*r, name)
	return nil
}

// githubNarrowing is the narrowing the flags ask for, nil when they ask for none: the owner
// lowercased and the repositories sorted, each once, as the broker names what it applied, so
// narrowedAsAsked compares like with like. --github-repo or --github-read-only without
// --github-owner is refused, since a narrowing names one account's token.
func githubNarrowing(owner string, repositories []string, readOnly bool) (*GitHubNarrowing, error) {
	if owner == "" {
		if len(repositories) > 0 || readOnly {
			return nil, errors.New("--github-repo and --github-read-only narrow one account's GitHub App token: name the account with --github-owner")
		}
		return nil, nil
	}
	sorted := slices.Clone(repositories)
	slices.Sort(sorted)
	return &GitHubNarrowing{Owner: strings.ToLower(owner), Repositories: slices.Compact(sorted), ReadOnly: readOnly}, nil
}

// narrowedAsAsked checks that the broker applied asked to the release it answered with applied. A
// broker from before narrowing ignores the release's body and answers no github_app, having
// released the owner-wide tokens, which the command must not get in place of the narrowed one.
// This check stays: a client can meet an older broker on every future independent rollout.
func narrowedAsAsked(asked, applied *GitHubNarrowing) error {
	if asked == nil {
		return nil
	}
	if applied != nil && applied.Owner == asked.Owner && slices.Equal(applied.Repositories, asked.Repositories) && applied.ReadOnly == asked.ReadOnly {
		return nil
	}
	answered := "no narrowing, as a broker from before narrowing does when it releases the owner-wide token"
	if applied != nil {
		encoded, _ := json.Marshal(applied)
		answered = string(encoded)
	}
	encoded, _ := json.Marshal(asked)
	return fmt.Errorf("the broker did not narrow the GitHub App token to %s: it answered %s", encoded, answered)
}
