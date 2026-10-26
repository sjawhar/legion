package requests

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

func requestKindSecret(name, kind, tier, maxGrant string) secrets.LocalSecret {
	secret := policytest.KindSecret(name, fixtureOperator, tier, kind, "stored-value")
	if maxGrant != "" {
		secret.Tags[policy.TagMaxGrant] = maxGrant
	}
	return secret
}

// TestAMalformedMintedSourceIsNamedAndReleasesNothing: a frozen source that opens with mint: but
// names no kind or no store source, which no policy built, is refused as malformed rather than as a
// kind the broker has no minter for, and releases nothing.
func TestAMalformedMintedSourceIsNamedAndReleasesNothing(t *testing.T) {
	machine, _, _, _ := newFixture(t)
	for _, source := range []string{"mint:github-app", "mint::" + policytest.ID("X")} {
		value, err := machine.release(t.Context(), requestedSecret{name: "X", source: source}, nil, nil)
		if !errors.Is(err, policy.ErrKindSourceMalformed) || value != "" {
			t.Fatalf("release(%q) = %q, %v; want ErrKindSourceMalformed and no value", source, value, err)
		}
	}
}

func TestAgentKindSecretIsGrantedAtOnceAndMintsForEachRelease(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("GITHUB_APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""))
	calls := 0
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(context.Context, secrets.MintRequest) (string, error) {
			calls++
			return "minted-token-" + string(rune('0'+calls)), nil
		},
	}

	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "GITHUB_APP_KEY"), "")
	if err != nil || request.State != "granted" || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want automatic grant", request, err)
	}
	for want := 1; want <= 2; want++ {
		values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, nil)
		if err != nil || values["GITHUB_APP_KEY"] != "minted-token-"+string(rune('0'+want)) {
			t.Fatalf("Values call %d = %v, %v; want a fresh minted token", want, values, err)
		}
	}
	if calls != 2 {
		t.Fatalf("minter calls = %d, want 2", calls)
	}
}

// TestAGitHubAppReleaseMintsForTheOwnersThePolicyServesNow: the minter gets the github-owners the
// current policy serves for the name, not the list the request was decided under, so a list
// narrowed after a grant holds at the grant's next release.
func TestAGitHubAppReleaseMintsForTheOwnersThePolicyServesNow(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	secret := requestKindSecret("APP_KEY", policy.KindGitHubApp, policy.TierAgent, "")
	retag(t, machine, secret)
	var got []string
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (string, error) {
			got = req.GitHubOwners
			return "minted", nil
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "APP_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	if _, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, nil); err != nil {
		t.Fatalf("Values: %v", err)
	}
	if want := strings.Fields(policytest.FixtureGitHubOwners); !slices.Equal(got, want) {
		t.Fatalf("minter's GitHubOwners = %v, want %v", got, want)
	}
	narrowed := secret
	narrowed.Tags = maps.Clone(secret.Tags)
	narrowed.Tags[policy.TagGitHubOwners] = "ada"
	retag(t, machine, narrowed)
	if _, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, nil); err != nil {
		t.Fatalf("Values after narrowing github-owners: %v", err)
	}
	if !slices.Equal(got, []string{"ada"}) {
		t.Fatalf("minter's GitHubOwners after narrowing = %v, want [ada]", got)
	}
}

func TestHumanKindSecretWaitsForApprovalThenMints(t *testing.T) {
	machine, enrollmentID, key, approver := newFixture(t)
	retag(t, machine, requestKindSecret("GOOGLE_LOGIN_KEY", policy.KindGoogleLogin, policy.TierHuman, ""))
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) { return "access-token", nil },
	}

	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "GOOGLE_LOGIN_KEY"), "")
	if err != nil || request.State != "pending" || request.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want pending request", request, err)
	}
	decision, err := machine.ApplyDecision(t.Context(), *request.RecordID, true, approver)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want grant", decision, err)
	}
	values, _, err := machine.Values(t.Context(), decision.GrantID, enrollmentID, nil)
	if err != nil || values["GOOGLE_LOGIN_KEY"] != "access-token" {
		t.Fatalf("Values = %v, %v; want minted token", values, err)
	}
}

