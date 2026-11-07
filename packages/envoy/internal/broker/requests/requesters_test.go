// requesters_test.go drives a secret whose requesters tag names who may ask for it through the
// request machine: what a refused session is told, what becomes of a live grant the list drops
// the session from — by a retag, or by an account mapping that stops proving the pod's service
// — what an approval of such a request records, even when another of its names would be refused
// first, what a secret's own max-grant bounds its grants to, and what the list decides on its
// own for a kind whose rules make a service's pod wait for the secret's one person owner.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// listedValue is the value every listed secret below holds, so a release can be told apart from
// an empty answer.
const listedValue = "listed-v1"

// anHour is the lifetime most requests below sign; two sign a multiple of it to ask past an hour's
// cap. policytest.ListedSecret gives each secret this file lists a max-grant of an hour, and a
// request signing longer than a secret's own max-grant is refused before the policy decides it at
// all (overMaxGrant), which is one of this file's cases rather than its premise. The hour is that
// fixture's choice and no rule of the list's: policy.TagRules demands a max-grant of an admin
// secret and of a google-service-account one alone, so a secret that merely lists its requesters
// keeps whatever cap it sets, or none at all (TestUncappedListedLoginAwaitsOwner).
const anHour = 3600

// listedSecret is policytest.ListedSecret holding the one value this file releases: a namespace
// secret whose requesters tag names requesters, carrying that fixture's max-grant of an hour so
// that the requests below sign for exactly as long as the secret allows. A test of a secret the
// list leaves uncapped, or of a kind whose own rules demand a cap, builds its secret itself.
func listedSecret(name, owner, tier, requesters string) secrets.LocalSecret {
	return policytest.ListedSecret(name, owner, tier, requesters, listedValue)
}

// createdAudit reads request requestID's one request.created audit row: the state it recorded and
// the person and service it named, each empty when the row names neither.
func createdAudit(t *testing.T, m *Machine, requestID string) (state, person, service string) {
	t.Helper()
	if err := m.Store.Pool.QueryRow(context.Background(), `select detail->>'state', coalesce(detail->>'person',''), coalesce(detail->>'service','')
		from audit where kind='request.created' and request_id=$1`, requestID).Scan(&state, &person, &service); err != nil {
		t.Fatalf("read the request.created audit row of %s: %v", requestID, err)
	}
	return state, person, service
}

// TestRequestersARefusalNamesTheSecretAndTheRule: a session the list does not name is denied, and
// the request it gets back and the row the broker wrote both say which secret refused it and why,
// instead of only that something did. The request.created audit row names who asked — the person
// for an operator's session, the verified service for a service's pod — which is what a later
// read of the trail answers "who asked for this?" from.
func TestRequestersARefusalNamesTheSecretAndTheRule(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newFixture(t)
	retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, otherPerson))
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())

	for _, who := range []struct {
		name, enrollment, person, service string
		key                               *ecdsa.PrivateKey
	}{
		{name: "the operator's session", enrollment: enr, person: fixtureOperator, key: key},
		{name: "a service's pod", enrollment: pod, service: fixtureService, key: podKey},
	} {
		t.Run(who.name, func(t *testing.T) {
			const want = "LISTED_KEY: " + policy.ReasonRequesterNotListed
			req, err := m.Create(ctx, who.enrollment, signRequestFor(t, m, who.key, anHour, "LISTED_KEY"), "")
			if err != nil || req.State != "denied" || req.GrantID != nil {
				t.Fatalf("Create(%s, LISTED_KEY) = %+v, %v; want it denied with no grant", who.name, req, err)
			}
			if req.Detail == nil || *req.Detail != want {
				t.Fatalf("Create's own detail = %v, want %q", req.Detail, want)
			}
			got, err := m.Get(ctx, req.ID)
			if err != nil || got.Detail == nil || *got.Detail != want {
				t.Fatalf("Get = %+v, %v; want the row's decision_detail to read %q", got, err, want)
			}
			state, person, service := createdAudit(t, m, req.ID)
			if state != "denied" || person != who.person || service != who.service {
				t.Fatalf("request.created audit = state %q person %q service %q; want denied by person %q, service %q",
					state, person, service, who.person, who.service)
			}
		})
	}
}

