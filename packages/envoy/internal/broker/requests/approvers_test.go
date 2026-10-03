package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const (
	// mallory is any signed-in person; her machine's session asks for the secrets below.
	mallory = "mallory@example.com"
	// bob is a person who owns nothing until a secret is handed to him.
	bob = "bob@example.com"
)

// TestAnApprovalStopsWhenItsSecretChangesHands pins whose approval keeps a grant once a secret's
// tags change: a grant its requester's own operator approved while the secret was shared and
// human-tier, or that the secret's owner approved, releases nothing once the secret belongs to a
// person who did not approve it, its rotated value included, and reuse no longer hands it back:
// asking again is a new request to the new owner.
func TestAnApprovalStopsWhenItsSecretChangesHands(t *testing.T) {
	for name, c := range map[string]struct {
		secret, approver, value string
		handedOver              secrets.LocalSecret
	}{
		"a shared human-tier secret its requester approved, handed to a person": {
			secret: "SHARED_KEY", approver: mallory, value: "shared-v1",
			handedOver: policytest.Secret("SHARED_KEY", otherPerson, policy.TierHuman, "shared-v2-rotated"),
		},
		"a person's secret its owner approved, handed to another person": {
			secret: "ALICE_KEY", approver: otherPerson, value: "alice-v1",
			handedOver: policytest.Secret("ALICE_KEY", bob, policy.TierHuman, "alice-v2-rotated"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _, _ := newFixture(t)
			ctx := context.Background()
			enr, key := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
			req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", c.secret), "")
			if err != nil || req.RecordID == nil {
				t.Fatalf("Create = %+v, %v, want pending", req, err)
			}
			dec, err := m.ApplyDecision(ctx, *req.RecordID, true, c.approver)
			if err != nil || dec.GrantID == "" {
				t.Fatalf("ApplyDecision(%s) = %+v, %v, want granted", c.approver, dec, err)
			}
			if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values[c.secret] != c.value {
				t.Fatalf("Values before the handover = %v, %v, want %s=%s", values, err, c.secret, c.value)
			}

			retag(t, m, c.handedOver)
			if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values after %s became %s's = %v, %v; want ErrGrantNotLive", c.secret, c.handedOver.Tags[policy.TagOwner], values, err)
			}
			again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", c.secret), "")
			if err != nil || again.ID == req.ID || again.State != "pending" {
				t.Fatalf("Create(again) = %+v, %v; want a new pending request, not the grant handed back", again, err)
			}
			var approver string
			if err := m.Store.Pool.QueryRow(ctx, `select approver from credential_requests where id=$1`, *again.RecordID).Scan(&approver); err != nil || approver != c.handedOver.Tags[policy.TagOwner] {
				t.Fatalf("new request's approver = %q, %v; want the new owner %s", approver, err, c.handedOver.Tags[policy.TagOwner])
			}
		})
	}
}