func TestMaxGrantRequiresATTLNoLongerThanTheTag(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("LIMITED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))

	for name, compact := range map[string]string{
		"no TTL":    signRequest(t, machine, key, "read limited login", "LIMITED_LOGIN"),
		"two hours": signRequestFor(t, machine, key, 2*60*60, "LIMITED_LOGIN"),
	} {
		request, err := machine.Create(t.Context(), enrollmentID, compact, "")
		if !errors.Is(err, ErrGrantRequiresLifetime) || !strings.Contains(err.Error(), "LIMITED_LOGIN") || !strings.Contains(err.Error(), "1h") {
			t.Fatalf("Create(%s) = %+v, %v; want ErrGrantRequiresLifetime naming the secret and cap", name, request, err)
		}
	}
	var requests int
	if err := machine.Store.Pool.QueryRow(t.Context(), `select count(*) from requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("invalid max-grant requests wrote %d rows, want 0", requests)
	}
}

func TestMaxGrantAcceptsALifetimeAtTheTag(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("LIMITED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))
	request, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 60*60, "LIMITED_LOGIN"), "")
	if err != nil || request.State != "granted" || request.GrantID == nil {
		t.Fatalf("Create(1h) = %+v, %v; want a grant", request, err)
	}
}

func TestPlainToKindRetagEndsTheGrant(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	plain := policytest.Secret("PLAIN_TO_KIND", fixtureOperator, policy.TierAgent, "stored-value")
	retag(t, machine, plain)
	calls := 0
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(context.Context, secrets.MintRequest) (string, error) { calls++; return "minted", nil },
	}
	first, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "PLAIN_TO_KIND"), "")
	if err != nil || first.GrantID == nil {
		t.Fatalf("Create = %+v, %v", first, err)
	}
	kind := plain
	kind.Tags = maps.Clone(plain.Tags)
	kind.Tags[policy.TagKind] = policy.KindGitHubApp
	kind.Tags[policy.TagGitHubOwners] = policytest.FixtureGitHubOwners
	retag(t, machine, kind)
	if values, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID, nil); !errors.Is(err, ErrGrantNotLive) || values != nil || calls != 0 {
		t.Fatalf("Values after plain-to-kind retag = %v, %v after %d mints; want ErrGrantNotLive", values, err, calls)
	}
	next, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read again", "PLAIN_TO_KIND"), "")
	if err != nil || next.ID == first.ID {
		t.Fatalf("second Create = %+v, %v; want a new request", next, err)
	}
}

func TestDeletedKindSecretStopsReleasing(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("DELETE_KIND_KEY", policy.KindGoogleLogin, policy.TierAgent, ""))
	calls := 0
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) {
			calls++
			return "access-token", nil
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "DELETE_KIND_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	fixtureStore(machine).Delete(policytest.ID("DELETE_KIND_KEY"))
	if values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, nil); !errors.Is(err, ErrSecretNotInStore) || values != nil || calls != 0 {
		t.Fatalf("Values after delete = %v, %v after %d minter calls; want ErrSecretNotInStore and no release", values, err, calls)
	}
}

func TestKindRetagEndsTheGrantAndPreventsReuse(t *testing.T) {
	for name, replacement := range map[string]func(secrets.LocalSecret) secrets.LocalSecret{
		"cleared": func(secret secrets.LocalSecret) secrets.LocalSecret {
			secret.Tags = maps.Clone(secret.Tags)
			delete(secret.Tags, policy.TagKind)
			return secret
		},
		"changed": func(secret secrets.LocalSecret) secrets.LocalSecret {
			secret.Tags = maps.Clone(secret.Tags)
			secret.Tags[policy.TagKind] = policy.KindGoogleLogin
			return secret
		},
	} {
		t.Run(name, func(t *testing.T) {
			machine, enrollmentID, key, _ := newFixture(t)
			secret := requestKindSecret("RETAG_KIND_KEY", policy.KindGitHubApp, policy.TierAgent, "")
			retag(t, machine, secret)
			calls := 0
			machine.Minters = map[string]Minter{
				policy.KindGitHubApp: func(context.Context, secrets.MintRequest) (string, error) {
					calls++
					return "minted-token", nil
				},
				policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) {
					calls++
					return "access-token", nil
				},
			}
			first, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "RETAG_KIND_KEY"), "")
			if err != nil || first.GrantID == nil {
				t.Fatalf("first Create = %+v, %v", first, err)
			}
			retag(t, machine, replacement(secret))
			if values, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID, nil); !errors.Is(err, ErrGrantNotLive) || values != nil || calls != 0 {
				t.Fatalf("old Values = %v, %v after %d minter calls; want ErrGrantNotLive and no stored value", values, err, calls)
			}
			next, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read again", "RETAG_KIND_KEY"), "")
			if err != nil || next.ID == first.ID {
				t.Fatalf("second Create = %+v, %v; want a new request", next, err)
			}
		})
	}
}

func TestKindSecretWithoutAMinterDoesNotReleaseItsStoredValue(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("NO_MINTER_KEY", policy.KindGitHubApp, policy.TierAgent, ""))
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "NO_MINTER_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	if values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, nil); values != nil || err == nil || !strings.Contains(err.Error(), "no token minter") {
		t.Fatalf("Values = %v, %v; want no-minter error and no stored value", values, err)
	}
}

// TestAMaxGrantAddedLaterBoundsLiveGrantsAndPendingApprovals: a max-grant tag set after a grant
// was made, or after a request started waiting, holds them to it as Create holds a new request:
// the grant that lasts until its session ends releases nothing more, and the waiting request is
// no one's to approve, so neither outlives the bound the owner has just set.
func TestAMaxGrantAddedLaterBoundsLiveGrantsAndPendingApprovals(t *testing.T) {
	machine, enrollmentID, key, approver := newFixture(t)
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) { return "access-token", nil },
	}
	retag(t, machine,
		requestKindSecret("AGENT_LOGIN", policy.KindGoogleLogin, policy.TierAgent, ""),
		requestKindSecret("HUMAN_LOGIN", policy.KindGoogleLogin, policy.TierHuman, ""),
	)
	granted, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "AGENT_LOGIN"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create(AGENT_LOGIN) = %+v, %v; want an automatic grant", granted, err)
	}
	pending, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 2*60*60, "HUMAN_LOGIN"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(HUMAN_LOGIN, 2h) = %+v, %v; want pending", pending, err)
	}

	retag(t, machine,
		requestKindSecret("AGENT_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"),
		requestKindSecret("HUMAN_LOGIN", policy.KindGoogleLogin, policy.TierHuman, "1h"),
	)
	if values, _, err := machine.Values(t.Context(), *granted.GrantID, enrollmentID, nil); !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, ErrGrantRequiresLifetime) || values != nil {
		t.Fatalf("Values after max-grant 1h = %v, %v; want ErrGrantNotLive naming the max grant", values, err)
	}
	if decision, err := machine.ApplyDecision(t.Context(), *pending.RecordID, true, approver); !errors.Is(err, record.ErrNotApprover) || !errors.Is(err, ErrGrantRequiresLifetime) {
		t.Fatalf("ApplyDecision(2h) after max-grant 1h = %+v, %v; want ErrNotApprover naming the max grant", decision, err)
	}
	if again, err := machine.Get(t.Context(), pending.ID); err != nil || again.State != "pending" {
		t.Fatalf("Get(pending) = %+v, %v; want still pending", again, err)
	}
}

// endedForGood reads grantID's revocation and its grant.revoked audit rows naming GrantEndedActor.
func endedForGood(t *testing.T, machine *Machine, grantID string) (revokedBy string, audits int) {
	t.Helper()
	var by *string
	if err := machine.Store.Pool.QueryRow(t.Context(), `select revoked_by from grants where id=$1`, grantID).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if err := machine.Store.Pool.QueryRow(t.Context(), `select count(*) from audit where kind='grant.revoked' and grant_id=$1 and actor=$2`, grantID, GrantEndedActor).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if by != nil {
		revokedBy = *by
	}
	return revokedBy, audits
}

// TestAKindChangedAwayAndBackLeavesTheGrantEnded: a kind retag after a grant ends that grant in
// the store at its next release, so setting the kind back, which serves the same source the grant
// froze, does not bring it back; the session asks again, and a new request makes a new grant.
func TestAKindChangedAwayAndBackLeavesTheGrantEnded(t *testing.T) {
	for name, away := range map[string]string{"removed": "", "changed": policy.KindGoogleLogin} {
		t.Run(name, func(t *testing.T) {
			machine, enrollmentID, key, _ := newFixture(t)
			secret := requestKindSecret("BACK_KIND_KEY", policy.KindGitHubApp, policy.TierAgent, "")
			retag(t, machine, secret)
			machine.Minters = map[string]Minter{
				policy.KindGitHubApp:   func(context.Context, secrets.MintRequest) (string, error) { return "minted-token", nil },
				policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) { return "access-token", nil },
			}
			first, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "BACK_KIND_KEY"), "")
			if err != nil || first.GrantID == nil {
				t.Fatalf("Create = %+v, %v", first, err)
			}
			moved := secret
			moved.Tags = maps.Clone(secret.Tags)
			if away == "" {
				delete(moved.Tags, policy.TagKind)
			} else {
				moved.Tags[policy.TagKind] = away
			}
			retag(t, machine, moved)
			if _, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID, nil); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values after the kind moved = %v; want ErrGrantNotLive", err)
			}
			retag(t, machine, secret)
			if values, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID, nil); !errors.Is(err, ErrGrantNotLive) || values != nil {
				t.Fatalf("Values once the kind is back = %v, %v; want ErrGrantNotLive: the grant stays ended", values, err)
			}
			if by, audits := endedForGood(t, machine, *first.GrantID); by != GrantEndedActor || audits != 1 {
				t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
			}
			next, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read again", "BACK_KIND_KEY"), "")
			if err != nil || next.ID == first.ID || next.GrantID == nil || *next.GrantID == *first.GrantID {
				t.Fatalf("Create after the kind is back = %+v, %v; want a new request and grant", next, err)
			}
			if values, _, err := machine.Values(t.Context(), *next.GrantID, enrollmentID, nil); err != nil || values["BACK_KIND_KEY"] != "minted-token" {
				t.Fatalf("Values of the new grant = %v, %v; want the minted token", values, err)
			}
		})
	}
}

// TestAMaxGrantLoweredBelowAGrantAndRaisedBackLeavesItEnded: a max-grant lowered below a live
// grant's lifetime ends that grant in the store at its next release, so raising it back does not
// bring the grant back, while a grant within the lowered bound keeps releasing.
func TestAMaxGrantLoweredBelowAGrantAndRaisedBackLeavesItEnded(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) { return "access-token", nil },
	}
	retag(t, machine, requestKindSecret("BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))
	long, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 60*60, "BOUNDED_LOGIN"), "")
	if err != nil || long.GrantID == nil {
		t.Fatalf("Create(1h) = %+v, %v", long, err)
	}
	short, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 20*60, "BOUNDED_LOGIN"), "")
	if err != nil || short.GrantID == nil {
		t.Fatalf("Create(20m) = %+v, %v", short, err)
	}
	retag(t, machine, requestKindSecret("BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "30m"))
	if _, _, err := machine.Values(t.Context(), *long.GrantID, enrollmentID, nil); !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, ErrGrantRequiresLifetime) {
		t.Fatalf("Values(1h) under max-grant 30m = %v; want ErrGrantNotLive naming the max grant", err)
	}
	if values, _, err := machine.Values(t.Context(), *short.GrantID, enrollmentID, nil); err != nil || values["BOUNDED_LOGIN"] != "access-token" {
		t.Fatalf("Values(20m) under max-grant 30m = %v, %v; want the token", values, err)
	}
	retag(t, machine, requestKindSecret("BOUNDED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, "1h"))
	if values, _, err := machine.Values(t.Context(), *long.GrantID, enrollmentID, nil); !errors.Is(err, ErrGrantNotLive) || values != nil {
		t.Fatalf("Values(1h) once max-grant is 1h again = %v, %v; want ErrGrantNotLive: the grant stays ended", values, err)
	}
	if by, audits := endedForGood(t, machine, *long.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
	}
	if by, _ := endedForGood(t, machine, *short.GrantID); by != "" {
		t.Fatalf("the 20m grant was revoked by %q; want it live", by)
	}
}

// TestAFailedMintIsNamedAndReleasesNothing: a minter's refusal, such as a Google login that needs
// a new sign-in, reaches the session as ErrMintFailed with the minter's own words, never as the
// stored value.
func TestAFailedMintIsNamedAndReleasesNothing(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("LAPSED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, ""))
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) {
			return "", errors.New("Google login needs a new sign-in: invalid_grant (invalid_rapt)")
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "LAPSED_LOGIN"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, nil)
	if !errors.Is(err, ErrMintFailed) || !strings.Contains(err.Error(), "invalid_rapt") || values != nil {
		t.Fatalf("Values = %v, %v; want ErrMintFailed naming invalid_rapt", values, err)
	}
	if strings.Contains(err.Error(), "stored-value") {
		t.Fatalf("Values error leaked the stored value: %q", err)
	}
}
