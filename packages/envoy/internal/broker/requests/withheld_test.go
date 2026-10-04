package requests

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// sharedToken is a shared agent-tier secret, which every session gets without asking.
const sharedToken = "SHARED_TOKEN"

// carol is a person who owns none of the fixture's secrets.
const carol = "carol@example.com"

func withSharedToken(t *testing.T, m *Machine) {
	t.Helper()
	retag(t, m, policytest.Secret(sharedToken, policy.OwnerShared, policy.TierAgent, "shared-token-v1"))
}

// recordApprover reads the approver a pending request's record names.
func recordApprover(t *testing.T, m *Machine, req Request) string {
	t.Helper()
	if req.RecordID == nil {
		t.Fatalf("request %+v has no record", req)
	}
	detail, err := m.ReadRecord(context.Background(), *req.RecordID)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	return detail.Approver
}

// withheldFrom reads the names withheld from enrollment enr, in name order.
func withheldFrom(t *testing.T, m *Machine, enr string) []string {
	t.Helper()
	rows, err := m.Store.Pool.Query(context.Background(), `select name from withheld_secrets where enrollment_id=$1 order by name`, enr)
	if err != nil {
		t.Fatalf("read withheld names: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read withheld names: %v", err)
	}
	return names
}

// revokedAudit reads grant grantID's one grant.revoked audit row (grantAudit).
func revokedAudit(t *testing.T, m *Machine, grantID string) (string, []string) {
	t.Helper()
	return grantAudit(t, m, "grant.revoked", grantID)
}