// TestAPendingRequestIsDecidedByWhomTheTagsNameNow pins who decides a request whose secret changed
// hands while it waited. One waiting on anyone for a shared human-tier secret that is now alice's
// is refused to its requester's own operator and to any other person, approve and deny alike, and
// stays pending until alice, whose approval releases the rotated value. One waiting on alice for a
// secret that is now bob's is refused to both of them.
func TestAPendingRequestIsDecidedByWhomTheTagsNameNow(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	enr, key := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
	shared, err := m.Create(ctx, enr, signRequest(t, m, key, "need the shared one", "SHARED_KEY"), "")
	if err != nil || shared.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v, want pending", shared, err)
	}
	alices, err := m.Create(ctx, enr, signRequest(t, m, key, "need alice's", "ALICE_KEY"), "")
	if err != nil || alices.RecordID == nil {
		t.Fatalf("Create(ALICE_KEY) = %+v, %v, want pending", alices, err)
	}

	retag(t, m,
		policytest.Secret("SHARED_KEY", otherPerson, policy.TierHuman, "alice-owned-v2"),
		policytest.Secret("ALICE_KEY", bob, policy.TierAgent, "bob-owned-v2"))
	for _, c := range []struct {
		recordID string
		logins   []string
	}{
		{*shared.RecordID, []string{mallory, "carol@example.com"}},
		{*alices.RecordID, []string{otherPerson, bob}},
	} {
		for _, login := range c.logins {
			for _, approve := range []bool{true, false} {
				if dec, err := m.ApplyDecision(ctx, c.recordID, approve, login); !errors.Is(err, record.ErrNotApprover) {
					t.Fatalf("ApplyDecision(approve=%v, %s) after the handover = %+v, %v; want record.ErrNotApprover", approve, login, dec, err)
				}
			}
		}
	}
	for _, id := range []string{shared.ID, alices.ID} {
		if got, err := m.Get(ctx, id); err != nil || got.State != "pending" {
			t.Fatalf("Get(%s) after refused decisions = %+v, %v; want pending", id, got, err)
		}
	}

	dec, err := m.ApplyDecision(ctx, *shared.RecordID, true, otherPerson)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice, the new owner) = %+v, %v; want granted", dec, err)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["SHARED_KEY"] != "alice-owned-v2" {
		t.Fatalf("Values after alice approved = %v, %v; want SHARED_KEY=alice-owned-v2", values, err)
	}
}

// TestAnOwnersApprovalSurvivesLooseningToShared pins the control: once alice's agent-tier secret
// is shared and human-tier, whose approver is anyone, the grant alice approved keeps releasing its
// value and is handed back by reuse, and a request still waiting on alice is hers to decide.
func TestAnOwnersApprovalSurvivesLooseningToShared(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need alice's", "ALICE_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v, want pending", req, err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, otherPerson)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice) = %+v, %v, want granted", dec, err)
	}
	otherEnr, otherKey := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
	waiting, err := m.Create(ctx, otherEnr, signRequest(t, m, otherKey, "need alice's too", "ALICE_KEY"), "")
	if err != nil || waiting.RecordID == nil {
		t.Fatalf("Create(mallory) = %+v, %v, want pending", waiting, err)
	}

	retag(t, m, policytest.Secret("ALICE_KEY", policy.OwnerShared, policy.TierHuman, "alice-v1"))
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["ALICE_KEY"] != "alice-v1" {
		t.Fatalf("Values once shared = %v, %v; want ALICE_KEY still released", values, err)
	}
	if again, err := m.Create(ctx, enr, signRequest(t, m, key, "again", "ALICE_KEY"), ""); err != nil || again.ID != req.ID {
		t.Fatalf("Create(again) once shared = %+v, %v; want the live grant reused", again, err)
	}
	if dec, err := m.ApplyDecision(ctx, *waiting.RecordID, true, otherPerson); err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice) on the waiting request once shared = %+v, %v; want granted", dec, err)
	}
}

// TestAnApprovalGivenWhileItsSecretWasRefusedReleasesNothing pins that the time the broker refuses
// a secret (here an owner tag it reads as malformed) launders no approval. The current tags name no
// approver for a secret they leave out, so mallory may decide her own request waiting on anyone
// then; but the grant releases nothing while the secret is refused, nothing once the tag is
// corrected to alice, who never approved it, and reuse does not hand it back.
func TestAnApprovalGivenWhileItsSecretWasRefusedReleasesNothing(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	enr, key := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need the shared one", "SHARED_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v, want pending", req, err)
	}

	retag(t, m, policytest.Secret("SHARED_KEY", "Alice@Example.com", policy.TierHuman, "alice-v2"))
	if _, err := m.Policy.Get().Evaluate("SHARED_KEY", policy.Requester{Operator: mallory}); !errors.Is(err, policy.ErrUnknownSecret) {
		t.Fatalf("Evaluate(SHARED_KEY) with a cased owner tag = %v; want policy.ErrUnknownSecret", err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, mallory)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(mallory) while SHARED_KEY is refused = %+v, %v; want granted", dec, err)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values while SHARED_KEY is refused = %v, %v; want ErrGrantNotLive", values, err)
	}

	retag(t, m, policytest.Secret("SHARED_KEY", otherPerson, policy.TierHuman, "alice-v3"))
	if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values once SHARED_KEY is alice's = %v, %v; want ErrGrantNotLive", values, err)
	}
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", "SHARED_KEY"), "")
	if err != nil || again.State != "pending" || again.GrantID != nil {
		t.Fatalf("Create(again) once SHARED_KEY is alice's = %+v, %v; want a new pending request, not the grant handed back", again, err)
	}
}

