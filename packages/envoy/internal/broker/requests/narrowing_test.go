package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// usedAudits reads grantID's grant.used audit rows' github_app detail, oldest first: nil for a
// release that narrowed nothing.
func usedAudits(t *testing.T, machine *Machine, grantID string) []map[string]any {
	t.Helper()
	rows, err := machine.Store.Pool.Query(t.Context(), `select detail->'github_app' from audit where kind='grant.used' and grant_id=$1 order by id`, grantID)
	if err != nil {
		t.Fatalf("read grant.used audit: %v", err)
	}
	raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read grant.used audit: %v", err)
	}
	var audits []map[string]any
	for _, raw := range raws {
		var narrowing map[string]any
		if raw != nil {
			if err := json.Unmarshal(raw, &narrowing); err != nil {
				t.Fatalf("decode grant.used github_app %s: %v", raw, err)
			}
		}
		audits = append(audits, narrowing)
	}
	return audits
}

// TestANarrowedReleaseLeavesTheGrantOwnerWide: narrowing is a release's alone. The github-app
// minter gets the narrowing the release named, and the same grant's next release without one gets
// none and its owner-wide token, as the release before it did: the broker keeps no narrowed token
// and records no narrowing on the grant, so the narrowed token and the owner-wide one never stand
// in for each other. Each release's audit row records the narrowing it applied.
func TestANarrowedReleaseLeavesTheGrantOwnerWide(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""))
	var got []*secrets.GitHubNarrowing
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
			got = append(got, req.GitHubNarrowing)
			if req.GitHubNarrowing != nil {
				return secrets.Minted{Value: "narrowed-token"}, nil
			}
			return secrets.Minted{Value: "owner-wide-token"}, nil
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "APP_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant", request, err)
	}
	narrowing := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs"}, ReadOnly: true}

	for i, step := range []struct {
		narrowing *secrets.GitHubNarrowing
		want      string
	}{{nil, "owner-wide-token"}, {narrowing, "narrowed-token"}, {nil, "owner-wide-token"}} {
		release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, step.narrowing)
		if err != nil || release.Values["APP_KEY"] != step.want {
			t.Fatalf("release %d = %v, %v; want APP_KEY=%s", i, release.Values, err, step.want)
		}
	}
	if want := []*secrets.GitHubNarrowing{nil, narrowing, nil}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the minter's narrowings = %v, want %v: each release's own", got, want)
	}
	wantAudits := []map[string]any{nil, {"owner": "ada", "repositories": []any{"proofs"}, "read_only": true}, nil}
	if audits := usedAudits(t, machine, *request.GrantID); !reflect.DeepEqual(audits, wantAudits) {
		t.Fatalf("grant.used github_app details = %v, want %v", audits, wantAudits)
	}
}