// grantAudit reads grant grantID's one audit row of kind: its actor and the names its detail lists
// under "withheld" (nil when it has none).
func grantAudit(t *testing.T, m *Machine, kind, grantID string) (string, []string) {
	t.Helper()
	type row struct {
		Actor    string
		Withheld []string
	}
	rows, err := m.Store.Pool.Query(context.Background(), `select actor, array(select jsonb_array_elements_text(detail->'withheld')) as withheld
		from audit where kind=$1 and grant_id=$2`, kind, grantID)
	if err != nil {
		t.Fatalf("read %s audit: %v", kind, err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil || len(got) != 1 {
		t.Fatalf("%s audit rows of %s = %+v, %v; want one", kind, grantID, got, err)
	}
	if len(got[0].Withheld) == 0 {
		return got[0].Actor, nil
	}
	return got[0].Actor, got[0].Withheld
}

// retagUnrelated adds a secret none of the fixture's requests name, which moves the policy to a
// new version as any tag change in the namespace does at the broker's next refresh.
func retagUnrelated(t *testing.T, m *Machine) {
	t.Helper()
	before := m.Policy.Get().Version
	retag(t, m, policytest.Secret("UNRELATED_KEY", otherPerson, policy.TierAgent, "unrelated-v1"))
	if m.Policy.Get().Version == before {
		t.Fatalf("policy version %s did not change", before)
	}
}

// TestRevokingAnAutomaticGrantWithholdsItFromThatSessionAlone pins what revoking an automatic grant
// from Live grants does: the grant is listed as automatic before, the person who operates the
// session revokes it, and that session's next request for the secret is an approval request to the
// secret's owner (anyone, for a shared secret) instead of a new automatic grant, while another of
// the person's sessions still gets it at once, and the same session still gets its other secrets
// at once. The revoke's audit row names the withheld secret. Once approved, the session's later
// requests reuse that approval.
func TestRevokingAnAutomaticGrantWithholdsItFromThatSessionAlone(t *testing.T) {
	for _, c := range []struct {
		name, other, approver, decidedBy string
	}{
		{"AUTO_TOKEN", sharedToken, fixtureOperator, fixtureOperator},
		{sharedToken, "AUTO_TOKEN", record.AnyoneApprover, carol},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, operator := newFixture(t)
			withSharedToken(t, m)
			ctx := context.Background()
			other, otherKey := newEnrollment(t, m.Store, "box", "box-b-"+t.Name(), new(fixtureOperator), nil)

			auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", c.name), "")
			if err != nil || auto.State != "granted" || auto.GrantID == nil || auto.RecordID != nil {
				t.Fatalf("Create = %+v, %v; want an automatic grant", auto, err)
			}
			listed, err := m.GrantsForApprover(ctx, operator)
			if err != nil || len(listed) != 1 || listed[0].GrantID != *auto.GrantID || listed[0].Granted != policy.Automatic || listed[0].Approver != "" || listed[0].RecordID != nil {
				t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the automatic grant, with no approver and no record", operator, listed, err)
			}
			if others, err := m.GrantsForApprover(ctx, otherPerson); err != nil || len(others) != 0 {
				t.Fatalf("GrantsForApprover(%s) = %+v, %v; want nothing: the grant is neither theirs to approve nor on their session", otherPerson, others, err)
			}
			if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
				t.Fatalf("RevokeByApprover: %v", err)
			}
			if actor, withheld := revokedAudit(t, m, *auto.GrantID); actor != "human:"+operator || !slices.Equal(withheld, []string{c.name}) {
				t.Fatalf("grant.revoked audit = %s withheld %v; want human:%s withheld [%s]", actor, withheld, operator, c.name)
			}

			again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it again", c.name), "")
			if err != nil || again.State != "pending" || again.GrantID != nil {
				t.Fatalf("Create after the revoke = %+v, %v; want an approval request", again, err)
			}
			if got := recordApprover(t, m, again); got != c.approver {
				t.Fatalf("approver = %q, want %q", got, c.approver)
			}
			if len(again.Secrets) != 1 || again.Secrets[0].Decision != policy.Approval {
				t.Fatalf("decisions = %+v, want %s decided approval", again.Secrets, c.name)
			}

			if rest, err := m.Create(ctx, enr, signRequest(t, m, key, "something else", c.other), ""); err != nil || rest.State != "granted" || rest.GrantID == nil {
				t.Fatalf("Create(%s) on the same session = %+v, %v; want an automatic grant: only %s is withheld", c.other, rest, err, c.name)
			}
			elsewhere, err := m.Create(ctx, other, signRequest(t, m, otherKey, "need it", c.name), "")
			if err != nil || elsewhere.State != "granted" || elsewhere.GrantID == nil {
				t.Fatalf("Create on another of the person's sessions = %+v, %v; want an automatic grant", elsewhere, err)
			}

			dec, err := m.ApplyDecision(ctx, *again.RecordID, true, c.decidedBy)
			if err != nil || dec.GrantID == "" {
				t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", c.decidedBy, dec, err)
			}
			if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values[c.name] == "" {
				t.Fatalf("Values of the approved grant = %v, %v", values, err)
			}
			reused, err := m.Create(ctx, enr, signRequest(t, m, key, "and again", c.name), "")
			if err != nil || reused.ID != again.ID {
				t.Fatalf("Create once approved = %+v, %v; want the approved grant reused", reused, err)
			}
		})
	}
}

