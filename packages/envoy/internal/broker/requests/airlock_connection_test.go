package requests

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// twoNameRules are the entries the connection tests release behind: a stored key pair, whose two
// slots prove that any slot of a grant opens a connection, and a google-login, whose kind the
// connection answers.
func twoNameRules() airlock.Rules {
	return airlock.Rules{Secrets: []airlock.SecretRule{rule("AWS_KEY", "aws"), rule("LOGIN_KEY", "")}}
}

// twoNameGrant is a live grant of AWS_KEY and LOGIN_KEY, released once so its placeholders exist,
// with reason as its request's signed reason.
func twoNameGrant(t *testing.T, reason string) (m *Machine, enrollmentID, grantID string, release Release) {
	t.Helper()
	m, enrollmentID, key, _ := newPlaceholderFixture(t, twoNameRules())
	retag(t, m,
		policytest.Secret("AWS_KEY", fixtureOperator, policy.TierAgent, `{"access_key_id":"AKIAONE","secret_access_key":"real-secret"}`),
		policytest.KindSecret("LOGIN_KEY", fixtureOperator, policy.TierAgent, policy.KindGoogleLogin, `{"type":"authorized_user"}`),
	)
	m.Minters = map[string]Minter{policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
		return secrets.Minted{Value: "real-access-token"}, nil
	}}
	_, grantID = grantOf(t, m, enrollmentID, key, reason, "AWS_KEY", "LOGIN_KEY")
	release, err := m.Values(t.Context(), grantID, enrollmentID, nil)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	return m, enrollmentID, grantID, release
}

// TestAirlockConnectionAnswersEverySlotOfALiveGrant: a connection opened with any one of a live
// grant's placeholders proves the whole grant — its ids, its expiry, the issue its request named,
// and every airlock name with its slots and its kind — so the proxy can swap any of them.
func TestAirlockConnectionAnswersEverySlotOfALiveGrant(t *testing.T) {
	m, enr, grantID, release := twoNameGrant(t, "deploy for INFRA-1352 today")
	for slot, placeholder := range maps.Clone(release.Airlock.Names["AWS_KEY"].Slots) {
		conn, err := m.AirlockConnection(t.Context(), placeholder)
		if err != nil {
			t.Fatalf("AirlockConnection(the %s placeholder): %v", slot, err)
		}
		if conn.GrantID != grantID || conn.EnrollmentID != enr {
			t.Errorf("connection on the %s placeholder = grant %s of enrollment %s; want %s and %s", slot, conn.GrantID, conn.EnrollmentID, grantID, enr)
		}
		if conn.Issue != "INFRA-1352" {
			t.Errorf("connection issue = %q, want INFRA-1352, which the request's reason named", conn.Issue)
		}
		if conn.Expires == nil || release.Expires == nil || !conn.Expires.Equal(*release.Expires) {
			t.Errorf("connection expires %v, want the grant's own %v", conn.Expires, release.Expires)
		}
		if got := slices.Sorted(maps.Keys(conn.Names)); !slices.Equal(got, []string{"AWS_KEY", "LOGIN_KEY"}) {
			t.Fatalf("connection names = %v; want both of the grant's", got)
		}
		if got, want := conn.Names["AWS_KEY"].Slots, release.Airlock.Names["AWS_KEY"].Slots; !maps.Equal(got, want) {
			t.Errorf("AWS_KEY's slots = %v, want the released %v", got, want)
		}
		if got := conn.Names["AWS_KEY"].Kind; got != "" {
			t.Errorf("AWS_KEY's kind = %q, want empty: it is a stored secret", got)
		}
		if got := conn.Names["LOGIN_KEY"].Kind; got != policy.KindGoogleLogin {
			t.Errorf("LOGIN_KEY's kind = %q, want %s", got, policy.KindGoogleLogin)
		}
	}
}

// TestAirlockConnectionRefusesWhatOpensNothing: a placeholder no row holds and a placeholder
// whose grant has ended are both ErrGrantNotLive, which the proxy answers 407. A revoked grant's
// own string is the case that matters: it is in a running command's environment.
func TestAirlockConnectionRefusesWhatOpensNothing(t *testing.T) {
	m, enr, grantID, release := twoNameGrant(t, "no issue here")
	if conn, err := m.AirlockConnection(t.Context(), PlaceholderPrefix+"neverMintedByThisBrokerAtAll00"); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockConnection(an unknown placeholder) = %+v, %v; want ErrGrantNotLive", conn, err)
	}
	if conn, err := m.AirlockConnection(t.Context(), ""); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockConnection(no placeholder) = %+v, %v; want ErrGrantNotLive", conn, err)
	}
	placeholder := release.Airlock.ProxyPassword
	if err := m.RevokeGrant(t.Context(), grantID, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if conn, err := m.AirlockConnection(t.Context(), placeholder); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockConnection(a revoked grant's placeholder) = %+v, %v; want ErrGrantNotLive", conn, err)
	}
}