// TestRequestersAGrantEndsForGoodWhenTheListDropsThePerson: a live grant of a listed secret stops
// releasing the moment the list drops the person its session is signed in as, ends with a
// grant.revoked audit row by the broker, and stays ended when the list names them again — they
// ask for a new grant instead. The unchanged case is the control: a retag that still names them
// moves the policy version, so the same re-check runs at the same point and releases the value,
// which is what makes the end above the list's doing and not the retag's.
func TestRequestersAGrantEndsForGoodWhenTheListDropsThePerson(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop bool
	}{
		{name: "the list drops the person", drop: true},
		{name: "the list still names the person", drop: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, enr, key, _ := newFixture(t)
			retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, fixtureOperator))
			granted, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
			if err != nil || granted.State != "granted" || granted.GrantID == nil {
				t.Fatalf("Create(LISTED_KEY) = %+v, %v; want it granted at once to the person it lists", granted, err)
			}
			if values, _, err := m.Values(ctx, *granted.GrantID, enr, nil); err != nil || values["LISTED_KEY"] != listedValue {
				t.Fatalf("Values before the retag = %v, %v; want the value", values, err)
			}

			listed := fixtureOperator + " " + otherPerson
			if tc.drop {
				listed = otherPerson
			}
			retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, listed))
			values, _, err := m.Values(ctx, *granted.GrantID, enr, nil)
			if !tc.drop {
				if err != nil || values["LISTED_KEY"] != listedValue {
					t.Fatalf("Values after a retag that still lists the person = %v, %v; want the value", values, err)
				}
				if by, audits := endedForGood(t, m, *granted.GrantID); by != "" || audits != 0 {
					t.Fatalf("grant revoked_by %q with %d grant.revoked rows; want it live", by, audits)
				}
				return
			}
			if !errors.Is(err, ErrGrantNotLive) || values != nil {
				t.Fatalf("Values after the list dropped the person = %v, %v; want ErrGrantNotLive", values, err)
			}
			if by, audits := endedForGood(t, m, *granted.GrantID); by != GrantEndedActor || audits != 1 {
				t.Fatalf("grant revoked_by %q with %d grant.revoked rows by %s; want it ended once", by, audits, GrantEndedActor)
			}

			retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, fixtureOperator))
			if _, _, err := m.Values(ctx, *granted.GrantID, enr, nil); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values once the person is listed again = %v; want the grant still ended", err)
			}
			if by, audits := endedForGood(t, m, *granted.GrantID); by != GrantEndedActor || audits != 1 {
				t.Fatalf("after the person is listed again: revoked_by %q with %d grant.revoked rows; want the one end", by, audits)
			}
			again, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
			if err != nil || again.GrantID == nil || *again.GrantID == *granted.GrantID {
				t.Fatalf("Create once the person is listed again = %+v, %v; want a grant of its own", again, err)
			}
		})
	}
}

// TestRequestersTheNextRequestEndsTheGrantTheListDropped drives the other re-check. A session that
// asks again for names it already holds never reaches Values: Create's reuse of the live grant is
// where the list is read (reuseLiveGrant). That grant must end there too, rather than be passed
// over and left live for the next release, and the session gets back the denied request that says
// why.
func TestRequestersTheNextRequestEndsTheGrantTheListDropped(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newFixture(t)
	retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, fixtureOperator))
	granted, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create(LISTED_KEY) = %+v, %v; want a grant", granted, err)
	}
	reused, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
	if err != nil || reused.ID != granted.ID {
		t.Fatalf("a second request while the list names the person = %+v, %v; want the live grant of %s reused", reused, err, granted.ID)
	}

	retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, otherPerson))
	const want = "LISTED_KEY: " + policy.ReasonRequesterNotListed
	denied, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
	if err != nil || denied.State != "denied" || denied.ID == granted.ID || denied.GrantID != nil {
		t.Fatalf("Create after the list dropped the person = %+v, %v; want a denied request of its own", denied, err)
	}
	if denied.Detail == nil || *denied.Detail != want {
		t.Fatalf("the denied request's detail = %v, want %q", denied.Detail, want)
	}
	if by, audits := endedForGood(t, m, *granted.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("the reused grant's revoked_by = %q with %d grant.revoked rows by %s; want the reuse to have ended it once", by, audits, GrantEndedActor)
	}
	if _, _, err := m.Values(ctx, *granted.GrantID, enr, nil); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values of the grant the reuse ended = %v; want ErrGrantNotLive", err)
	}
}

