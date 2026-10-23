package requests

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

func requestKindSecret(name, kind, tier, maxGrant string) secrets.LocalSecret {
	secret := policytest.Secret(name, fixtureOperator, tier, "stored-value")
	secret.Tags[policy.TagKind] = kind
	if maxGrant != "" {
		secret.Tags[policy.TagMaxGrant] = maxGrant
	}
	return secret
}

type trackingFreshReader struct {
	reader secrets.Reader
	reads  int
	fresh  int
}

func (r *trackingFreshReader) Read(ctx context.Context, source string) (string, error) {
	r.reads++
	return r.reader.Read(ctx, source)
}

func (r *trackingFreshReader) ReadFresh(ctx context.Context, source string) (string, error) {
	r.fresh++
	return r.reader.Read(ctx, source)
}

func TestKindReleaseUsesFreshStoreRead(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("FRESH_KIND_KEY", policy.KindGoogleLogin, policy.TierAgent, ""))
	reader := &trackingFreshReader{reader: machine.Secrets}
	machine.Secrets = reader
	machine.Minters = map[string]func(context.Context, string) (string, error){
		policy.KindGoogleLogin: func(_ context.Context, value string) (string, error) { return "minted-" + value, nil },
	}

	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "FRESH_KIND_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID)
	if err != nil || values["FRESH_KIND_KEY"] != "minted-stored-value" {
		t.Fatalf("Values = %v, %v", values, err)
	}
	if reader.fresh != 1 || reader.reads != 0 {
		t.Fatalf("kind release made %d fresh and %d ordinary reads, want 1 and 0", reader.fresh, reader.reads)
	}
}

func TestAgentKindSecretIsGrantedAtOnceAndMintsForEachRelease(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("GITHUB_APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""))
	calls := 0
	machine.Minters = map[string]func(context.Context, string) (string, error){
		policy.KindGitHubApp: func(context.Context, string) (string, error) {
			calls++
			return "minted-token-" + string(rune('0'+calls)), nil
		},
	}

	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "GITHUB_APP_KEY"), "")
	if err != nil || request.State != "granted" || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want automatic grant", request, err)
	}
	for want := 1; want <= 2; want++ {
		values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID)
		if err != nil || values["GITHUB_APP_KEY"] != "minted-token-"+string(rune('0'+want)) {
			t.Fatalf("Values call %d = %v, %v; want a fresh minted token", want, values, err)
		}
	}
	if calls != 2 {
		t.Fatalf("minter calls = %d, want 2", calls)
	}
}

func TestHumanKindSecretWaitsForApprovalThenMints(t *testing.T) {
	machine, enrollmentID, key, approver := newFixture(t)
	retag(t, machine, requestKindSecret("GOOGLE_LOGIN_KEY", policy.KindGoogleLogin, policy.TierHuman, ""))
	machine.Minters = map[string]func(context.Context, string) (string, error){
		policy.KindGoogleLogin: func(context.Context, string) (string, error) { return "access-token", nil },
	}

	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "GOOGLE_LOGIN_KEY"), "")
	if err != nil || request.State != "pending" || request.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want pending request", request, err)
	}
	decision, err := machine.ApplyDecision(t.Context(), *request.RecordID, true, approver)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want grant", decision, err)
	}
	values, _, err := machine.Values(t.Context(), decision.GrantID, enrollmentID)
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
	machine.Minters = map[string]func(context.Context, string) (string, error){
		policy.KindGitHubApp: func(context.Context, string) (string, error) { calls++; return "minted", nil },
	}
	first, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "PLAIN_TO_KIND"), "")
	if err != nil || first.GrantID == nil {
		t.Fatalf("Create = %+v, %v", first, err)
	}
	kind := plain
	kind.Tags = maps.Clone(plain.Tags)
	kind.Tags[policy.TagKind] = policy.KindGitHubApp
	retag(t, machine, kind)
	if values, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID); !errors.Is(err, ErrGrantNotLive) || values != nil || calls != 0 {
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
	machine.Minters = map[string]func(context.Context, string) (string, error){
		policy.KindGoogleLogin: func(context.Context, string) (string, error) {
			calls++
			return "access-token", nil
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "DELETE_KIND_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	fixtureStore(machine).Delete(policytest.ID("DELETE_KIND_KEY"))
	if values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID); !errors.Is(err, ErrSecretNotInStore) || values != nil || calls != 0 {
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
			machine.Minters = map[string]func(context.Context, string) (string, error){
				policy.KindGitHubApp: func(context.Context, string) (string, error) {
					calls++
					return "minted-token", nil
				},
				policy.KindGoogleLogin: func(context.Context, string) (string, error) {
					calls++
					return "access-token", nil
				},
			}
			first, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "RETAG_KIND_KEY"), "")
			if err != nil || first.GrantID == nil {
				t.Fatalf("first Create = %+v, %v", first, err)
			}
			retag(t, machine, replacement(secret))
			if values, _, err := machine.Values(t.Context(), *first.GrantID, enrollmentID); !errors.Is(err, ErrGrantNotLive) || values != nil || calls != 0 {
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
	if values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID); values != nil || err == nil || !strings.Contains(err.Error(), "no token minter") {
		t.Fatalf("Values = %v, %v; want no-minter error and no stored value", values, err)
	}
}

