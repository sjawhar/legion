// requesters_test.go drives a secret whose requesters tag names who may ask for it through the
// request machine: what a refused session is told, what becomes of a live grant the list drops
// the session from — by a retag, or by an account mapping that stops proving the pod's service
// — what an approval of such a request records, even when another of its names would be refused
// first, and the hour a listed secret bounds its grants to.
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

// anHour is the lifetime every request below signs. A listed secret must bound its grants to an
// hour (policy.RequesterRules), and a request signing longer is refused before the policy decides
// it at all (overMaxGrant), which is this file's last case rather than its premise.
const anHour = 3600

// listedSecret is policytest.ListedSecret holding the one value this file releases: a namespace
// secret whose requesters tag names requesters, carrying the max-grant every listed secret must
// set for the loader to serve it.
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

// TestRequestersAListedSecretGrantsForAnHourAtMost: the hour a listed secret's grants are bounded
// to is enforced where every other max-grant is, before the policy decides the request — a two
// hour request is refused and names the bound, and the hour one is granted for the hour.
func TestRequestersAListedSecretGrantsForAnHourAtMost(t *testing.T) {
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