// TestRequestersAnApprovalAfterTheDropIsRecordedAsADenial: a card waiting on its owner when the
// list drops the requesting session is not left pending for a person who can do nothing with it.
// The approval is recorded as the request's denial by the broker, naming the secret and the rule,
// and mints no grant, while the event keeps the login of whoever pressed approve. With the list
// unchanged the same approval grants, which is what makes the denial the list's doing.
func TestRequestersAnApprovalAfterTheDropIsRecordedAsADenial(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop bool
	}{
		{name: "the list drops the person", drop: true},
		{name: "the list is unchanged", drop: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, enr, key, approver := newFixture(t)
			retag(t, m, listedSecret("LISTED_HUMAN_KEY", fixtureOperator, policy.TierHuman, fixtureOperator))
			req, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_HUMAN_KEY"), "")
			if err != nil || req.State != "pending" || req.RecordID == nil {
				t.Fatalf("Create(LISTED_HUMAN_KEY) = %+v, %v; want it pending on its owner", req, err)
			}
			if tc.drop {
				retag(t, m, listedSecret("LISTED_HUMAN_KEY", fixtureOperator, policy.TierHuman, otherPerson))
			}
			decision, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
			if err != nil {
				t.Fatalf("ApplyDecision(approve) = %+v, %v", decision, err)
			}
			if !tc.drop {
				if decision.State != "granted" || decision.GrantID == "" || decision.Detail != "" {
					t.Fatalf("ApplyDecision with the list unchanged = %+v; want it granted with no detail", decision)
				}
				if values, _, err := m.Values(ctx, decision.GrantID, enr, nil); err != nil || values["LISTED_HUMAN_KEY"] != listedValue {
					t.Fatalf("Values of the approved grant = %v, %v; want the value", values, err)
				}
				return
			}

			const want = "LISTED_HUMAN_KEY: " + policy.ReasonRequesterNotListed
			if decision.State != "denied" || decision.GrantID != "" || decision.Detail != want {
				t.Fatalf("ApplyDecision after the drop = %+v; want it denied with no grant and the detail %q", decision, want)
			}
			var state, decidedBy, detail string
			var grants int
			if err := m.Store.Pool.QueryRow(ctx, `select r.state, coalesce(r.decided_by,''), coalesce(r.decision_detail,''),
				(select count(*) from grants g where g.request_id=r.id) from requests r where r.id=$1`, req.ID).
				Scan(&state, &decidedBy, &detail, &grants); err != nil {
				t.Fatalf("read the request row: %v", err)
			}
			if state != "denied" || decidedBy != PolicyDeniedActor || detail != want || grants != 0 {
				t.Fatalf("the request row = state %q decided_by %q detail %q with %d grants; want it denied by %s, %q, and no grant",
					state, decidedBy, detail, grants, PolicyDeniedActor, want)
			}
			var event, login, actor, eventDetail string
			if err := m.Store.Pool.QueryRow(ctx, `select event, coalesce(login,''), actor, coalesce(detail,'')
				from credential_request_events where record_id=$1`, *req.RecordID).Scan(&event, &login, &actor, &eventDetail); err != nil {
				t.Fatalf("read the record's event: %v", err)
			}
			if event != "denied" || login != approver || actor != PolicyDeniedActor || eventDetail != want {
				t.Fatalf("the record's event = %s by login %q, actor %q, detail %q; want denied, login %s, actor %s, detail %q",
					event, login, actor, eventDetail, approver, PolicyDeniedActor, want)
			}
		})
	}
}

