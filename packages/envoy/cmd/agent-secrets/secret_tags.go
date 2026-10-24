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
// account logins separated by commas, or none where orNone allows it. It answers the list as the
// broker reads it, lowercased and sorted, so the tag is written as it will be served.
func checkGitHubOwners(owners string, orNone bool) (string, error) {
	if owners == "" || (orNone && owners == tagNone) {
		return owners, nil
	}
	parsed, err := policy.ParseGitHubOwners(owners)
	if err != nil {
		return "", usageErr{fmt.Errorf("--github-owners: %w", err)}
	}
	return parsed, nil
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
