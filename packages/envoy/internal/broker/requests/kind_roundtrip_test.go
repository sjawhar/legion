package requests

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestAKindChangedAwayAndBackWithNoReleaseBetweenLeavesTheGrantEnded: a kind retag after a grant
// ends that grant once the broker's policy serves the name from another source, whether or not the
// grant is released while the kind differs. An owner who retags by mistake and at once retags back
// sets the kind back before any release, so the policy serves the source the grant froze again,
// and the grant still does not release: the session asks again, and a new request makes a new
// grant. Today only a release that finds the source moved ends the grant, so with no release
// between the two retags the grant releases again.
func TestAKindChangedAwayAndBackWithNoReleaseBetweenLeavesTheGrantEnded(t *testing.T) {
	for name, away := range map[string]string{"removed": "", "changed": policy.KindGoogleLogin} {
		t.Run(name, func(t *testing.T) {
			machine, enrollmentID, key, _ := newFixture(t)
			secret := requestKindSecret("QUIET_KIND_KEY", policy.KindGitHubApp, policy.TierAgent, "")
			retag(t, machine, secret)
			machine.Minters = map[string]Minter{
				policy.KindGitHubApp:   func(context.Context, secrets.MintRequest) (string, error) { return "minted-token", nil },
				policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) { return "access-token", nil },
			}
			first, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "QUIET_KIND_KEY"), "")
			if err != nil || first.GrantID == nil {
				t.Fatalf("Create = %+v, %v", first, err)
			}
			if values, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID); err != nil || values["QUIET_KIND_KEY"] != "minted-token" {
				t.Fatalf("Values before the retag = %v, %v; want the minted token", values, err)
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
			if values, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID); !errors.Is(err, ErrGrantNotLive) || values != nil {
				t.Fatalf("Values once the kind is back, with no release while it was away = %v, %v; want ErrGrantNotLive: the policy served the name from another source, so the grant stays ended", values, err)
			}
			next, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read again", "QUIET_KIND_KEY"), "")
			if err != nil || next.ID == first.ID || next.GrantID == nil || *next.GrantID == *first.GrantID {
				t.Fatalf("Create after the kind is back = %+v, %v; want a new request and grant", next, err)
			}
			if values, _, err := machine.Values(t.Context(), *next.GrantID, enrollmentID); err != nil || values["QUIET_KIND_KEY"] != "minted-token" {
				t.Fatalf("Values of the new grant = %v, %v; want the minted token", values, err)
			}
		})
	}
}

// TestAMaxGrantLoweredAndRaisedWithNoReleaseBetweenLeavesTheGrantEnded: a max-grant lowered below
// a live grant's lifetime ends that grant once the broker's policy holds the lower bound, whether
// or not the grant is released while it holds. Raised back before any release, the bound admits
// the grant's lifetime again, and the grant still does not release, while a grant within the
// lowered bound keeps releasing. Today only a release under the lower bound ends the grant, so with
// no release between lowering and raising it the longer grant releases again.
func TestAMaxGrantLoweredAndRaisedWithNoReleaseBetweenLeavesTheGrantEnded(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) { return "access-token", nil },
	}
	retag(t, machine, requestKindSecret("QUIET_BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))
	long, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 60*60, "QUIET_BOUNDED_LOGIN"), "")
	if err != nil || long.GrantID == nil {
		t.Fatalf("Create(1h) = %+v, %v", long, err)
	}
	short, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 20*60, "QUIET_BOUNDED_LOGIN"), "")
	if err != nil || short.GrantID == nil {
		t.Fatalf("Create(20m) = %+v, %v", short, err)
	}
	retag(t, machine, requestKindSecret("QUIET_BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "30m"))
	retag(t, machine, requestKindSecret("QUIET_BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))
	if values, _, err := machine.Values(t.Context(), *long.GrantID, enrollmentID); !errors.Is(err, ErrGrantNotLive) || values != nil {
		t.Fatalf("Values(1h) once max-grant is 1h again, with no release while it was 30m = %v, %v; want ErrGrantNotLive: the policy bounded the name below the grant's lifetime, so the grant stays ended", values, err)
	}
	if values, _, err := machine.Values(t.Context(), *short.GrantID, enrollmentID); err != nil || values["QUIET_BOUNDED_LOGIN"] != "access-token" {
		t.Fatalf("Values(20m) = %v, %v; want the token: a grant within the lowered bound keeps releasing", values, err)
	}
	next, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 60*60, "QUIET_BOUNDED_LOGIN"), "")
	if err != nil || next.GrantID == nil || *next.GrantID == *long.GrantID {
		t.Fatalf("Create(1h) once max-grant is 1h again = %+v, %v; want a new grant", next, err)
	}
}
