package requests

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

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
	defer rows.Close()
	var audits []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan grant.used audit: %v", err)
		}
		var narrowing map[string]any
		if raw != nil {
			if err := json.Unmarshal(raw, &narrowing); err != nil {
				t.Fatalf("decode grant.used github_app %s: %v", raw, err)
			}
		}
		audits = append(audits, narrowing)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read grant.used audit: %v", err)
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
		policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (string, error) {
			got = append(got, req.GitHubNarrowing)
			if req.GitHubNarrowing != nil {
				return "narrowed-token", nil
			}
			return "owner-wide-token", nil
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
		values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, step.narrowing)
		if err != nil || values["APP_KEY"] != step.want {
			t.Fatalf("release %d = %v, %v; want APP_KEY=%s", i, values, err, step.want)
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
// that holds one, the narrowing reaches the github-app minter alone, and a stored value is released
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
		policy.KindGoogleLogin: func(_ context.Context, req secrets.MintRequest) (string, error) {
			googleNarrowings = append(googleNarrowings, req.GitHubNarrowing)
			return "access-token", nil
		},
		policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (string, error) {
			githubNarrowings = append(githubNarrowings, req.GitHubNarrowing)
			return "narrowed-token", nil
		},
	}
	narrowing := &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}
	for _, names := range [][]string{{"PLAIN_KEY"}, {"LOGIN_KEY"}, {"LOGIN_KEY", "PLAIN_KEY"}} {
		request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", names...), "")
		if err != nil || request.GrantID == nil {
			t.Fatalf("Create(%v) = %+v, %v; want a grant", names, request, err)
		}
		if values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, narrowing); !errors.Is(err, ErrNarrowingNotApplicable) || values != nil {
			t.Fatalf("narrowed Values(%v) = %v, %v; want ErrNarrowingNotApplicable and no value", names, values, err)
		}
	}
	if len(googleNarrowings) != 0 {
		t.Fatalf("the Google login minter ran %d times for refused releases, want none", len(googleNarrowings))
	}

	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "APP_KEY", "LOGIN_KEY", "PLAIN_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(APP_KEY, LOGIN_KEY, PLAIN_KEY) = %+v, %v; want a grant", request, err)
	}
	values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID, narrowing)
	if err != nil || values["APP_KEY"] != "narrowed-token" || values["LOGIN_KEY"] != "access-token" || values["PLAIN_KEY"] != "plain-v1" {
		t.Fatalf("narrowed Values(APP_KEY, LOGIN_KEY, PLAIN_KEY) = %v, %v; want each value", values, err)
	}
	if !reflect.DeepEqual(githubNarrowings, []*secrets.GitHubNarrowing{narrowing}) || !reflect.DeepEqual(googleNarrowings, []*secrets.GitHubNarrowing{nil}) {
		t.Fatalf("narrowings: github-app minter %v, google-login minter %v; want the narrowing for github-app alone", githubNarrowings, googleNarrowings)
	}
}