// TestAnOwnersApprovalSurvivesHerRaisingTheTier pins that a decision records the login later
// checks compare: alice approves bob's session for her agent-tier ALICE_KEY with her login cased
// and padded, then raises the secret to human tier. She is still its approver, so the grant keeps
// releasing, its new value included.
func TestAnOwnersApprovalSurvivesHerRaisingTheTier(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	enr, key := newEnrollment(t, m.Store, "box", "box-bob-"+t.Name(), new(bob), nil)
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need alice's", "ALICE_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v, want pending", req, err)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, " Alice@Example.com ")
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice, cased and padded) = %+v, %v, want granted", dec, err)
	}

	retag(t, m, policytest.Secret("ALICE_KEY", otherPerson, policy.TierHuman, "alice-v2"))
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["ALICE_KEY"] != "alice-v2" {
		t.Fatalf("Values after alice raised the tier = %v, %v; want ALICE_KEY=alice-v2", values, err)
	}
}

// TestRequestsNeedingTwoApproversAreRefused pins that one request never bundles secrets two
// approvers decide: a shared human-tier secret's approver is anyone, so a bundle decided as one
// record would let anyone release the other secret. The owner's human-tier secret beside a shared
// human-tier one, and another person's session asking for the owner's agent-tier secret beside a
// shared human-tier one, are both refused ErrMixedApprovers, writing no request and no record.
func TestRequestsNeedingTwoApproversAreRefused(t *testing.T) {
	m, ownerEnr, ownerKey, _ := newFixture(t)
	ctx := context.Background()
	otherEnr, otherKey := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
	for _, c := range []struct {
		who        string
		enrollment string
		key        *ecdsa.PrivateKey
		names      []string
	}{
		{"the owner's session", ownerEnr, ownerKey, []string{"DEEL_API_KEY", "SHARED_KEY"}},
		{"another person's session", otherEnr, otherKey, []string{"AUTO_TOKEN", "SHARED_KEY"}},
	} {
		if req, err := m.Create(ctx, c.enrollment, signRequest(t, m, c.key, "both", c.names...), ""); !errors.Is(err, ErrMixedApprovers) {
			t.Errorf("%s asking for %v = %+v, %v; want ErrMixedApprovers", c.who, c.names, req, err)
		}
	}
	var written int
	if err := m.Store.Pool.QueryRow(ctx, `select (select count(*) from requests) + (select count(*) from credential_requests)`).Scan(&written); err != nil || written != 0 {
		t.Fatalf("requests and records written = %d, %v; want none", written, err)
	}
}

// TestReuseComparesWholeNames pins that a live grant is handed back only for exactly its names,
// never for names nobody decided: with a grant of AUTO_TOKEN and SHARED_TOKEN live, a request for
// the one name "AUTO_TOKEN,SHARED_TOKEN" is not that grant, and is refused as a name the policy
// does not serve.
func TestReuseComparesWholeNames(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	retag(t, m, policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"))
	ctx := context.Background()
	pair, err := m.Create(ctx, enr, signRequest(t, m, key, "both", "AUTO_TOKEN", "SHARED_TOKEN"), "")
	if err != nil || pair.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN, SHARED_TOKEN) = %+v, %v; want granted", pair, err)
	}
	if joined, err := m.Create(ctx, enr, signRequest(t, m, key, "joined", "AUTO_TOKEN,SHARED_TOKEN"), ""); !errors.Is(err, policy.ErrUnknownSecret) {
		t.Fatalf("Create(%q) = %+v, %v; want policy.ErrUnknownSecret, not the pair's grant", "AUTO_TOKEN,SHARED_TOKEN", joined, err)
	}
}
