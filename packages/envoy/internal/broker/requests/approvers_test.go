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

// TestAPendingRequestIsApprovedByWhomTheTagsNameNow pins who decides a request whose secret
// changed hands while it waited. Approving it follows the current tags: one waiting on anyone for
// a shared human-tier secret that is now alice's is refused to its requester's own operator and to
// any other person, and stays pending until alice, whose approval releases the rotated value; one
// waiting on alice for a secret that is now bob's is refused to both of them. Denying it follows
// the record, since a denial releases nothing: the approver a stranded request waits on can clear
// it (any person for one waiting on anyone, alice for one waiting on her), and bob, its secret's
// new owner but not that approver, cannot.
func TestAPendingRequestIsApprovedByWhomTheTagsNameNow(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	enr, key := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
	otherEnr, otherKey := newEnrollment(t, m.Store, "box", "box-mallory-other-"+t.Name(), new(mallory), nil)
	pending := func(enrollment string, key *ecdsa.PrivateKey, name string) Request {
		t.Helper()
		req, err := m.Create(ctx, enrollment, signRequest(t, m, key, "need "+name, name), "")
		if err != nil || req.RecordID == nil {
			t.Fatalf("Create(%s) = %+v, %v, want pending", name, req, err)
		}
		return req
	}
	shared := pending(enr, key, "SHARED_KEY")
	strandedShared := pending(otherEnr, otherKey, "SHARED_KEY")
	alices := pending(enr, key, "ALICE_KEY")

	retag(t, m,
		policytest.Secret("SHARED_KEY", otherPerson, policy.TierHuman, "alice-owned-v2"),
		policytest.Secret("ALICE_KEY", bob, policy.TierAgent, "bob-owned-v2"))
	for _, c := range []struct {
		req     Request
		approve bool
		login   string
	}{
		{shared, true, mallory},
		{shared, true, "carol@example.com"},
		{alices, true, otherPerson},
		{alices, true, bob},
		{alices, false, bob},
	} {
		if dec, err := m.ApplyDecision(ctx, *c.req.RecordID, c.approve, c.login); !errors.Is(err, record.ErrNotApprover) {
			t.Errorf("ApplyDecision(%s, approve=%v, %s) after the handover = %+v, %v; want record.ErrNotApprover", c.req.Secrets[0].Name, c.approve, c.login, dec, err)
		}
	}
	for _, req := range []Request{shared, strandedShared, alices} {
		if got, err := m.Get(ctx, req.ID); err != nil || got.State != "pending" {
			t.Fatalf("Get(%s) after refused decisions = %+v, %v; want pending", req.ID, got, err)
		}
	}

	dec, err := m.ApplyDecision(ctx, *shared.RecordID, true, otherPerson)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice, the new owner) = %+v, %v; want granted", dec, err)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["SHARED_KEY"] != "alice-owned-v2" {
		t.Fatalf("Values after alice approved = %v, %v; want SHARED_KEY=alice-owned-v2", values, err)
	}
	for _, c := range []struct {
		who   string
		req   Request
		login string
	}{
		{"carol, on a request waiting on anyone", strandedShared, "carol@example.com"},
		{"alice, on a request waiting on her", alices, otherPerson},
	} {
		if dec, err := m.ApplyDecision(ctx, *c.req.RecordID, false, c.login); err != nil || dec.State != "denied" {
			t.Errorf("deny by %s after the handover = %+v, %v; want denied", c.who, dec, err)
		}
	}
}