// TestAWithheldNameIsNotReleasedThroughAnotherLiveGrant pins that the revoke ends the session's
// other grants of the withheld secret it got without asking: with grants of {AUTO_TOKEN,
// SHARED_TOKEN}, {AUTO_TOKEN} and {SHARED_TOKEN} live, revoking the second ends the first as well,
// which no longer releases AUTO_TOKEN, leaves Live grants and carries a grant.revoked audit row of
// its own by the same person naming AUTO_TOKEN, while the third, which holds no withheld name,
// stays live. The session's next AUTO_TOKEN request asks its owner.
func TestAWithheldNameIsNotReleasedThroughAnotherLiveGrant(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	pair, err := m.Create(ctx, enr, signRequest(t, m, key, "both", "AUTO_TOKEN", sharedToken), "")
	if err != nil || pair.GrantID == nil {
		t.Fatalf("Create(pair) = %+v, %v; want granted", pair, err)
	}
	single, err := m.Create(ctx, enr, signRequest(t, m, key, "one", "AUTO_TOKEN"), "")
	if err != nil || single.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", single, err)
	}
	shared, err := m.Create(ctx, enr, signRequest(t, m, key, "shared", sharedToken), "")
	if err != nil || shared.GrantID == nil {
		t.Fatalf("Create(%s) = %+v, %v; want granted", sharedToken, shared, err)
	}
	if err := m.RevokeByApprover(ctx, *single.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	if again, err := m.Create(ctx, enr, signRequest(t, m, key, "one again", "AUTO_TOKEN"), ""); err != nil || again.State != "pending" {
		t.Fatalf("control: Create(AUTO_TOKEN) after the withhold = %+v, %v; want pending", again, err)
	}
	values, _, err := m.Values(ctx, *pair.GrantID, enr)
	if !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values(pair grant) after AUTO_TOKEN was withheld = %v, %v; want ErrGrantNotLive", values, err)
	}
	if actor, withheld := revokedAudit(t, m, *pair.GrantID); actor != "human:"+operator || !slices.Equal(withheld, []string{"AUTO_TOKEN"}) {
		t.Fatalf("pair's grant.revoked audit = %s withheld %v; want human:%s withheld [AUTO_TOKEN]", actor, withheld, operator)
	}
	listed, err := m.GrantsForApprover(ctx, operator)
	if err != nil || len(listed) != 1 || listed[0].GrantID != *shared.GrantID {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the %s grant alone", operator, listed, err, sharedToken)
	}
	if values, _, err := m.Values(ctx, *shared.GrantID, enr); err != nil || values[sharedToken] == "" {
		t.Fatalf("Values(%s grant) = %v, %v; want it released: nothing it holds is withheld", sharedToken, values, err)
	}
}

// TestRevokingAGrantOfAnAlreadyWithheldNameSucceeds pins a second revoke on a session that names
// a secret already withheld from it: a request for {AUTO_TOKEN, DEEL_API_KEY} waits on the operator
// when the operator withholds AUTO_TOKEN by revoking an automatic grant of it; the operator then
// approves the request and revokes that grant. The second revoke succeeds, withholds nothing new
// (DEEL_API_KEY, which the request got by approval, is never withheld), and its audit row lists
// nothing under withheld.
func TestRevokingAGrantOfAnAlreadyWithheldNameSucceeds(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	mixed, err := m.Create(ctx, enr, signRequest(t, m, key, "deploy", "AUTO_TOKEN", "DEEL_API_KEY"), "")
	if err != nil || mixed.State != "pending" {
		t.Fatalf("Create(AUTO_TOKEN, DEEL_API_KEY) = %+v, %v; want pending", mixed, err)
	}
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "one", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover(automatic grant): %v", err)
	}
	dec, err := m.ApplyDecision(ctx, *mixed.RecordID, true, operator)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", operator, dec, err)
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover(approved grant holding the withheld AUTO_TOKEN) = %v; want it revoked", err)
	}
	if actor, withheld := revokedAudit(t, m, dec.GrantID); actor != "human:"+operator || withheld != nil {
		t.Fatalf("second grant.revoked audit = %s withheld %v; want human:%s withholding nothing new", actor, withheld, operator)
	}
	if got := withheldFrom(t, m, enr); !slices.Equal(got, []string{"AUTO_TOKEN"}) {
		t.Fatalf("withheld = %v, want [AUTO_TOKEN]", got)
	}
}