// TestRequestersAListedSecretGrantsForNoLongerThanItsOwnMaxGrant: the hour this listed secret's
// own max-grant tag bounds its grants to is enforced where every other max-grant is, before the
// policy decides the request — a two hour request is refused and names the bound, and the hour
// one is granted for the hour. The bound is the secret's own tag and nothing its requesters tag
// demands: a secret that lists who may ask for it sets whatever cap it likes, or none at all
// (TestUncappedListedLoginAwaitsOwner).
func TestRequestersAListedSecretGrantsForNoLongerThanItsOwnMaxGrant(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newFixture(t)
	retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, fixtureOperator))

	if req, err := m.Create(ctx, enr, signRequestFor(t, m, key, 2*anHour, "LISTED_KEY"), ""); !errors.Is(err, ErrGrantRequiresLifetime) {
		t.Fatalf("Create(LISTED_KEY, 2h) = %+v, %v; want ErrGrantRequiresLifetime", req, err)
	}
	granted, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
	if err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("Create(LISTED_KEY, 1h) = %+v, %v; want it granted", granted, err)
	}
	values, expires, err := m.Values(ctx, *granted.GrantID, enr, nil)
	if err != nil || values["LISTED_KEY"] != listedValue {
		t.Fatalf("Values = %v, %v; want the value", values, err)
	}
	if expires == nil || time.Until(*expires) > time.Hour || time.Until(*expires) < 59*time.Minute {
		t.Fatalf("the grant expires %v; want an hour from now", expires)
	}
}

// TestRequestersAPodsGrantEndsWhenItsAccountStopsProvingItsService: an entry naming a service
// names the pods whose account BROKER_SERVICES maps to it, and the policy's version covers the
// tag and not that mapping. A broker restarted with a mapping that no longer names this pod's
// account answers it requester-not-listed, so its live grant stops releasing at its very next
// read and is ended for good, instead of running to its expiry on a list it is no longer on.
// The mapping left standing is the control: the same release answers the value, which is what
// makes the end the mapping's doing and not the re-check's.
func TestRequestersAPodsGrantEndsWhenItsAccountStopsProvingItsService(t *testing.T) {
	for _, tc := range []struct {
		name  string
		unmap bool
	}{
		{name: "the account no longer proves the service", unmap: true},
		{name: "the mapping stands", unmap: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, _, _, _ := newFixture(t)
			retag(t, m, listedSecret("LISTED_POD_KEY", policy.OwnerShared, policy.TierAgent, fixtureService))
			pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())
			granted, err := m.Create(ctx, pod, signRequestFor(t, m, podKey, anHour, "LISTED_POD_KEY"), "")
			if err != nil || granted.State != "granted" || granted.GrantID == nil {
				t.Fatalf("Create(LISTED_POD_KEY) = %+v, %v; want it granted at once to the pod the list names", granted, err)
			}
			if values, _, err := m.Values(ctx, *granted.GrantID, pod, nil); err != nil || values["LISTED_POD_KEY"] != listedValue {
				t.Fatalf("Values while the account proves the service = %v, %v; want the value", values, err)
			}
			version := m.Policy.Get().Version

			if tc.unmap {
				m.ServicesByAccount = map[string]string{}
			}
			values, _, err := m.Values(ctx, *granted.GrantID, pod, nil)
			if now := m.Policy.Get().Version; now != version {
				t.Fatalf("the policy moved from version %s to %s; this case turns on the version standing still", version, now)
			}
			if !tc.unmap {
				if err != nil || values["LISTED_POD_KEY"] != listedValue {
					t.Fatalf("Values while the mapping stands = %v, %v; want the value", values, err)
				}
				if by, audits := endedForGood(t, m, *granted.GrantID); by != "" || audits != 0 {
					t.Fatalf("grant revoked_by %q with %d grant.revoked rows; want it live", by, audits)
				}
				return
			}
			if !errors.Is(err, ErrGrantNotLive) || values != nil {
				t.Fatalf("Values once the account no longer proves the service = %v, %v; want ErrGrantNotLive", values, err)
			}
			if by, audits := endedForGood(t, m, *granted.GrantID); by != GrantEndedActor || audits != 1 {
				t.Fatalf("grant revoked_by %q with %d grant.revoked rows by %s; want it ended once", by, audits, GrantEndedActor)
			}
		})
	}
}