// TestANarrowingOfAGrantWithNoGitHubAppSecretReleasesNothing: a release that asks to narrow a
// GitHub App token from a grant that holds none (a stored secret, another kind) is refused and
// releases nothing, rather than every value whole as though no narrowing were asked for. In a grant
// that holds one, the narrowing reaches its github-app minter, and every other value is released
// as it is.
func TestANarrowingOfAGrantWithNoGitHubAppSecretReleasesNothing(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine,
		requestKindSecret("LOGIN_KEY", policy.KindGoogleLogin, policy.TierAgent, ""),
		requestKindSecret("APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""),
		policytest.Secret("PLAIN_KEY", fixtureOperator, policy.TierAgent, "plain-v1"),
	)
	var googleNarrowings, githubNarrowings []*secrets.GitHubNarrowing
	machine.Minters = map[string]Minter{
		policy.KindGoogleLogin: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
			googleNarrowings = append(googleNarrowings, req.GitHubNarrowing)
			return secrets.Minted{Value: "access-token"}, nil
		},
		policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
			githubNarrowings = append(githubNarrowings, req.GitHubNarrowing)
			return secrets.Minted{Value: "narrowed-token"}, nil
		},
	}
	narrowing := &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}
	for _, names := range [][]string{{"PLAIN_KEY"}, {"LOGIN_KEY"}, {"LOGIN_KEY", "PLAIN_KEY"}} {
		request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", names...), "")
		if err != nil || request.GrantID == nil {
			t.Fatalf("Create(%v) = %+v, %v; want a grant", names, request, err)
		}
		if release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, narrowing); !errors.Is(err, ErrNarrowingNotApplicable) || release.Values != nil {
			t.Fatalf("narrowed Values(%v) = %v, %v; want ErrNarrowingNotApplicable and no value", names, release.Values, err)
		}
	}
	if len(googleNarrowings) != 0 {
		t.Fatalf("the Google login minter ran %d times for refused releases, want none", len(googleNarrowings))
	}

	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "APP_KEY", "LOGIN_KEY", "PLAIN_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(APP_KEY, LOGIN_KEY, PLAIN_KEY) = %+v, %v; want a grant", request, err)
	}
	release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, narrowing)
	if err != nil || release.Values["APP_KEY"] != "narrowed-token" || release.Values["LOGIN_KEY"] != "access-token" || release.Values["PLAIN_KEY"] != "plain-v1" {
		t.Fatalf("narrowed Values(APP_KEY, LOGIN_KEY, PLAIN_KEY) = %v, %v; want each value", release.Values, err)
	}
	if !reflect.DeepEqual(githubNarrowings, []*secrets.GitHubNarrowing{narrowing}) {
		t.Fatalf("the github-app minter's narrowings = %v, want %v", githubNarrowings, []*secrets.GitHubNarrowing{narrowing})
	}
}

// TestANarrowingToAnUnlistedOwnerReleasesNothing: a narrowing names an account every github-app
// secret of the grant lists in its github-owners tag, or the release is refused before any minter
// runs (ErrNarrowingOwnerNotListed), naming the secret and the account, and releases nothing, not
// even the grant's stored values: the caller's input, not the App, is at fault, so nothing reaches
// the minter whose failure the operator acts on. The same grant narrowed to a listed account
// releases, and so does its release with no narrowing.
func TestANarrowingToAnUnlistedOwnerReleasesNothing(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine,
		requestKindSecret("APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""),
		policytest.Secret("PLAIN_KEY", fixtureOperator, policy.TierAgent, "plain-v1"),
	)
	minted := 0
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
			minted++
			return secrets.Minted{Value: "token"}, nil
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "APP_KEY", "PLAIN_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant", request, err)
	}
	release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, &secrets.GitHubNarrowing{Owner: "outsider", ReadOnly: true})
	if !errors.Is(err, ErrNarrowingOwnerNotListed) || release.Values != nil || !strings.Contains(err.Error(), "APP_KEY") || !strings.Contains(err.Error(), "outsider") {
		t.Fatalf("Values narrowed to an unlisted owner = %v, %v; want ErrNarrowingOwnerNotListed naming APP_KEY and outsider, no value", release.Values, err)
	}
	if minted != 0 {
		t.Fatalf("the github-app minter ran %d times for a refused narrowing, want none", minted)
	}
	if audits := usedAudits(t, machine, *request.GrantID); len(audits) != 0 {
		t.Fatalf("a refused narrowing wrote grant.used rows %v, want none", audits)
	}
	for _, narrowing := range []*secrets.GitHubNarrowing{{Owner: "ada", ReadOnly: true}, nil} {
		if release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, narrowing); err != nil || release.Values["APP_KEY"] != "token" || release.Values["PLAIN_KEY"] != "plain-v1" {
			t.Fatalf("Values narrowed to %+v = %v, %v; want both values", narrowing, release.Values, err)
		}
	}
}

// TestANarrowingTheMinterRefusesIsNotAMintFailure: a minter's refusal of the narrowing itself
// (secrets.ErrMintNarrowingRefused: GitHub's 422 to a narrowed token) reaches the caller as that
// refusal and not as ErrMintFailed, the class whose ERROR line tells the operator to fix the App.
func TestANarrowingTheMinterRefusesIsNotAMintFailure(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""))
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
			return secrets.Minted{}, fmt.Errorf("%w: GitHub answered HTTP 422", secrets.ErrMintNarrowingRefused)
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "APP_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant", request, err)
	}
	release, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"missing"}})
	if !errors.Is(err, secrets.ErrMintNarrowingRefused) || errors.Is(err, ErrMintFailed) || release.Values != nil {
		t.Fatalf("Values = %v, %v; want ErrMintNarrowingRefused, not ErrMintFailed, and no value", release.Values, err)
	}
}