// TestAnAnyoneApprovalOfAWithheldSecretStopsOnceTheSecretIsTheOperators pins that a release and a
// reuse decide for the session with what its operator withheld from it: SHARED_TOKEN is withheld,
// carol approves the session's request for it (anyone may, while it is shared), and once it is
// retagged to the operator's agent tier the operator alone may approve it for this session, so
// carol's grant releases nothing and the next request asks the operator rather than reusing it.
func TestAnAnyoneApprovalOfAWithheldSecretStopsOnceTheSecretIsTheOperators(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", sharedToken), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want granted", auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "again", sharedToken), "")
	if err != nil || again.State != "pending" {
		t.Fatalf("Create after the withhold = %+v, %v; want pending", again, err)
	}
	dec, err := m.ApplyDecision(ctx, *again.RecordID, true, carol)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", carol, dec, err)
	}
	retag(t, m, policytest.Secret(sharedToken, operator, policy.TierAgent, "shared-token-v1"))
	if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values(%s's grant) once %s is %s's = %v, %v; want ErrGrantNotLive", carol, sharedToken, operator, values, err)
	}
	next, err := m.Create(ctx, enr, signRequest(t, m, key, "once more", sharedToken), "")
	if err != nil || next.State != "pending" || recordApprover(t, m, next) != operator {
		t.Fatalf("Create once %s is %s's = %+v, %v; want a new request waiting on %s", sharedToken, operator, next, err, operator)
	}
}

// TestAnAnyoneApprovalAfterTheWithholdDoesNotReleaseAPersonsWithheldSecret pins who may approve a
// request that was pending when its operator withheld one of its names: {AUTO_TOKEN (automatic),
// SHARED_KEY (shared, human tier)} waits on anyone, the operator withholds AUTO_TOKEN, and carol,
// who does not own AUTO_TOKEN, is refused NOT_APPROVER; the operator, its owner, still approves it.
func TestAnAnyoneApprovalAfterTheWithholdDoesNotReleaseAPersonsWithheldSecret(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	single, err := m.Create(ctx, enr, signRequest(t, m, key, "one", "AUTO_TOKEN"), "")
	if err != nil || single.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", single, err)
	}
	mixed, err := m.Create(ctx, enr, signRequest(t, m, key, "mixed", "AUTO_TOKEN", "SHARED_KEY"), "")
	if err != nil || mixed.State != "pending" || mixed.RecordID == nil {
		t.Fatalf("Create(AUTO_TOKEN, SHARED_KEY) = %+v, %v; want pending", mixed, err)
	}
	if got := recordApprover(t, m, mixed); got != record.AnyoneApprover {
		t.Fatalf("mixed approver = %q, want %s", got, record.AnyoneApprover)
	}
	if err := m.RevokeByApprover(ctx, *single.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	if dec, err := m.ApplyDecision(ctx, *mixed.RecordID, true, carol); !errors.Is(err, record.ErrNotApprover) {
		values, _, verr := m.Values(ctx, dec.GrantID, enr)
		t.Fatalf("ApplyDecision(%s) after %s withheld AUTO_TOKEN = %+v, %v (Values %v, %v); want NOT_APPROVER", carol, operator, dec, err, values, verr)
	}
	dec, err := m.ApplyDecision(ctx, *mixed.RecordID, true, operator)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", operator, dec, err)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["AUTO_TOKEN"] == "" {
		t.Fatalf("Values of the owner's approval = %v, %v; want AUTO_TOKEN released", values, err)
	}
}