// TestReuseRefusesAPodWhoseServiceIsNoLongerListed drives reuseLiveGrant's requesters re-check on
// its own. That re-check runs behind a gate of three terms — the policy's version has moved, a
// granted name is a service's own (anyServiceOwned), or a granted name carries a requesters tag
// (anyListed) — and here only the last of them can open it: the secret is shared, so no name is
// service-owned, and emptying the account mapping changes no tag, so the policy's version stands
// still. A pod that asks again for the name it already holds, off the list by that mapping alone,
// is answered a denied request of its own naming the secret and the rule, and the grant the reuse
// passed over is ended for good there rather than left live until its next release reads it.
func TestReuseRefusesAPodWhoseServiceIsNoLongerListed(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := newFixture(t)
	retag(t, m, listedSecret("LISTED_POD_KEY", policy.OwnerShared, policy.TierAgent, fixtureService))
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())
	granted, err := m.Create(ctx, pod, signRequestFor(t, m, podKey, anHour, "LISTED_POD_KEY"), "")
	if err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("Create(LISTED_POD_KEY) = %+v, %v; want it granted at once to the pod the list names", granted, err)
	}
	version := m.Policy.Get().Version

	m.ServicesByAccount = map[string]string{}
	const want = "LISTED_POD_KEY: " + policy.ReasonRequesterNotListed
	denied, err := m.Create(ctx, pod, signRequestFor(t, m, podKey, anHour, "LISTED_POD_KEY"), "")
	if now := m.Policy.Get().Version; now != version {
		t.Fatalf("the policy moved from version %s to %s; this case turns on the version standing still, so that the requesters tag is the only term that can open the re-check", version, now)
	}
	if err != nil || denied.State != "denied" || denied.ID == granted.ID || denied.GrantID != nil {
		t.Fatalf("Create once the account no longer proves the service = %+v, %v; want a denied request of its own, not the live grant reused", denied, err)
	}
	if denied.Detail == nil || *denied.Detail != want {
		t.Fatalf("the denied request's detail = %v, want %q", denied.Detail, want)
	}
	if by, audits := endedForGood(t, m, *granted.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("the grant the reuse passed over: revoked_by %q with %d grant.revoked rows by %s; want it ended once", by, audits, GrantEndedActor)
	}
}

// TestRequestersADroppedNameIsDeniedBeforeAnEarlierNameRefusesTheApprover: a request of two names
// whose first name the deciding login may no longer approve, and whose second the list has
// dropped, is answered rather than left pending. The first name's refusal would keep the card on
// a page where approving it does nothing and denying it is the only move left, for a request no
// owner change could ever make releasable, so the list's deny is read over every name first.
func TestRequestersADroppedNameIsDeniedBeforeAnEarlierNameRefusesTheApprover(t *testing.T) {
	ctx := context.Background()
	m, enr, key, approver := newFixture(t)
	retag(t, m,
		policytest.Secret("ALPHA_HUMAN_KEY", fixtureOperator, policy.TierHuman, "alpha-v1"),
		listedSecret("ZETA_LISTED_KEY", fixtureOperator, policy.TierHuman, fixtureOperator))
	req, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "ALPHA_HUMAN_KEY", "ZETA_LISTED_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(ALPHA_HUMAN_KEY, ZETA_LISTED_KEY) = %+v, %v; want one card pending on their owner", req, err)
	}

	// ALPHA_HUMAN_KEY, which the names are read in order of, moves to another owner, so the
	// current policy has someone other than the approver decide it; ZETA_LISTED_KEY leaves the
	// list.
	retag(t, m,
		policytest.Secret("ALPHA_HUMAN_KEY", otherPerson, policy.TierHuman, "alpha-v1"),
		listedSecret("ZETA_LISTED_KEY", fixtureOperator, policy.TierHuman, otherPerson))
	const want = "ZETA_LISTED_KEY: " + policy.ReasonRequesterNotListed
	decision, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil {
		t.Fatalf("ApplyDecision(approve) = %+v, %v; want the dropped name's denial, not a refusal that leaves the card pending", decision, err)
	}
	if decision.State != "denied" || decision.GrantID != "" || decision.Detail != want {
		t.Fatalf("ApplyDecision = %+v; want it denied with no grant and the detail %q", decision, want)
	}
	var state, decidedBy, detail string
	var grants int
	if err := m.Store.Pool.QueryRow(ctx, `select r.state, coalesce(r.decided_by,''), coalesce(r.decision_detail,''),
		(select count(*) from grants g where g.request_id=r.id) from requests r where r.id=$1`, req.ID).
		Scan(&state, &decidedBy, &detail, &grants); err != nil {
		t.Fatalf("read the request row: %v", err)
	}
	if state != "denied" || decidedBy != PolicyDeniedActor || detail != want || grants != 0 {
		t.Fatalf("the request row = state %q decided_by %q detail %q with %d grants; want it denied by %s, %q, and no grant",
			state, decidedBy, detail, grants, PolicyDeniedActor, want)
	}
}

