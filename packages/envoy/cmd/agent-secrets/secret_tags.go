// packages/envoy/cmd/agent-secrets/secret_tags.go
//
// The secret forms' tag rules: what each tag flag accepts, under the broker's own rules, and the
// tags a write sends.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// checkOwnerTier refuses an --owner that is neither me nor shared and a --tier that is neither
// agent nor human; "" passes either, for a form where it is optional.
func checkOwnerTier(owner, tier string) error {
	var errs []error
	if owner != "" && owner != "me" && owner != policy.OwnerShared {
		errs = append(errs, fmt.Errorf("--owner is me or %s, not %q", policy.OwnerShared, owner))
	}
	if tier != "" && tier != policy.TierAgent && tier != policy.TierHuman {
		errs = append(errs, fmt.Errorf("--tier is %s or %s, not %q", policy.TierAgent, policy.TierHuman, tier))
	}
	if len(errs) > 0 {
		return usageErr{errors.Join(errs...)}
	}
	return nil
}

// tagNone is the value of --kind, --github-owners and --max-grant that removes the tag.
const tagNone = "none"

// checkKind is --kind's rule: a kind the broker mints (policy.ValidKind), or none.
func checkKind(kind string) error {
	if kind == "" || kind == tagNone || policy.ValidKind(kind) {
		return nil
	}
	return usageErr{fmt.Errorf("--kind is %s or %s, not %q", strings.Join(policy.Kinds, ", "), tagNone, kind)}
}

// checkMaxGrant is --max-grant's rule, the broker's own (policy.ParseMaxGrant), or none where
// orNone allows it.
func checkMaxGrant(maxGrant string, orNone bool) error {
	if maxGrant == "" || (orNone && maxGrant == tagNone) {
		return nil
	}
	if _, err := policy.ParseMaxGrant(maxGrant); err != nil {
		return usageErr{fmt.Errorf("--max-grant: %w", err)}
	}
	return nil
}

// checkGitHubOwners is --github-owners' rule, the broker's own (policy.ParseGitHubOwners): GitHub
// account logins separated by spaces, which the shell quotes as one argument; a comma is refused.
// none is a sentinel, not a login: alone, and only where orNone allows it (retag), it removes the
// tag; given on create, or beside a login, or in another case, it is refused, since it would
// otherwise be written as a login or dropped as the removal (optionalTags), leaving a github-app
// secret the broker refuses. It answers the list as the broker reads it, lowercased, sorted and
// single-spaced, so the tag is written as it will be served.
func checkGitHubOwners(owners string, orNone bool) (string, error) {
	if owners == "" || (orNone && owners == tagNone) {
		return owners, nil
	}
	for _, field := range strings.Fields(owners) {
		if !strings.EqualFold(field, tagNone) {
			continue
		}
		if orNone {
			return "", usageErr{fmt.Errorf("--github-owners %q: none removes the tag and is given alone, as --github-owners none; a GitHub login list names no none", owners)}
		}
		return "", usageErr{fmt.Errorf("--github-owners %q: none names no GitHub account, and removes the tag only on retag; a github-app secret needs at least one login", owners)}
	}
	parsed, err := policy.ParseGitHubOwners(owners)
	if err != nil {
		return "", usageErr{fmt.Errorf("--github-owners: %w", err)}
	}
	return parsed, nil
}

// checkGitHubOwnersFor applies the broker's github-owners rule (policy.GitHubOwnersFor) to the
// kind and owners a secret will hold once a write lands, so create and retag refuse, before any
// write, a github-app secret the broker would then refuse as github-owners-missing or
// github-owners-malformed, each named as the broker names it. named says the command itself named
// --github-owners: owners named for a kind that does not read them are refused too, since the
// broker would ignore them, while owners the secret already holds are left alone whatever its kind
// becomes.
func checkGitHubOwnersFor(kind, owners string, named bool) error {
	if !policy.KindNeedsGitHubOwners(kind) {
		if named {
			return usageErr{fmt.Errorf("--github-owners is read only on a %s secret, and this one's kind is %q", policy.KindGitHubApp, kind)}
		}
		return nil
	}
	_, err := policy.GitHubOwnersFor(kind, owners)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, policy.ErrGitHubOwnersMissing):
		return usageErr{fmt.Errorf("a %s secret needs --github-owners naming at least one GitHub account, or the broker refuses it as %s", kind, policy.ReasonGitHubOwnersMissing)}
	default:
		return usageErr{fmt.Errorf("the secret's github-owners tag %q is malformed (%w), and the broker refuses it as %s: name the accounts with --github-owners", owners, err, policy.ReasonGitHubOwnersMalformed)}
	}
}

// optionalTags is the kind, github-owners and max-grant tags the flags set: each flag's value
// when it names one, and nothing for one left unset or set to none. A kind brings the rollback
// guard with it (policy.TagRollbackGuard), so a broker from before kinds refuses the secret rather
// than serve its stored key or refresh token.
func optionalTags(kind, githubOwners, maxGrant string) map[string]string {
	tags := map[string]string{}
	for key, value := range map[string]string{policy.TagKind: kind, policy.TagGitHubOwners: githubOwners, policy.TagMaxGrant: maxGrant} {
		if value != "" && value != tagNone {
			tags[key] = value
		}
	}
	if _, kinded := tags[policy.TagKind]; kinded {
		tags[policy.TagRollbackGuard] = policy.RollbackGuardValue
	}
	return tags
}

// ownerTag is the owner tag --owner names: the signed-in person's email for me, shared for shared.
func ownerTag(owner, email string) (string, error) {
	if owner == "me" {
		if !policy.ValidPersonOwner(email) {
			return "", usageErr{fmt.Errorf("your sign-in's email %q is not an email the broker accepts as an owner", email)}
		}
		return email, nil
	}
	return owner, nil
}