// TestTheOwnersApprovalOfAWithheldSecretOutlivesAnUnrelatedTagChange pins that an approval the
// withhold left to a secret's owner stands like any other approval: {AUTO_TOKEN (automatic),
// DEEL_API_KEY} waits on the operator when the operator withholds AUTO_TOKEN, the operator approves
// it, and once another secret's tags change, the grant still releases AUTO_TOKEN and the same
// request again reuses it.
func TestTheOwnersApprovalOfAWithheldSecretOutlivesAnUnrelatedTagChange(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	mixed, err := m.Create(ctx, enr, signRequest(t, m, key, "deploy", "AUTO_TOKEN", "DEEL_API_KEY"), "")
	if err != nil || mixed.State != "pending" {
		t.Fatalf("Create(AUTO_TOKEN, DEEL_API_KEY) = %+v, %v; want pending", mixed, err)
	}
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "one", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	dec, err := m.ApplyDecision(ctx, *mixed.RecordID, true, operator)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", operator, dec, err)
	}
	retagUnrelated(t, m)
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values["AUTO_TOKEN"] == "" {
		t.Fatalf("Values of the owner's approval once another secret's tags changed = %v, %v; want AUTO_TOKEN released", values, err)
	}
	reused, err := m.Create(ctx, enr, signRequest(t, m, key, "deploy again", "AUTO_TOKEN", "DEEL_API_KEY"), "")
	if err != nil || reused.ID != mixed.ID {
		t.Fatalf("Create(AUTO_TOKEN, DEEL_API_KEY) again = %+v, %v; want the owner's approval reused", reused, err)
	}
}

// TestAnAnyoneApprovalOfAWithheldSharedSecretOutlivesAnUnrelatedTagChange pins the same for a
// shared secret, which anyone may approve: {SHARED_TOKEN (automatic), SHARED_KEY} waits on anyone
// when the operator withholds SHARED_TOKEN, carol approves it, and once another secret's tags
// change the grant still releases SHARED_TOKEN. Once SHARED_TOKEN is the operator's, only the
// operator may approve it for this session, so carol's grant stops.
func TestAnAnyoneApprovalOfAWithheldSharedSecretOutlivesAnUnrelatedTagChange(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	mixed, err := m.Create(ctx, enr, signRequest(t, m, key, "deploy", sharedToken, "SHARED_KEY"), "")
	if err != nil || mixed.State != "pending" {
		t.Fatalf("Create(%s, SHARED_KEY) = %+v, %v; want pending", sharedToken, mixed, err)
	}
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "one", sharedToken), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(%s) = %+v, %v; want granted", sharedToken, auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	dec, err := m.ApplyDecision(ctx, *mixed.RecordID, true, carol)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", carol, dec, err)
	}
	retagUnrelated(t, m)
	if values, _, err := m.Values(ctx, dec.GrantID, enr); err != nil || values[sharedToken] == "" {
		t.Fatalf("Values of %s's approval once another secret's tags changed = %v, %v; want %s released", carol, values, err, sharedToken)
	}
	retag(t, m, policytest.Secret(sharedToken, operator, policy.TierAgent, "shared-token-v1"))
	if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values of %s's approval once %s is %s's = %v, %v; want ErrGrantNotLive", carol, sharedToken, operator, values, err)
	}
}

// TestTheOperatorsRevokeOfAGrantThatAlreadyEndedStillWithholds pins a revoke from a stale Live
// grants list: the operator lists the session's automatic AUTO_TOKEN grant, the session then ends
// it itself, and the operator revokes it. The revoke still withholds AUTO_TOKEN, so the session's
// next request for it asks the operator. The session stays the grant's one revoker in the audit,
// a grant.withheld row by the operator names AUTO_TOKEN, and revoking it again writes nothing.
func TestTheOperatorsRevokeOfAGrantThatAlreadyEndedStillWithholds(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "one", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", auto, err)
	}
	listed, err := m.GrantsForApprover(ctx, operator)
	if err != nil || len(listed) != 1 || listed[0].GrantID != *auto.GrantID {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the automatic grant", operator, listed, err)
	}
	if err := m.RevokeGrant(ctx, *auto.GrantID, enr); err != nil {
		t.Fatalf("RevokeGrant by its session: %v", err)
	}
	for range 2 {
		if err := m.RevokeByApprover(ctx, listed[0].GrantID, operator); err != nil {
			t.Fatalf("RevokeByApprover(the grant its session ended) = %v; want success", err)
		}
	}
	if got := withheldFrom(t, m, enr); !slices.Equal(got, []string{"AUTO_TOKEN"}) {
		t.Fatalf("withheld after the operator's revoke of an ended grant = %v, want [AUTO_TOKEN]", got)
	}
	if actor, withheld := revokedAudit(t, m, *auto.GrantID); actor != "session:"+enr || withheld != nil {
		t.Fatalf("grant.revoked audit = %s withheld %v; want session:%s alone, withholding nothing", actor, withheld, enr)
	}
	if actor, withheld := grantAudit(t, m, "grant.withheld", *auto.GrantID); actor != "human:"+operator || !slices.Equal(withheld, []string{"AUTO_TOKEN"}) {
		t.Fatalf("grant.withheld audit = %s withheld %v; want human:%s withheld [AUTO_TOKEN]", actor, withheld, operator)
	}
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "again", "AUTO_TOKEN"), "")
	if err != nil || again.State != "pending" || recordApprover(t, m, again) != operator {
		t.Fatalf("Create(AUTO_TOKEN) after the operator's revoke = %+v, %v; want a request waiting on %s", again, err, operator)
	}
}