// TestRequestersAWithheldNameTheListDropsIsDeniedRatherThanRefused: a name its operator withheld
// from the session is the owner's alone to approve, and one the policy denies them is no one's —
// but a name the list dropped is no one's for good, so it is answered as the list's denial and
// not as that refusal, which would leave the card pending until it expired.
func TestRequestersAWithheldNameTheListDropsIsDeniedRatherThanRefused(t *testing.T) {
	ctx := context.Background()
	m, enr, key, approver := newFixture(t)
	retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, fixtureOperator))
	auto, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(LISTED_KEY) = %+v, %v; want the automatic grant the list allows", auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, fixtureOperator, record.HumanActor(fixtureOperator)); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	pending, err := m.Create(ctx, enr, signRequestFor(t, m, key, anHour, "LISTED_KEY"), "")
	if err != nil || pending.State != "pending" || pending.RecordID == nil {
		t.Fatalf("Create(LISTED_KEY) once it is withheld = %+v, %v; want a card pending on its owner", pending, err)
	}

	retag(t, m, listedSecret("LISTED_KEY", fixtureOperator, policy.TierAgent, otherPerson))
	const want = "LISTED_KEY: " + policy.ReasonRequesterNotListed
	decision, err := m.ApplyDecision(ctx, *pending.RecordID, true, approver)
	if err != nil {
		t.Fatalf("ApplyDecision(approve) of a withheld name the list dropped = %+v, %v; want its denial", decision, err)
	}
	if decision.State != "denied" || decision.GrantID != "" || decision.Detail != want {
		t.Fatalf("ApplyDecision = %+v; want it denied with no grant and the detail %q", decision, want)
	}
	if _, _, err := m.Values(ctx, *auto.GrantID, enr, nil); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values of the withheld grant = %v; want it still ended", err)
	}
}

// accountValue is the stored value of the google-service-account secret below: the one account
// its token would act as. No request here releases it, so no minter ever reads it; it is here
// because the kind's secret is no secret without one.
const accountValue = `{"service_account": "releases@example-project.iam.gserviceaccount.com"}`

// TestServiceRequesterAwaitsOwner: a human-tier google-service-account secret may name registered
// services beside the one person who owns it (policy.TagRules), and the list is then all the
// service's pods get. The pod asks and waits: the tier decides once the list has let it through,
// so the card is the owner's to approve rather than a token handed over without asking, which is
// what keeps a service account a person still stands behind. Every session the list leaves out is
// refused as before — another person's is denied requester-not-listed and leaves no card on
// anyone's page, so naming a service widens who may ask and nothing else.
func TestServiceRequesterAwaitsOwner(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := newFixture(t)
	account := policytest.KindSecret("GCLOUD_ACCOUNT_KEY", fixtureOperator, policy.TierHuman,
		policy.KindGoogleServiceAccount, accountValue)
	account.Tags[policy.TagRequesters] = fixtureOperator + " " + fixtureService
	retag(t, m, account)

	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())
	pending, err := m.Create(ctx, pod, signRequestFor(t, m, podKey, anHour, "GCLOUD_ACCOUNT_KEY"), "")
	if err != nil || pending.State != "pending" || pending.RecordID == nil || pending.GrantID != nil {
		t.Fatalf("Create(GCLOUD_ACCOUNT_KEY) by a pod of the listed service = %+v, %v; want it pending with no grant", pending, err)
	}
	if pending.Approver == nil || *pending.Approver != fixtureOperator {
		t.Fatalf("the pod's card names approver %v, want the secret's one person owner %s", pending.Approver, fixtureOperator)
	}
	waiting, err := m.PendingForApprover(ctx, fixtureOperator)
	if err != nil || len(waiting) != 1 || waiting[0].RecordID != *pending.RecordID {
		t.Fatalf("the owner's pending list = %+v, %v; want the pod's card %s waiting on it", waiting, err, *pending.RecordID)
	}

	other, otherKey := newEnrollment(t, m.Store, "session", "box-other-"+t.Name(), new(otherPerson), nil)
	const want = "GCLOUD_ACCOUNT_KEY: " + policy.ReasonRequesterNotListed
	denied, err := m.Create(ctx, other, signRequestFor(t, m, otherKey, anHour, "GCLOUD_ACCOUNT_KEY"), "")
	if err != nil || denied.State != "denied" || denied.GrantID != nil || denied.RecordID != nil {
		t.Fatalf("Create(GCLOUD_ACCOUNT_KEY) by another person's session = %+v, %v; want it denied with no grant and no card", denied, err)
	}
	if denied.Detail == nil || *denied.Detail != want {
		t.Fatalf("the denied request's detail = %v, want %q", denied.Detail, want)
	}
	// The owner's page is as it was, and the refused person's is empty: a request the list denied
	// leaves nothing for anyone to approve, neither under the owner the secret names nor under
	// the person who asked, whose page would also show a card any approver may decide.
	if waiting, err := m.PendingForApprover(ctx, fixtureOperator); err != nil || len(waiting) != 1 || waiting[0].RecordID != *pending.RecordID {
		t.Fatalf("the owner's pending list after the refusal = %+v, %v; want the pod's card %s still alone on it", waiting, err, *pending.RecordID)
	}
	if waiting, err := m.PendingForApprover(ctx, otherPerson); err != nil || len(waiting) != 0 {
		t.Fatalf("the refused person's pending list = %+v, %v; want it empty", waiting, err)
	}
}