// TestADenialAfterAnOwnerChangeReleasesNothingAndClosesTheRecord pins what a denial of a stranded
// request leaves behind: alice denies the one waiting on her for a secret that is now bob's, and
// mallory, its requester, denies her own waiting on anyone for a secret that is now alice's. Neither
// releases anything, each record holds its one denied event and leaves every pending list, no one
// decides it again, and the session's next ask for each name is a new request to the secret's new
// owner rather than the denied one reused.
func TestADenialAfterAnOwnerChangeReleasesNothingAndClosesTheRecord(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	enr, key := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
	pending := func(name string) Request {
		t.Helper()
		req, err := m.Create(ctx, enr, signRequest(t, m, key, "need "+name, name), "")
		if err != nil || req.RecordID == nil {
			t.Fatalf("Create(%s) = %+v, %v, want pending", name, req, err)
		}
		return req
	}
	alices := pending("ALICE_KEY")
	shared := pending("SHARED_KEY")
	retag(t, m,
		policytest.Secret("ALICE_KEY", bob, policy.TierAgent, "bob-owned-v2"),
		policytest.Secret("SHARED_KEY", otherPerson, policy.TierHuman, "alice-owned-v2"))

	for _, c := range []struct {
		req   Request
		login string
	}{{alices, otherPerson}, {shared, mallory}} {
		dec, err := m.ApplyDecision(ctx, *c.req.RecordID, false, c.login)
		if err != nil || dec.State != "denied" || dec.GrantID != "" {
			t.Fatalf("deny %s by %s after the handover = %+v, %v; want denied with no grant", c.req.Secrets[0].Name, c.login, dec, err)
		}
	}
	people := []string{otherPerson, bob, mallory, "carol@example.com"}
	for _, req := range []Request{alices, shared} {
		var grants, events int
		if err := m.Store.Pool.QueryRow(ctx, `select count(*) from grants where request_id=$1`, req.ID).Scan(&grants); err != nil || grants != 0 {
			t.Errorf("grants of the denied %s request = %d, %v; want none", req.Secrets[0].Name, grants, err)
		}
		if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_request_events where record_id=$1`, *req.RecordID).Scan(&events); err != nil || events != 1 {
			t.Errorf("events on the denied %s record = %d, %v; want its one denied event", req.Secrets[0].Name, events, err)
		}
		for _, login := range people {
			for _, approve := range []bool{true, false} {
				if dec, err := m.ApplyDecision(ctx, *req.RecordID, approve, login); !errors.Is(err, record.ErrNotApprover) && !errors.Is(err, ErrTerminal) {
					t.Errorf("ApplyDecision(denied %s, approve=%v, %s) = %+v, %v; want NOT_APPROVER or RECORD_TERMINAL", req.Secrets[0].Name, approve, login, dec, err)
				}
			}
		}
		if err := m.Cancel(ctx, req.ID, enr); err == nil {
			t.Errorf("Cancel(denied %s) = nil; want a refusal", req.Secrets[0].Name)
		}
		if got, err := m.Get(ctx, req.ID); err != nil || got.State != "denied" {
			t.Errorf("Get(denied %s) = %+v, %v; want denied", req.Secrets[0].Name, got, err)
		}
	}
	for _, person := range people {
		listed, err := m.PendingForApprover(ctx, person)
		if err != nil {
			t.Fatalf("PendingForApprover(%s): %v", person, err)
		}
		for _, p := range listed {
			if p.RecordID == *alices.RecordID || p.RecordID == *shared.RecordID {
				t.Errorf("PendingForApprover(%s) lists the denied record %s", person, p.RecordID)
			}
		}
	}
	for name, owner := range map[string]string{"ALICE_KEY": bob, "SHARED_KEY": otherPerson} {
		again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", name), "")
		if err != nil || again.State != "pending" || again.RecordID == nil || again.Coalesced || again.GrantID != nil ||
			*again.RecordID == *alices.RecordID || *again.RecordID == *shared.RecordID {
			t.Fatalf("Create(%s) after the denial = %+v, %v; want a new pending request", name, again, err)
		}
		var approver string
		if err := m.Store.Pool.QueryRow(ctx, `select approver from credential_requests where id=$1`, *again.RecordID).Scan(&approver); err != nil || approver != owner {
			t.Errorf("new %s request's approver = %q, %v; want the new owner %s", name, approver, err, owner)
		}
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

// TestAnApprovalOfASecretNoLongerServedIsLeftToItsApprover pins that the current tags constrain an
// approval only for a secret they still serve: once ALICE_KEY leaves the namespace they name no
// approver for it, so alice, the approver the request waits on, may still approve it, and the
// grant releases nothing at its first read.
func TestAnApprovalOfASecretNoLongerServedIsLeftToItsApprover(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	enr, key := newEnrollment(t, m.Store, "box", "box-mallory-"+t.Name(), new(mallory), nil)
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need alice's", "ALICE_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v, want pending", req, err)
	}

	fixtureStore(m).Delete(policytest.ID("ALICE_KEY"))
	retag(t, m)
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, otherPerson)
	if err != nil || dec.State != "granted" {
		t.Fatalf("ApplyDecision(alice) once ALICE_KEY is gone = %+v, %v; want granted", dec, err)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values once ALICE_KEY is gone = %v, %v; want ErrGrantNotLive", values, err)
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