// TestTheWithholdLeavesTheSessionsApprovedGrantOfTheSecretLive pins that the withhold ends only
// the session's grants that got a withheld secret without asking: the operator approves the
// session's request for DEEL_API_KEY while it is human tier, it then becomes the operator's agent
// tier, the session gets {DEEL_API_KEY, AUTO_TOKEN} at once, and the operator revokes that
// automatic grant. Both are withheld, and the grant the operator approved still releases
// DEEL_API_KEY.
func TestTheWithholdLeavesTheSessionsApprovedGrantOfTheSecretLive(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	asked, err := m.Create(ctx, enr, signRequest(t, m, key, "deel", "DEEL_API_KEY"), "")
	if err != nil || asked.State != "pending" {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want pending", asked, err)
	}
	approved, err := m.ApplyDecision(ctx, *asked.RecordID, true, operator)
	if err != nil || approved.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", operator, approved, err)
	}
	retag(t, m, policytest.Secret("DEEL_API_KEY", operator, policy.TierAgent, "deel-v1"))
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "both", "DEEL_API_KEY", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil || auto.RecordID != nil {
		t.Fatalf("Create(DEEL_API_KEY, AUTO_TOKEN) once DEEL_API_KEY is agent tier = %+v, %v; want an automatic grant", auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	if got := withheldFrom(t, m, enr); !slices.Equal(got, []string{"AUTO_TOKEN", "DEEL_API_KEY"}) {
		t.Fatalf("withheld = %v, want [AUTO_TOKEN DEEL_API_KEY]", got)
	}
	if values, _, err := m.Values(ctx, approved.GrantID, enr); err != nil || values["DEEL_API_KEY"] == "" {
		t.Fatalf("Values of the grant %s approved = %v, %v; want DEEL_API_KEY released: the withhold ends only grants that got it without asking", operator, values, err)
	}
}

// TestAnApprovalAfterTheWithholdOutlivesTheOperatorsLaterRevokes pins that a revoke withholding
// nothing new ends no grant but its own: the operator withholds AUTO_TOKEN and SHARED_TOKEN by
// revoking the session's automatic {AUTO_TOKEN, SHARED_TOKEN} grant, which also ends its automatic
// AUTO_TOKEN grant, and then approves two requests that were waiting with AUTO_TOKEN in them.
// Revoking the ended AUTO_TOKEN grant (from a Live grants list loaded before), and then the first
// approval's grant, leaves the second approval's grant releasing AUTO_TOKEN.
func TestAnApprovalAfterTheWithholdOutlivesTheOperatorsLaterRevokes(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	var waiting []Request
	for _, names := range [][]string{{"AUTO_TOKEN", "DEEL_API_KEY"}, {"AUTO_TOKEN", "DEEL_API_KEY", sharedToken}} {
		req, err := m.Create(ctx, enr, signRequest(t, m, key, "deploy", names...), "")
		if err != nil || req.State != "pending" {
			t.Fatalf("Create(%v) = %+v, %v; want pending", names, req, err)
		}
		waiting = append(waiting, req)
	}
	single, err := m.Create(ctx, enr, signRequest(t, m, key, "one", "AUTO_TOKEN"), "")
	if err != nil || single.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want granted", single, err)
	}
	pair, err := m.Create(ctx, enr, signRequest(t, m, key, "both", "AUTO_TOKEN", sharedToken), "")
	if err != nil || pair.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN, %s) = %+v, %v; want granted", sharedToken, pair, err)
	}
	if err := m.RevokeByApprover(ctx, *pair.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover(pair): %v", err)
	}
	var approved []string
	for _, req := range waiting {
		dec, err := m.ApplyDecision(ctx, *req.RecordID, true, operator)
		if err != nil || dec.GrantID == "" {
			t.Fatalf("ApplyDecision(%s) = %+v, %v; want granted", operator, dec, err)
		}
		approved = append(approved, dec.GrantID)
	}
	releases := func(step string, grants []string) {
		t.Helper()
		for _, g := range grants {
			if values, _, err := m.Values(ctx, g, enr); err != nil || values["AUTO_TOKEN"] == "" {
				t.Fatalf("after %s, Values of the operator's approval %s = %v, %v; want AUTO_TOKEN released", step, g, values, err)
			}
		}
	}
	if err := m.RevokeByApprover(ctx, *single.GrantID, operator); err != nil {
		t.Fatalf("RevokeByApprover(the AUTO_TOKEN grant the withhold ended): %v", err)
	}
	releases("the operator's revoke of the ended AUTO_TOKEN grant", approved)
	if err := m.RevokeByApprover(ctx, approved[0], operator); err != nil {
		t.Fatalf("RevokeByApprover(first approval): %v", err)
	}
	releases("the operator's revoke of the first approval", approved[1:])
}

