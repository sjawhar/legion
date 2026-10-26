package requests

import (
	"context"
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// A grant ends for good when a release finds its secret's kind changed or its lifetime over the
// current max-grant (TestAKindChangedAwayAndBackLeavesTheGrantEnded and
// TestAMaxGrantLoweredBelowAGrantAndRaisedBackLeavesItEnded pin that, with a release between the
// change and its revert). A change reverted before any release finds it leaves the grant as it
// was: the grant was approved for that secret, kind and source, and on its next release it mints
// a token of that same kind from that same frozen source, never the stored value. To end a grant,
// revoke it. The tests below pin the revert with no release between.

// TestAKindChangedAwayAndBackWithNoReleaseBetweenLeavesTheGrantAsItWas: a kind retag reverted
// before any release leaves the grant live, and its next release goes through the minter of its
// original kind, returning that minter's token and never the stored value.
func TestAKindChangedAwayAndBackWithNoReleaseBetweenLeavesTheGrantAsItWas(t *testing.T) {
	for name, away := range map[string]string{"removed": "", "changed": policy.KindGoogleLogin} {
		t.Run(name, func(t *testing.T) {
			machine, enrollmentID, key, _ := newFixture(t)
			secret := requestKindSecret("QUIET_KIND_KEY", policy.KindGitHubApp, policy.TierAgent, "")
			retag(t, machine, secret)
			var githubCalls, googleCalls int
			var minted []string
			machine.Minters = map[string]Minter{
				policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (string, error) {
					githubCalls++
					minted = append(minted, req.Value)
					return "github-installation-token", nil
				},
				policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) {
					googleCalls++
					return "google-access-token", nil
				},
			}
			grant, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "QUIET_KIND_KEY"), "")
			if err != nil || grant.GrantID == nil {
				t.Fatalf("Create = %+v, %v", grant, err)
			}
			if values, _, err := machine.Values(t.Context(), *grant.GrantID, enrollmentID, nil); err != nil || values["QUIET_KIND_KEY"] != "github-installation-token" {
				t.Fatalf("Values before the retag = %v, %v; want the GitHub App minter's token", values, err)
			}
			moved := secret
			moved.Tags = maps.Clone(secret.Tags)
			if away == "" {
				delete(moved.Tags, policy.TagKind)
			} else {
				moved.Tags[policy.TagKind] = away
			}
			retag(t, machine, moved)
			retag(t, machine, secret)
			values, _, err := machine.Values(t.Context(), *grant.GrantID, enrollmentID, nil)
			if err != nil || values["QUIET_KIND_KEY"] != "github-installation-token" {
				t.Fatalf("Values once the kind is back, with no release while it was away = %v, %v; want the GitHub App minter's token: the grant is as it was", values, err)
			}
			if values["QUIET_KIND_KEY"] == "stored-value" {
				t.Fatal("the release returned the stored value")
			}
			if githubCalls != 2 || googleCalls != 0 {
				t.Fatalf("minter calls: github-app %d, google-login %d; want 2 and 0: the grant mints its original kind", githubCalls, googleCalls)
			}
			for _, value := range minted {
				if value != "stored-value" {
					t.Fatalf("the GitHub App minter was handed %q; want the secret's stored value, read from the grant's frozen source", value)
				}
			}
			if by, audits := endedForGood(t, machine, *grant.GrantID); by != "" || audits != 0 {
				t.Fatalf("grant revoked_by %q with %d grant.revoked rows; want it live", by, audits)
			}
		})
	}
}

// TestAMaxGrantLoweredAndRaisedWithNoReleaseBetweenLeavesTheGrantAsItWas: a max-grant lowered
// below a live grant's lifetime and raised back before any release leaves that grant live, and its
// next release mints its kind's token, never the stored value.
func TestAMaxGrantLoweredAndRaisedWithNoReleaseBetweenLeavesTheGrantAsItWas(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	var calls int
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(_ context.Context, req secrets.MintRequest) (string, error) {
			calls++
			if req.Value != "stored-value" {
				t.Errorf("the Google login minter was handed %q; want the secret's stored value", req.Value)
			}
			return "google-access-token", nil
		},
	}
	retag(t, machine, requestKindSecret("QUIET_BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))
	long, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 60*60, "QUIET_BOUNDED_LOGIN"), "")
	if err != nil || long.GrantID == nil {
		t.Fatalf("Create(1h) = %+v, %v", long, err)
	}
	retag(t, machine, requestKindSecret("QUIET_BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "30m"))
	retag(t, machine, requestKindSecret("QUIET_BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))
	values, _, err := machine.Values(t.Context(), *long.GrantID, enrollmentID, nil)
	if err != nil || values["QUIET_BOUNDED_LOGIN"] != "google-access-token" {
		t.Fatalf("Values(1h) once max-grant is 1h again, with no release while it was 30m = %v, %v; want the Google login minter's token: the grant is as it was", values, err)
	}
	if calls != 1 {
		t.Fatalf("minter calls = %d; want 1", calls)
	}
	if by, audits := endedForGood(t, machine, *long.GrantID); by != "" || audits != 0 {
		t.Fatalf("grant revoked_by %q with %d grant.revoked rows; want it live", by, audits)
	}
}
