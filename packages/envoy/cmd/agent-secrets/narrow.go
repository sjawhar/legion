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

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// githubNarrowing is the narrowing the flags ask for, nil when they ask for none, in the one form
// the broker applies a narrowing and names it back (policy.CanonicalGitHubNarrowing), so
// narrowedAsAsked compares like with like. ownerSet says whether --github-owner was given at all:
// given with no account (an unset variable a caller passed) it is refused rather than read as no
// narrowing, which would release the owner-wide tokens; and --github-repo or --github-read-only
// without --github-owner is refused, since a narrowing names one account's token. Whether the
// owner is one login and each repository a name GitHub can hold is the broker's to check
// (INVALID_NARROWING).
func githubNarrowing(owner string, ownerSet bool, repositories []string, readOnly bool) (*GitHubNarrowing, error) {
	switch {
	case ownerSet && strings.TrimSpace(owner) == "":
		return nil, errors.New("--github-owner names no account: give the account whose GitHub App token to narrow")
	case !ownerSet && (len(repositories) > 0 || readOnly):
		return nil, errors.New("--github-repo and --github-read-only narrow one account's GitHub App token: name the account with --github-owner")
	case !ownerSet:
		return nil, nil
	}
	canonical := policy.CanonicalGitHubNarrowing(owner, repositories, readOnly)
	return &GitHubNarrowing{Owner: canonical.Owner, Repositories: canonical.Repositories, ReadOnly: canonical.ReadOnly}, nil
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