// TestAnotherPersonsRevokeDoesNotWithholdTheOperatorsOwnSecret pins that only the session's own
// person withholds: carol approves the operator's session's request for {AUTO_TOKEN, SHARED_KEY}
// and then revokes her approval, which ends that grant and withholds nothing, so the session still
// gets the operator's own AUTO_TOKEN at once.
func TestAnotherPersonsRevokeDoesNotWithholdTheOperatorsOwnSecret(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	mixed, err := m.Create(ctx, enr, signRequest(t, m, key, "mixed", "AUTO_TOKEN", "SHARED_KEY"), "")
	if err != nil || mixed.State != "pending" || mixed.RecordID == nil {
		t.Fatalf("Create(AUTO_TOKEN, SHARED_KEY) = %+v, %v; want pending", mixed, err)
	}
	dec, err := m.ApplyDecision(ctx, *mixed.RecordID, true, carol)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v", carol, dec, err)
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, carol); err != nil {
		t.Fatalf("RevokeByApprover(%s) = %v", carol, err)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, enr); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values after %s revoked her approval = %v, %v; want ErrGrantNotLive", carol, values, err)
	}
	if actor, withheld := revokedAudit(t, m, dec.GrantID); actor != "human:"+carol || withheld != nil {
		t.Fatalf("grant.revoked audit = %s withheld %v; want human:%s withholding nothing", actor, withheld, carol)
	}
	withheld := withheldFrom(t, m, enr)
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "mine", "AUTO_TOKEN"), "")
	if err != nil {
		t.Fatalf("Create(AUTO_TOKEN) = %v", err)
	}
	if again.State != "granted" {
		t.Fatalf("after %s revoked her approval, %s's own session's request for %s's own AUTO_TOKEN = %s (withheld rows %v); want granted at once",
			carol, operator, operator, again.State, withheld)
	}
}