// TestAirlockConnectionDropsAWithheldName: a name its operator withheld from the session is not
// on the connection, so the proxy carries nothing for it, and a connection left with no name at
// all is refused with ErrAirlockNoNames rather than opened empty. The rows are written directly:
// what the connection reads is withheld_secrets, whatever wrote it.
func TestAirlockConnectionDropsAWithheldName(t *testing.T) {
	m, enr, _, release := twoNameGrant(t, "read both")
	placeholder := release.Airlock.Names["AWS_KEY"].Slots[SlotAWSAccessKeyID]
	withhold := func(name string) {
		t.Helper()
		if _, err := m.Store.Pool.Exec(t.Context(), `insert into withheld_secrets (enrollment_id, name) values ($1,$2)`, enr, name); err != nil {
			t.Fatal(err)
		}
	}

	withhold("LOGIN_KEY")
	conn, err := m.AirlockConnection(t.Context(), placeholder)
	if err != nil {
		t.Fatalf("AirlockConnection with LOGIN_KEY withheld: %v", err)
	}
	if got := slices.Sorted(maps.Keys(conn.Names)); !slices.Equal(got, []string{"AWS_KEY"}) {
		t.Fatalf("connection names = %v; want AWS_KEY alone, with the withheld LOGIN_KEY dropped", got)
	}

	withhold("AWS_KEY")
	if conn, err := m.AirlockConnection(t.Context(), placeholder); !errors.Is(err, ErrAirlockNoNames) {
		t.Fatalf("AirlockConnection with every name withheld = %+v, %v; want ErrAirlockNoNames", conn, err)
	}
}

// TestAirlockValuesAnswersTheRealValuesOfTheAirlocksNames: what the proxy swaps in, and the
// policy version the checks ran under, which the proxy holds beside them. The names the
// rules do not hold back are no part of it, since the session already holds them.
func TestAirlockValuesAnswersTheRealValuesOfTheAirlocksNames(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AUTO_TOKEN", "")}})
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	_, grantID := grantOf(t, m, enr, key, "read both", "AUTO_TOKEN", "SECOND_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, nil); err != nil {
		t.Fatalf("Values: %v", err)
	}

	values, version, err := m.AirlockValues(t.Context(), grantID)
	if err != nil {
		t.Fatalf("AirlockValues: %v", err)
	}
	if want := m.PolicyVersion(); version != want {
		t.Fatalf("AirlockValues read under policy version %q, want the live %q", version, want)
	}
	if len(values) != 1 {
		t.Fatalf("AirlockValues = %v, want the airlock's name alone: the session already holds the rest", values)
	}
	held := values["AUTO_TOKEN"]
	if want := map[string]string{SlotValue: "auto-v1"}; !maps.Equal(held.Slots, want) {
		t.Fatalf("AUTO_TOKEN's slots = %v, want %v: a stored secret is its value slot alone", held.Slots, want)
	}
	if !held.Expires.IsZero() || held.Kind != "" {
		t.Fatalf("AUTO_TOKEN = %+v; want no expiry and no kind: a stored value is read again only when its name is", held)
	}
}

// TestAirlockValuesEndsAGrantWhoseAirlockEntryChanged: the proxy's own read makes the full
// release checks, so a grant Values would end is ended here too rather than going on being
// proxied by the pod that holds the connection.
func TestAirlockValuesEndsAGrantWhoseAirlockEntryChanged(t *testing.T) {
	m, enr, key, digests := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AUTO_TOKEN", "")}})
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, nil); err != nil {
		t.Fatalf("Values: %v", err)
	}

	digests["AUTO_TOKEN"] = "the entry moved"
	reloadAirlock(t, m)
	if values, version, err := m.AirlockValues(t.Context(), grantID); !errors.Is(err, ErrGrantNotLive) || values != nil || version != "" {
		t.Fatalf("AirlockValues after the entry changed = %v, %q, %v; want ErrGrantNotLive and no value", values, version, err)
	}
	if by, audits := endedForGood(t, m, grantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1, as Values ends it", by, audits, GrantEndedActor)
	}
	if _, err := m.AirlockConnection(t.Context(), placeholderRows(t, m, grantID)["AUTO_TOKEN/"+SlotValue]); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockConnection after the grant ended = %v; want ErrGrantNotLive", err)
	}
}

// TestTheSweeperDeletesThePlaceholdersOfGrantsThatHaveEnded: the rows of a live grant stay and
// the rows of a revoked one go, so the table does not grow with every grant the broker ever made.
func TestTheSweeperDeletesThePlaceholdersOfGrantsThatHaveEnded(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AUTO_TOKEN", "")}})
	_, ended := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	if _, err := m.Values(t.Context(), ended, enr, nil); err != nil {
		t.Fatalf("Values: %v", err)
	}
	if deleted, err := m.deleteSpentPlaceholders(t.Context()); err != nil || deleted != 0 {
		t.Fatalf("the sweep deleted %d rows of a live grant, %v; want none", deleted, err)
	}
	if err := m.RevokeGrant(t.Context(), ended, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if deleted, err := m.deleteSpentPlaceholders(t.Context()); err != nil || deleted != 1 {
		t.Fatalf("the sweep deleted %d rows of the revoked grant, %v; want its one row", deleted, err)
	}
	if rows := placeholderRows(t, m, ended); len(rows) != 0 {
		t.Fatalf("the revoked grant still holds %v", rows)
	}
}

// TestIssueKeyInReason: the proxy's log line names the issue the session said it was working on,
// which is the first Dispatch key the request's own signed reason holds.
func TestIssueKeyInReason(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		{"", ""},
		{"deploying the broker", ""},
		{"INFRA-1352", "INFRA-1352"},
		{"work on INFRA-1352 today", "INFRA-1352"},
		{"INFRA-1352 then LEGION-587", "INFRA-1352"},
		{"blocked by LEGION-587, see INFRA-1352", "LEGION-587"},
		{"(INFRA-1352) the airlock", "INFRA-1352"},
		{"dispatch://INFRA-1352/ask/3", "INFRA-1352"},
		{"infra-1352 lower-cased is no key", ""},
		{"INFRA- is no key, nor is -1352", ""},
		{"an UPPERCASE word and 1352 apart", ""},
	} {
		if got := issueKeyInReason(c.reason); got != c.want {
			t.Errorf("issueKeyInReason(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}