// TestAMaxGrantAddedLaterBoundsLiveGrantsAndPendingApprovals: a max-grant tag set after a grant
// was made, or after a request started waiting, holds them to it as Create holds a new request:
// the grant that lasts until its session ends releases nothing more, and the waiting request is
// no one's to approve, so neither outlives the bound the owner has just set.
func TestAMaxGrantAddedLaterBoundsLiveGrantsAndPendingApprovals(t *testing.T) {
	machine, enrollmentID, key, approver := newFixture(t)
	machine.Minters = map[string]func(context.Context, string) (string, error){
		policy.KindGoogleLogin: func(context.Context, string) (string, error) { return "access-token", nil },
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
	if values, _, err := machine.Values(t.Context(), *granted.GrantID, enrollmentID); !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, ErrGrantRequiresLifetime) || values != nil {
		t.Fatalf("Values after max-grant 1h = %v, %v; want ErrGrantNotLive naming the max grant", values, err)
	}
	if decision, err := machine.ApplyDecision(t.Context(), *pending.RecordID, true, approver); !errors.Is(err, record.ErrNotApprover) || !errors.Is(err, ErrGrantRequiresLifetime) {
		t.Fatalf("ApplyDecision(2h) after max-grant 1h = %+v, %v; want ErrNotApprover naming the max grant", decision, err)
	}
	if again, err := machine.Get(t.Context(), pending.ID); err != nil || again.State != "pending" {
		t.Fatalf("Get(pending) = %+v, %v; want still pending", again, err)
	}
}

// TestAFailedMintIsNamedAndReleasesNothing: a minter's refusal, such as a Google login that needs
// a new sign-in, reaches the session as ErrMintFailed with the minter's own words, never as the
// stored value.
func TestAFailedMintIsNamedAndReleasesNothing(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("LAPSED_LOGIN", policy.KindGoogleLogin, policy.TierAgent, ""))
	machine.Minters = map[string]func(context.Context, string) (string, error){
		policy.KindGoogleLogin: func(context.Context, string) (string, error) {
			return "", errors.New("Google login needs a new sign-in: invalid_grant (invalid_rapt)")
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read login", "LAPSED_LOGIN"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID)
	if !errors.Is(err, ErrMintFailed) || !strings.Contains(err.Error(), "invalid_rapt") || values != nil {
		t.Fatalf("Values = %v, %v; want ErrMintFailed naming invalid_rapt", values, err)
	}
	if strings.Contains(err.Error(), "stored-value") {
		t.Fatalf("Values error leaked the stored value: %q", err)
	}
}