// TestRetagToOwnersAgentTierLeavesAWithheldRequestToItsOwner pins that a withheld name follows its
// owner: a shared agent-tier secret withheld from the operator's session waits on anyone, and once
// it is retagged to the operator's own agent tier only the operator may approve it there, so
// carol's approval is refused NOT_APPROVER.
func TestRetagToOwnersAgentTierLeavesAWithheldRequestToItsOwner(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", sharedToken), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v", auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator); err != nil {
		t.Fatal(err)
	}
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "again", sharedToken), "")
	if err != nil || again.State != "pending" || recordApprover(t, m, again) != record.AnyoneApprover {
		t.Fatalf("Create after withhold = %+v, %v; want pending on anyone", again, err)
	}
	retag(t, m, policytest.Secret(sharedToken, operator, policy.TierAgent, "shared-token-v1"))
	session := policy.Requester{Operator: operator, Withheld: []string{sharedToken}}
	if d, err := m.Policy.Get().Evaluate(sharedToken, session); err != nil || d.Approver != operator {
		t.Fatalf("Evaluate(%s) for the session after retag = %+v, %v; want approval by %s", sharedToken, d, err, operator)
	}
	if dec, err := m.ApplyDecision(ctx, *again.RecordID, true, carol); !errors.Is(err, record.ErrNotApprover) {
		values, _, verr := m.Values(ctx, dec.GrantID, enr)
		t.Fatalf("ApplyDecision(%s) after %s became %s's = %+v, %v (Values %v, %v); want NOT_APPROVER", carol, sharedToken, operator, dec, err, values, verr)
	}
}

// TestARequestRacingAWithholdingAsksForApproval pins the race between a request and the revoke
// that withholds its secret: a revoke that commits while the request is deciding (here, a
// transaction holding the session's row as RevokeByApprover does, revoking a grant of the secret
// and withholding it) leaves the request an approval request, never an automatic grant written
// after the revoke.
func TestARequestRacingAWithholdingAsksForApproval(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	pair, err := m.Create(ctx, enr, signRequest(t, m, key, "both", "AUTO_TOKEN", sharedToken), "")
	if err != nil || pair.GrantID == nil {
		t.Fatalf("Create(pair) = %+v, %v; want granted", pair, err)
	}

	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from enrollments where id=$1 for no key update`, enr); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	if _, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1`, *pair.GrantID, "human:"+operator); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := tx.Exec(ctx, `insert into withheld_secrets (enrollment_id, name) values ($1,'AUTO_TOKEN')`, enr); err != nil {
		t.Fatalf("withhold: %v", err)
	}

	type result struct {
		req Request
		err error
	}
	done := make(chan result, 1)
	racing := signRequest(t, m, key, "racing the revoke", "AUTO_TOKEN")
	go func() {
		req, err := m.Create(ctx, enr, racing, "")
		done <- result{req, err}
	}()
	storetest.AwaitLockWaiters(t, m.Store.Pool, tx, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the revoke: %v", err)
	}
	got := <-done
	if got.err != nil || got.req.State != "pending" || got.req.GrantID != nil {
		t.Fatalf("Create racing the revoke = %+v, %v; want an approval request", got.req, got.err)
	}
	if approver := recordApprover(t, m, got.req); approver != fixtureOperator {
		t.Fatalf("approver = %q, want %s", approver, fixtureOperator)
	}
}

// TestRevokeByApproverWaitsForARequestDecidingOnItsSession pins the other half of that race:
// RevokeByApprover takes the session's row before it revokes, so a request already deciding on the
// session (here, a transaction holding the row for share as Create's does) finishes first, and the
// revoke and its withholding land after it.
func TestRevokeByApproverWaitsForARequestDecidingOnItsSession(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want granted", auto, err)
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from enrollments where id=$1 for share`, enr); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.RevokeByApprover(ctx, *auto.GrantID, operator) }()
	storetest.AwaitLockWaiters(t, m.Store.Pool, tx, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RevokeByApprover = %v", err)
	}
}
