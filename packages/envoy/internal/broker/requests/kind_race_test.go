package requests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// retagDuringRead is a secrets.Reader that runs during once, after Values has checked its policy
// and before the store read it wraps answers, then reads the store as it stands after during.
type retagDuringRead struct {
	secrets.Reader
	once   *sync.Once
	during func(ctx context.Context) error
	err    *error
}

func (r retagDuringRead) Read(ctx context.Context, source string) (string, error) {
	r.once.Do(func() { *r.err = r.during(ctx) })
	return r.Reader.Read(ctx, source)
}

// TestAPlainGrantRacingAKindRetagNeverReleasesWhatTheKindStores: a grant made while its secret was
// plain releases the secret's stored value. When a retag to a kind is reread while a release is
// reading the store, and the kind's long-lived credential is stored once the broker serves the
// kind, that release returns nothing the store holds: its values are discarded and the grant ends
// for good, as a kind change found before the read ends it. Each route to a grant is held to it: a
// grant made at once, the same grant a later request reuses, and one a person approved.
func TestAPlainGrantRacingAKindRetagNeverReleasesWhatTheKindStores(t *testing.T) {
	const canary = "refresh-token-canary-9f3c"
	for _, route := range []string{"automatic", "reused", "approved"} {
		t.Run(route, func(t *testing.T) {
			machine, enrollmentID, key, approver := newFixture(t)
			tier := policy.TierAgent
			if route == "approved" {
				tier = policy.TierHuman
			}
			retag(t, machine, policytest.Secret("RACED_LOGIN", fixtureOperator, tier, "ordinary-value"))
			minted := 0
			machine.Minters = map[string]Minter{
				policy.KindGoogleLogin: func(context.Context, secrets.MintRequest) (string, error) { minted++; return "access-token", nil },
			}
			request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "RACED_LOGIN"), "")
			if err != nil {
				t.Fatalf("Create = %v", err)
			}
			var grantID string
			switch route {
			case "automatic":
				if request.GrantID == nil {
					t.Fatalf("Create = %+v; want an automatic grant", request)
				}
				grantID = *request.GrantID
			case "reused":
				reused, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read again", "RACED_LOGIN"), "")
				if err != nil || reused.GrantID == nil || request.GrantID == nil || *reused.GrantID != *request.GrantID {
					t.Fatalf("second Create = %+v, %v; want the first request's grant reused", reused, err)
				}
				grantID = *reused.GrantID
			case "approved":
				if request.RecordID == nil {
					t.Fatalf("Create = %+v; want a pending request", request)
				}
				decision, err := machine.ApplyDecision(t.Context(), *request.RecordID, true, approver)
				if err != nil || decision.GrantID == "" {
					t.Fatalf("ApplyDecision = %+v, %v", decision, err)
				}
				grantID = decision.GrantID
			}
			store := fixtureStore(machine)
			var duringErr error
			machine.Secrets = retagDuringRead{Reader: machine.Secrets, once: &sync.Once{}, err: &duringErr, during: func(ctx context.Context) error {
				store.Put(policytest.KindSecret("RACED_LOGIN", fixtureOperator, tier, policy.KindGoogleLogin, "ordinary-value"))
				if err := machine.Policy.Refresh(ctx); err != nil {
					return err
				}
				store.Put(policytest.KindSecret("RACED_LOGIN", fixtureOperator, tier, policy.KindGoogleLogin, canary))
				return nil
			}}
			values, _, err := machine.Values(t.Context(), grantID, enrollmentID)
			if duringErr != nil {
				t.Fatalf("the retag during the read: %v", duringErr)
			}
			if _, served := machine.Policy.Get().Secrets["RACED_LOGIN"]; !served {
				t.Fatal("the retag during the read left RACED_LOGIN unserved; the race never ran")
			}
			for _, value := range values {
				if strings.Contains(value, canary) {
					t.Fatal("Values released the credential the secret stores once it is a kind")
				}
			}
			if err != nil && strings.Contains(err.Error(), canary) {
				t.Fatal("Values' error holds the credential the secret stores once it is a kind")
			}
			if !errors.Is(err, ErrGrantNotLive) || values != nil || minted != 0 {
				t.Fatalf("Values across a retag to a kind = %d values, %v after %d mints; want ErrGrantNotLive and none", len(values), err, minted)
			}
			if by, audits := endedForGood(t, machine, grantID); by != GrantEndedActor || audits != 1 {
				t.Fatalf("grant revoked_by %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
			}
		})
	}
}
