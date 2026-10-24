package policy_test

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestAGitHubLoginWithATrailingHyphenIsServed: GitHub still serves accounts whose login an older
// sign-up rule let end in a hyphen: the organization x-, created in 2011, and the users y- and z-,
// each answered by api.github.com/users/<login>. A github-app secret listing one names GitHub
// logins, so it is served, and that account's installation gets its token. Today
// ParseGitHubOwners reads each owner against the current sign-up rule and refuses the secret
// github-owners-malformed, so none of its owners gets a token; agent-secrets secret create and
// retag refuse the same list, since they apply ParseGitHubOwners.
func TestAGitHubLoginWithATrailingHyphenIsServed(t *testing.T) {
	if got, err := policy.ParseGitHubOwners("Y- z- X-"); err != nil || got != "x- y- z-" {
		t.Fatalf("ParseGitHubOwners(%q) = %q, %v; want %q", "Y- z- X-", got, err, "x- y- z-")
	}
	secret := policytest.KindSecret("TRAILING_HYPHEN_APP", "ada@example.com", policy.TierAgent, policy.KindGitHubApp, "app-value")
	secret.Tags[policy.TagGitHubOwners] = "x- acme-org"
	set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	served, ok := set.Secrets["TRAILING_HYPHEN_APP"]
	if !ok || served.GitHubOwners != "acme-org x-" {
		t.Fatalf("TRAILING_HYPHEN_APP served = %v with GitHubOwners %q; want it served for %q", ok, served.GitHubOwners, "acme-org x-")
	}
}
