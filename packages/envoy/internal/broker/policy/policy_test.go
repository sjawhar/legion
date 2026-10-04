package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const (
	owner  = "sami@example.com"
	other  = "ben@example.com"
	legion = "legion"
)

// TestEvaluateFollowsOwnerAndTierAlone is the policy's whole table: who asks (the owner's own
// session, another person's session, a pod, the owning service's session) against whose secret
// at which tier. A person's agent-tier secret is automatic only for that person's sessions and an
// approval request to them for everyone else; a human-tier secret always needs its owner, or
// anyone for a shared one; a shared agent-tier secret goes to everyone; a service's secret goes
// to that service alone.
func TestEvaluateFollowsOwnerAndTierAlone(t *testing.T) {
	store := secrets.NewLocal(
		policytest.Secret("PERSON_AGENT", owner, policy.TierAgent, "v"),
		policytest.Secret("PERSON_HUMAN", owner, policy.TierHuman, "v"),
		policytest.Secret("SHARED_AGENT", policy.OwnerShared, policy.TierAgent, "v"),
		policytest.Secret("SHARED_HUMAN", policy.OwnerShared, policy.TierHuman, "v"),
		policytest.Secret("SERVICE_AGENT", legion, policy.TierAgent, "v"),
	)
	set, err := policytest.Loader(store, legion).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	automatic := policy.Decision{Outcome: policy.Automatic}
	deny := policy.Decision{Outcome: policy.Deny}
	approval := func(approver string) policy.Decision {
		return policy.Decision{Outcome: policy.Approval, Approver: approver}
	}
	requesters := map[string]policy.Requester{
		"owner's session":          {Operator: owner},
		"owner's session, cased":   {Operator: " Sami@Example.COM "},
		"another person's session": {Operator: other},
		"pod":                      {},
		"the service's session":    {Service: legion},
		"another service's":        {Service: "other-service"},
	}
	table := map[string]map[string]policy.Decision{
		"PERSON_AGENT": {
			"owner's session": automatic, "owner's session, cased": automatic,
			"another person's session": approval(owner), "pod": approval(owner),
			"the service's session": approval(owner), "another service's": approval(owner),
		},
		"PERSON_HUMAN": {
			"owner's session": approval(owner), "owner's session, cased": approval(owner),
			"another person's session": approval(owner), "pod": approval(owner),
			"the service's session": approval(owner), "another service's": approval(owner),
		},
		"SHARED_AGENT": {
			"owner's session": automatic, "owner's session, cased": automatic,
			"another person's session": automatic, "pod": automatic,
			"the service's session": automatic, "another service's": automatic,
		},
		"SHARED_HUMAN": {
			"owner's session": approval(record.AnyoneApprover), "owner's session, cased": approval(record.AnyoneApprover),
			"another person's session": approval(record.AnyoneApprover), "pod": approval(record.AnyoneApprover),
			"the service's session": approval(record.AnyoneApprover), "another service's": approval(record.AnyoneApprover),
		},
		"SERVICE_AGENT": {
			"owner's session": deny, "owner's session, cased": deny,
			"another person's session": deny, "pod": deny,
			"the service's session": automatic, "another service's": deny,
		},
	}
	for name, row := range table {
		for who, want := range row {
			got, err := set.Evaluate(name, requesters[who])
			want.Source = secrets.LocalARN(policytest.ID(name))
			if err != nil || got != want {
				t.Errorf("Evaluate(%s, %s) = %+v, %v; want %+v", name, who, got, err, want)
			}
		}
	}
	if _, err := set.Evaluate("NOT_A_SECRET", requesters["owner's session"]); !errors.Is(err, policy.ErrUnknownSecret) {
		t.Errorf("Evaluate(NOT_A_SECRET) = %v, want ErrUnknownSecret", err)
	}
}

// TestEvaluateTreatsAWithheldNameAsHumanTier is the rule for a session whose operator withheld a
// secret from it by revoking a grant of it the session got without asking: the secret is human tier
// to that session, so it asks the owner, or anyone for a shared secret, and a service's own secret,
// which no person approves, is denied. Withholding one name changes no other name's answer.
func TestEvaluateTreatsAWithheldNameAsHumanTier(t *testing.T) {
	store := secrets.NewLocal(
		policytest.Secret("PERSON_AGENT", owner, policy.TierAgent, "v"),
		policytest.Secret("PERSON_HUMAN", owner, policy.TierHuman, "v"),
		policytest.Secret("SHARED_AGENT", policy.OwnerShared, policy.TierAgent, "v"),
		policytest.Secret("SHARED_HUMAN", policy.OwnerShared, policy.TierHuman, "v"),
		policytest.Secret("SERVICE_AGENT", legion, policy.TierAgent, "v"),
	)
	set, err := policytest.Loader(store, legion).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	approval := func(approver string) policy.Decision {
		return policy.Decision{Outcome: policy.Approval, Approver: approver}
	}
	withheld := func(r policy.Requester, name string) policy.Requester {
		r.Withheld = []string{name}
		return r
	}
	for _, c := range []struct {
		name      string
		requester policy.Requester
		want      policy.Decision
	}{
		{"PERSON_AGENT", withheld(policy.Requester{Operator: owner}, "PERSON_AGENT"), approval(owner)},
		{"PERSON_AGENT", withheld(policy.Requester{Operator: other}, "PERSON_AGENT"), approval(owner)},
		{"PERSON_HUMAN", withheld(policy.Requester{Operator: owner}, "PERSON_HUMAN"), approval(owner)},
		{"SHARED_AGENT", withheld(policy.Requester{Operator: owner}, "SHARED_AGENT"), approval(record.AnyoneApprover)},
		{"SHARED_AGENT", withheld(policy.Requester{}, "SHARED_AGENT"), approval(record.AnyoneApprover)},
		{"SHARED_HUMAN", withheld(policy.Requester{Operator: other}, "SHARED_HUMAN"), approval(record.AnyoneApprover)},
		{"SERVICE_AGENT", withheld(policy.Requester{Service: legion}, "SERVICE_AGENT"), policy.Decision{Outcome: policy.Deny}},
		{"SERVICE_AGENT", withheld(policy.Requester{Operator: owner}, "SERVICE_AGENT"), policy.Decision{Outcome: policy.Deny}},
		// Another name withheld leaves this one as the tags decide it.
		{"PERSON_AGENT", withheld(policy.Requester{Operator: owner}, "SHARED_AGENT"), policy.Decision{Outcome: policy.Automatic}},
		{"SHARED_AGENT", withheld(policy.Requester{}, "PERSON_AGENT"), policy.Decision{Outcome: policy.Automatic}},
		{"SERVICE_AGENT", withheld(policy.Requester{Service: legion}, "PERSON_AGENT"), policy.Decision{Outcome: policy.Automatic}},
	} {
		got, err := set.Evaluate(c.name, c.requester)
		c.want.Source = secrets.LocalARN(policytest.ID(c.name))
		if err != nil || got != c.want {
			t.Errorf("Evaluate(%s, %+v) = %+v, %v; want %+v", c.name, c.requester, got, err, c.want)
		}
	}
	if _, err := set.Evaluate("NOT_A_SECRET", withheld(policy.Requester{Operator: owner}, "NOT_A_SECRET")); !errors.Is(err, policy.ErrUnknownSecret) {
		t.Errorf("Evaluate(NOT_A_SECRET) = %v, want ErrUnknownSecret", err)
	}
}
