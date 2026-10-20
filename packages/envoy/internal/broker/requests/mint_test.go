package requests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const mintTarget = policytest.MintTarget

// mintCall is one call the fake minter answered.
type mintCall struct {
	sa   string
	life time.Duration
}

// withMintedSecret adds ADMIN_GCP_TOKEN, the operator's human-tier minted secret, to m's policy and
// gives m a fake minter recording each call it answers.
func withMintedSecret(t *testing.T, m *Machine) *[]mintCall {
	t.Helper()
	s := policytest.Secret("ADMIN_GCP_TOKEN", fixtureOperator, policy.TierHuman, "")
	s.Tags[policy.TagMint] = mintTarget
	retag(t, m, s)
	calls := &[]mintCall{}
	m.MintToken = func(_ context.Context, sa string, life time.Duration) (string, error) {
		*calls = append(*calls, mintCall{sa, life})
		return "minted-token", nil
	}
	return calls
}

// TestMintRequestRequiresLifetimeAtMostAnHour pins the lifetime cap: a request naming a minted
// secret with no lifetime, or one past an hour, is refused before anything is written, and one of
// an hour is pending for its owner.
func TestMintRequestRequiresLifetimeAtMostAnHour(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	withMintedSecret(t, m)
	ctx := context.Background()
	for name, compact := range map[string]string{
		"no lifetime": signRequest(t, m, key, "admin", "ADMIN_GCP_TOKEN"),
		"3601 s":      signRequestFor(t, m, key, 3601, "ADMIN_GCP_TOKEN"),
	} {
		if req, err := m.Create(ctx, enr, compact, ""); !errors.Is(err, ErrMintRequiresLifetime) || !strings.Contains(err.Error(), "ADMIN_GCP_TOKEN") {
			t.Fatalf("Create(%s) = %+v, %v; want ErrMintRequiresLifetime naming the secret", name, req, err)
		}
	}
	var written int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from requests where enrollment_id=$1`, enr).Scan(&written); err != nil || written != 0 {
		t.Fatalf("requests written by refused creates = %d, %v; want 0", written, err)
	}
	req, err := m.Create(ctx, enr, signRequestFor(t, m, key, 3600, "ADMIN_GCP_TOKEN"), "")
	if err != nil || req.State != "pending" || req.Approver == nil || *req.Approver != fixtureOperator {
		t.Fatalf("Create(3600 s) = %+v, %v; want pending for %s", req, err, fixtureOperator)
	}
}

// TestValuesMintsForAGcpTokenSource pins the release: an approved grant of a minted secret mints
// a token for the source's service account, lasting the shorter of an hour and what is left of the
// grant, and never reads the secret store for it.
func TestValuesMintsForAGcpTokenSource(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	calls := withMintedSecret(t, m)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequestFor(t, m, key, 3600, "ADMIN_GCP_TOKEN"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v", req, err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v", dec, err)
	}
	// The fixture's store holds ADMIN_GCP_TOKEN with no value: a read of it would fail.
	if _, err := m.Secrets.Read(ctx, secrets.LocalARN(policytest.ID("ADMIN_GCP_TOKEN"))); err == nil {
		t.Fatal("the fixture's minted secret has a value to read")
	}

	age(t, m, dec.GrantID, 30*time.Minute)
	values, _, err := m.Values(ctx, dec.GrantID, enr)
	if err != nil || values["ADMIN_GCP_TOKEN"] != "minted-token" {
		t.Fatalf("Values (30 m left) = %v, %v; want the minted token", values, err)
	}
	if len(*calls) != 1 || (*calls)[0].sa != mintTarget || (*calls)[0].life > 30*time.Minute || (*calls)[0].life < 29*time.Minute {
		t.Fatalf("mint calls = %+v; want one for %s lasting about 30 m", *calls, mintTarget)
	}

	// A grant longer than an hour (a shape a broker before the cap could leave) mints an hour.
	if _, err := m.Store.Pool.Exec(ctx, `update grants set expires_at = now() + interval '2 hours' where id=$1`, dec.GrantID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Values(ctx, dec.GrantID, enr); err != nil {
		t.Fatalf("Values (2 h left): %v", err)
	}
	if len(*calls) != 2 || (*calls)[1].life != MaxMintLifetime {
		t.Fatalf("mint calls = %+v; want the second lasting %s", *calls, MaxMintLifetime)
	}
}

// TestValuesWithoutMinterFails pins a minted secret's release with no minter wired: it fails, with
// an error naming the missing minter, rather than releasing nothing.
func TestValuesWithoutMinterFails(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	withMintedSecret(t, m)
	m.MintToken = nil
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequestFor(t, m, key, 600, "ADMIN_GCP_TOKEN"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v", req, err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v", dec, err)
	}
	values, _, err := m.Values(ctx, dec.GrantID, enr)
	if err == nil || !strings.Contains(err.Error(), "no token minter") || values != nil {
		t.Fatalf("Values with no minter = %v, %v; want an error naming the store's missing minter", values, err)
	}
}

// TestValuesStopsMintingOnceTheSecretIsGone pins the release's own reread: a live grant of a
// minted secret mints nothing once the secret is retagged, before any policy reload (the policy
// package's hold test covers a delete).
func TestValuesStopsMintingOnceTheSecretIsGone(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	calls := withMintedSecret(t, m)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequestFor(t, m, key, 3600, "ADMIN_GCP_TOKEN"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v", req, err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v", dec, err)
	}
	retargeted := policytest.Secret("ADMIN_GCP_TOKEN", fixtureOperator, policy.TierHuman, "")
	retargeted.Tags[policy.TagMint] = "other-admin@example-project.iam.gserviceaccount.com"
	fixtureStore(m).Put(retargeted) // no policy reload
	if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) || values != nil || len(*calls) != 0 {
		t.Fatalf("Values after a retag = %v, %v after %d mints; want ErrGrantNotLive and no mint", values, err, len(*calls))
	}
}