// TestUncappedListedLoginAwaitsOwner: a secret that merely lists its requesters bounds no grant.
// A human-tier google-login secret may therefore carry a requesters list and no max-grant at all,
// and its owner's own request may ask for eight hours — longer than the hour the deleted rule
// forced on every listed secret — and be left pending at that length for its owner to approve,
// rather than refused ErrGrantRequiresLifetime before the policy decides it: a list says who may
// ask, never for how long. The request signs that lifetime rather than naming none, because one
// naming none takes DefaultHumanGrantLifetime, fifteen minutes, which is inside an hour and so
// would be accepted just as readily by a broker that capped every listed secret at one.
func TestUncappedListedLoginAwaitsOwner(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newFixture(t)
	login := policytest.KindSecret("UNCAPPED_LOGIN_KEY", fixtureOperator, policy.TierHuman, policy.KindGoogleLogin, "refresh-v1")
	login.Tags[policy.TagRequesters] = fixtureOperator
	retag(t, m, login)
	if cap := m.Policy.Get().Secrets["UNCAPPED_LOGIN_KEY"].MaxGrant; cap != 0 {
		t.Fatalf("the secret under test caps its grants at %s; this case is the listed secret that caps none", cap)
	}

	const eightHours = 8 * anHour
	req, err := m.Create(ctx, enr, signRequestFor(t, m, key, eightHours, "UNCAPPED_LOGIN_KEY"), "")
	if errors.Is(err, ErrGrantRequiresLifetime) {
		t.Fatalf("Create(UNCAPPED_LOGIN_KEY, 8h) = %v; want no refusal over the grant's length from a secret that bounds none", err)
	}
	if err != nil || req.State != "pending" || req.RecordID == nil || req.Detail != nil || req.GrantID != nil {
		t.Fatalf("Create(UNCAPPED_LOGIN_KEY, 8h) = %+v, %v; want it pending on its owner, neither denied nor granted", req, err)
	}
	if req.LifetimeSeconds == nil || *req.LifetimeSeconds != eightHours {
		t.Fatalf("the pending request asks for %v seconds, want the %d seconds it signed: nothing changes a signed lifetime, and a secret with no cap does not refuse it", req.LifetimeSeconds, eightHours)
	}
	waiting, err := m.PendingForApprover(ctx, fixtureOperator)
	if err != nil || len(waiting) != 1 || waiting[0].RecordID != *req.RecordID {
		t.Fatalf("the owner's pending list = %+v, %v; want the uncapped request's card %s waiting on it", waiting, err, *req.RecordID)
	}
}