// TestANarrowingOfAGrantBehindTheAirlockReleasesNothing: the airlock holds a name's value back
// and the proxy spends it, reading it once per grant per pod with no narrowing of its own, for
// as long as the grant lives. A release that answered "narrowing applied" for such a grant would
// be telling the command that the token behind its placeholder is the narrowed one while the
// proxy swapped in an owner-wide token with every permission the App holds. So the release is
// refused instead, naming the name the airlock holds; nothing is minted and no placeholder is
// written. The same grant without a narrowing releases as it always did, and a grant the rules
// do not name narrows as it always did.
func TestANarrowingOfAGrantBehindTheAirlockReleasesNothing(t *testing.T) {
	machine, enrollmentID, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("APP_KEY", "")}})
	retag(t, machine,
		requestKindSecret("APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""),
		requestKindSecret("OPEN_APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""),
	)
	var narrowings []*secrets.GitHubNarrowing
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
			narrowings = append(narrowings, req.GitHubNarrowing)
			return secrets.Minted{Value: `{"installations":{"ada":{"installation_id":7,"token":"ghs_the_real_installation_token"}}}`}, nil
		},
	}
	narrowing := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs"}, ReadOnly: true}
	_, behind := grantOf(t, machine, enrollmentID, key, "read the App key", "APP_KEY", "AUTO_TOKEN")

	release, err := machine.Values(t.Context(), behind, enrollmentID, narrowing)
	if !errors.Is(err, ErrNarrowingBehindAirlock) || release.Values != nil || release.Airlock != nil {
		t.Fatalf("narrowed Values of a grant behind the airlock = %+v, %v; want ErrNarrowingBehindAirlock and no release", release, err)
	}
	if !strings.Contains(err.Error(), "APP_KEY") || !strings.Contains(err.Error(), "airlock") {
		t.Fatalf("the refusal says %q; want it to name APP_KEY and say the airlock holds it", err)
	}
	if len(narrowings) != 0 {
		t.Fatalf("the github-app minter ran %d times for a refused narrowing; want none", len(narrowings))
	}
	if rows := placeholderRows(t, machine, behind); len(rows) != 0 {
		t.Fatalf("the refused release minted the placeholders %v; want none", rows)
	}

	// The same grant releases as it always did when it asks for no narrowing.
	release, err = machine.Values(t.Context(), behind, enrollmentID, nil)
	if err != nil || release.Airlock == nil || release.Airlock.Names["APP_KEY"].Value == "" {
		t.Fatalf("Values of the same grant with no narrowing = %+v, %v; want the airlock's placeholders", release, err)
	}
	if !reflect.DeepEqual(narrowings, []*secrets.GitHubNarrowing{nil}) {
		t.Fatalf("the github-app minter's narrowings = %v; want one owner-wide mint", narrowings)
	}
	holdsNothingOf(t, release, "ghs_the_real_installation_token")

	// A grant no name of which the rules name narrows as it did before the airlock.
	_, open := grantOf(t, machine, enrollmentID, key, "read the other App key", "OPEN_APP_KEY")
	release, err = machine.Values(t.Context(), open, enrollmentID, narrowing)
	if err != nil || !strings.Contains(release.Values["OPEN_APP_KEY"], "ghs_the_real_installation_token") {
		t.Fatalf("narrowed Values of a grant the rules do not name = %v, %v; want the minted value", release.Values, err)
	}
	if !reflect.DeepEqual(narrowings, []*secrets.GitHubNarrowing{nil, narrowing}) {
		t.Fatalf("the github-app minter's narrowings = %v; want the owner-wide mint and then this release's narrowing", narrowings)
	}
}
