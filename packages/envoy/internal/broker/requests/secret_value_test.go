package requests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
)

// TestASecretWithNoValueIsRefusedBeforeAnyoneApprovesIt pins that a secret created and tagged but
// never given a value is refused at Create as a name the policy does not serve, writing no request
// and no record, so no one is asked to approve a grant that would release nothing; and that once
// its value is put, the policy's next reload serves it: the request waits on its owner, and the
// approved grant releases the value.
func TestASecretWithNoValueIsRefusedBeforeAnyoneApprovesIt(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	retag(t, m, policytest.Secret("UNSEEDED_KEY", fixtureOperator, policy.TierHuman, ""))
	if req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "UNSEEDED_KEY"), ""); !errors.Is(err, policy.ErrUnknownSecret) {
		t.Fatalf("Create(UNSEEDED_KEY) while it has no value = %+v, %v; want policy.ErrUnknownSecret", req, err)
	}
	var written int
	if err := m.Store.Pool.QueryRow(ctx, `select (select count(*) from requests) + (select count(*) from credential_requests)`).Scan(&written); err != nil || written != 0 {
		t.Fatalf("requests and records written = %d, %v; want none", written, err)
	}

	retag(t, m, policytest.Secret("UNSEEDED_KEY", fixtureOperator, policy.TierHuman, "unseeded-v1"))
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it now", "UNSEEDED_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(UNSEEDED_KEY) once it has a value = %+v, %v; want pending", req, err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", approver, dec, err)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["UNSEEDED_KEY"] != "unseeded-v1" {
		t.Fatalf("Values = %v, %v; want UNSEEDED_KEY=unseeded-v1", values, err)
	}
}

// TestAGrantStopsWhileItsSecretHasNoValueAndResumesOnceItHasOne pins what a live grant does while
// one of its secrets has no current value: the policy leaves that secret out, so the whole grant
// releases nothing, refused ErrGrantNotLive naming the secret, rather than failing on the missing
// value at read; and once the value is back and the policy reloads, the same grant releases again,
// whether the namespace is otherwise as it was when the grant was decided (the policy version is
// the decided one again) or changed meanwhile (stillAllowed re-checks every name under the new
// version).
func TestAGrantStopsWhileItsSecretHasNoValueAndResumesOnceItHasOne(t *testing.T) {
	for name, c := range map[string]struct {
		meanwhile      func(*testing.T, *Machine)
		decidedVersion bool
	}{
		"the namespace otherwise unchanged": {func(*testing.T, *Machine) {}, true},
		"the namespace changed meanwhile":   {retagUnrelated, false},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, approver := newFixture(t)
			ctx := context.Background()
			retag(t, m, policytest.Secret("SECOND_KEY", fixtureOperator, policy.TierHuman, "second-v1"))
			req, err := m.Create(ctx, enr, signRequest(t, m, key, "need both", "DEEL_API_KEY", "SECOND_KEY"), "")
			if err != nil || req.RecordID == nil {
				t.Fatalf("Create = %+v, %v; want pending", req, err)
			}
			dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
			if err != nil || dec.GrantID == "" {
				t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", approver, dec, err)
			}
			decided := m.Policy.Get().Version
			if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["DEEL_API_KEY"] != "deel-v1" || values["SECOND_KEY"] != "second-v1" {
				t.Fatalf("Values while both have values = %v, %v; want both released", values, err)
			}

			retag(t, m, policytest.Secret("SECOND_KEY", fixtureOperator, policy.TierHuman, ""))
			if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) || !strings.Contains(err.Error(), "SECOND_KEY") {
				t.Fatalf("Values while SECOND_KEY has no value = %v, %v; want ErrGrantNotLive naming SECOND_KEY", values, err)
			}

			c.meanwhile(t, m)
			retag(t, m, policytest.Secret("SECOND_KEY", fixtureOperator, policy.TierHuman, "second-v2"))
			if got := m.Policy.Get().Version == decided; got != c.decidedVersion {
				t.Fatalf("policy version is the decided one: %v, want %v", got, c.decidedVersion)
			}
			if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["DEEL_API_KEY"] != "deel-v1" || values["SECOND_KEY"] != "second-v2" {
				t.Fatalf("Values once SECOND_KEY has a value again = %v, %v; want DEEL_API_KEY=deel-v1 and SECOND_KEY=second-v2", values, err)
			}
		})
	}
}
