// moved_session_test.go holds what a session's move to another box asks of this package: every
// surface keyed by an enrollment answers for the enrollment the rows name now, so a grant, its
// request and the names withheld from the session follow it to the box it enrols on next
// (dispatch://INFRA-1167).
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// moveSession hands everything enrollment from holds to enrollment to, as enroll.Create's own
// adoption does when a session enrols on a box it has not run on before: the grants, the requests
// and the names withheld from the session move by their own enrollment_id, nothing is revoked or
// cancelled, and the predecessor is left holding nothing. The tests below move the rows
// themselves rather than calling enroll.Service.Create, which this package's fixtures cannot use
// (newEnrollment says why).
func moveSession(t *testing.T, m *Machine, from, to string) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"grants", "requests"} {
		if _, err := m.Store.Pool.Exec(ctx, `update `+table+` set enrollment_id=$2 where enrollment_id=$1`, from, to); err != nil {
			t.Fatalf("move %s from %s to %s: %v", table, from, to, err)
		}
	}
	if _, err := m.Store.Pool.Exec(ctx, `with moved as (
			delete from withheld_secrets where enrollment_id=$1 returning name, created_at
		)
		insert into withheld_secrets (enrollment_id, name, created_at)
		select $2, name, created_at from moved on conflict do nothing`, from, to); err != nil {
		t.Fatalf("move the withheld names from %s to %s: %v", from, to, err)
	}
}

// nextBox is the enrollment the session moves to: another live box of the same person, with a
// signing key of its own, which is what its next request object is signed with.
func nextBox(t *testing.T, m *Machine) (string, *ecdsa.PrivateKey) {
	t.Helper()
	return newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
}

// heldRows counts the request rows, grant rows and credential-request records in the fixture's
// schema. storetest.Open gives each test a schema of its own, so these are the one session's rows
// and nobody else's.
func heldRows(t *testing.T, m *Machine) (requests, grants, records int) {
	t.Helper()
	if err := m.Store.Pool.QueryRow(context.Background(), `select (select count(*) from requests),
		(select count(*) from grants), (select count(*) from credential_requests)`).Scan(&requests, &grants, &records); err != nil {
		t.Fatalf("count what the broker holds: %v", err)
	}
	return requests, grants, records
}

// auditedEnrollment reads the enrollment one audit row of kind names for grantID.
func auditedEnrollment(t *testing.T, m *Machine, kind, grantID string) string {
	t.Helper()
	var enrollment string
	if err := m.Store.Pool.QueryRow(context.Background(), `select enrollment_id::text from audit where kind=$1 and grant_id=$2`,
		kind, grantID).Scan(&enrollment); err != nil {
		t.Fatalf("read the %s audit row of grant %s: %v", kind, grantID, err)
	}
	return enrollment
}

// TestAnApprovalAfterTheMoveGrantsToTheEnrollmentTheRequestNamesNow is why ApplyDecision reads
// the enrollment off the requests row rather than off the record. A person is asked while the
// session runs on one box and answers hours later, by which time the session is on another: the
// request moved with the session (enroll.Create's adoption) and the record, which is append-only
// and cannot be re-pointed, still names the box that asked. Reading the record's own column
// answered such an approval with "the requesting enrollment is no longer live" and minted
// nothing, which is the half of the incident the issue reports.
func TestAnApprovalAfterTheMoveGrantsToTheEnrollmentTheRequestNamesNow(t *testing.T) {
	m, first, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}

	// The move, as enroll.Create makes it: the request hangs off the enrollment the second box
	// made, and the first box's enrollment has ended holding nothing.
	second, _ := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
	if _, err := m.Store.Pool.Exec(ctx, `update requests set enrollment_id=$2 where id=$1`, req.ID, second); err != nil {
		t.Fatalf("move the request to the second box: %v", err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set revoked_at=now() where id=$1`, first); err != nil {
		t.Fatalf("end the first box's enrollment: %v", err)
	}
	var named string
	if err := m.Store.Pool.QueryRow(ctx, `select enrollment_id::text from credential_requests where id=$1`, *req.RecordID).Scan(&named); err != nil {
		t.Fatalf("read the record's own enrollment: %v", err)
	}
	if named != first {
		t.Fatalf("the record names enrollment %s, want the box that asked, %s: the record is append-only, so the move cannot re-point it", named, first)
	}

	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("%s approving after the move = %+v, %v; want a grant: the approval lands on the session, which is now on the second box", approver, dec, err)
	}
	var holder string
	if err := m.Store.Pool.QueryRow(ctx, `select enrollment_id::text from grants where id=$1`, dec.GrantID).Scan(&holder); err != nil {
		t.Fatalf("read the grant's enrollment: %v", err)
	}
	if holder != second {
		t.Fatalf("grant %s hangs off enrollment %s, want the moved session's %s", dec.GrantID, holder, second)
	}
	if released, err := m.Values(ctx, dec.GrantID, second, nil); err != nil || released.Values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("the moved session reading its new grant = %v, %v; want DEEL_API_KEY=deel-v1", released.Values, err)
	}
}

// TestTheMovedSessionAsksAgainAndGetsTheSameGrantBack is what the issue's own incident comes to
// here: a session approved on one box asks for the same names, for the same lifetime, from the box
// it moved to, and reuseLiveGrant hands back the grant it already holds. Its lookup reads the
// requests row's own enrollment_id, which the move rewrote, so the broker is left holding exactly
// one request row, one grant row and one record for the session — no second card for a person to
// answer and no second grant to revoke.
func TestTheMovedSessionAsksAgainAndGetsTheSameGrantBack(t *testing.T) {
	m, first, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("%s approving = %+v, %v; want a grant", approver, dec, err)
	}

	second, secondKey := nextBox(t, m)
	moveSession(t, m, first, second)

	again, err := m.Create(ctx, second, signRequest(t, m, secondKey, "need it on the new box", "DEEL_API_KEY"), "")
	if err != nil || again.ID != req.ID || again.GrantID == nil || *again.GrantID != dec.GrantID {
		t.Fatalf("Create on the box the session moved to = %+v, %v; want request %s and its own grant %s back", again, err, req.ID, dec.GrantID)
	}
	if again.State != "granted" || !sameLifetime(again.LifetimeSeconds, req.LifetimeSeconds) {
		t.Fatalf("the grant it got back = %+v; want it granted for the lifetime the first ask signed (%v)", again, req.LifetimeSeconds)
	}
	if requests, grants, records := heldRows(t, m); requests != 1 || grants != 1 || records != 1 {
		t.Fatalf("the broker holds %d requests, %d grants and %d records; want one of each: the moved session asked for what it already holds", requests, grants, records)
	}
}

// TestSelfListsTheMovedGrantUnderTheEnrollmentThatHoldsItNow pins what GET /v1/enrollments/self
// answers after a move: LiveGrants reads the grants row's own enrollment_id, which the move
// rewrote, so the session's new box lists the moved grant and the box it left lists nothing.
func TestSelfListsTheMovedGrantUnderTheEnrollmentThatHoldsItNow(t *testing.T) {
	m, first, key, _ := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want an automatic grant", auto, err)
	}
	if listed, err := m.LiveGrants(ctx, first); err != nil || len(listed) != 1 || listed[0].ID != *auto.GrantID {
		t.Fatalf("LiveGrants(the box it asked from) = %+v, %v; want grant %s", listed, err, *auto.GrantID)
	}

	second, _ := nextBox(t, m)
	moveSession(t, m, first, second)

	listed, err := m.LiveGrants(ctx, second)
	if err != nil || len(listed) != 1 || listed[0].ID != *auto.GrantID || listed[0].RequestID != auto.ID {
		t.Fatalf("LiveGrants(the box it moved to) = %+v, %v; want grant %s of request %s", listed, err, *auto.GrantID, auto.ID)
	}
	if left, err := m.LiveGrants(ctx, first); err != nil || len(left) != 0 {
		t.Fatalf("LiveGrants(the box it left) = %+v, %v; want nothing: the grant moved off it", left, err)
	}
}

// TestTheMovedSessionFindsItsPendingRequestAndTheBoxItLeftDoesNot pins both halves of what a
// moved card does: pendingRequest reads the requests row's own enrollment_id, so the moved
// session's next ask for the same name coalesces onto the request a person is already holding
// rather than opening a second card, and the box it left, which holds nothing now, is refused
// nothing and opens a card of its own. The approver's list shows each card once.
func TestTheMovedSessionFindsItsPendingRequestAndTheBoxItLeftDoesNot(t *testing.T) {
	m, first, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}

	second, secondKey := nextBox(t, m)
	moveSession(t, m, first, second)

	again, err := m.Create(ctx, second, signRequest(t, m, secondKey, "need it on the new box", "DEEL_API_KEY"), "")
	if err != nil || again.ID != req.ID || !again.Coalesced {
		t.Fatalf("Create on the box the session moved to = %+v, %v; want it coalesced onto the pending request %s that moved with it", again, err, req.ID)
	}
	if listed, err := m.PendingForApprover(ctx, approver); err != nil || len(listed) != 1 || listed[0].RecordID != *req.RecordID {
		t.Fatalf("PendingForApprover(%s) = %+v, %v; want the moved request's one record %s", approver, listed, err, *req.RecordID)
	}

	third, err := m.Create(ctx, first, signRequest(t, m, key, "the box it left asks too", "DEEL_API_KEY"), "")
	if err != nil || third.State != "pending" || third.ID == req.ID {
		t.Fatalf("Create on the box the session left = %+v, %v; want a pending request of its own: the moved one is no longer under it", third, err)
	}
	if requests, _, records := heldRows(t, m); requests != 2 || records != 2 {
		t.Fatalf("the broker holds %d requests and %d records; want two of each: the moved card and the one the box it left opened", requests, records)
	}
}

// TestTheMovedSessionCancelsItsMovedPendingRequestAndTheBoxItLeftCannot pins Cancel's owner
// check after a move: it compares the caller against the requests row's own enrollment_id, which
// the move rewrote, so the session cancels its card from the box it runs on now and the box it
// left is ErrNotYours.
func TestTheMovedSessionCancelsItsMovedPendingRequestAndTheBoxItLeftCannot(t *testing.T) {
	m, first, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}

	second, _ := nextBox(t, m)
	moveSession(t, m, first, second)

	if err := m.Cancel(ctx, req.ID, first); !errors.Is(err, ErrNotYours) {
		t.Fatalf("the box it left cancelling request %s = %v; want ErrNotYours", req.ID, err)
	}
	if err := m.Cancel(ctx, req.ID, second); err != nil {
		t.Fatalf("the moved session cancelling its own request %s: %v", req.ID, err)
	}
	var state string
	if err := m.Store.Pool.QueryRow(ctx, `select state from requests where id=$1`, req.ID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("request %s is %q, %v; want cancelled", req.ID, state, err)
	}
}

// TestTheMovedSessionRevokesItsMovedGrantAndTheBoxItLeftCannot pins RevokeGrant's owner check
// after a move: it reads the grants row's own enrollment_id, which the move rewrote, so the
// session ends its own grant from the box it runs on now and the box it left is ErrNotYours.
func TestTheMovedSessionRevokesItsMovedGrantAndTheBoxItLeftCannot(t *testing.T) {
	m, first, key, _ := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want an automatic grant", auto, err)
	}

	second, _ := nextBox(t, m)
	moveSession(t, m, first, second)

	if err := m.RevokeGrant(ctx, *auto.GrantID, first); !errors.Is(err, ErrNotYours) {
		t.Fatalf("the box it left revoking grant %s = %v; want ErrNotYours", *auto.GrantID, err)
	}
	if err := m.RevokeGrant(ctx, *auto.GrantID, second); err != nil {
		t.Fatalf("the moved session revoking its own grant %s: %v", *auto.GrantID, err)
	}
	if listed, err := m.LiveGrants(ctx, second); err != nil || len(listed) != 0 {
		t.Fatalf("LiveGrants(the box it moved to) = %+v, %v; want nothing: the session ended the grant", listed, err)
	}
	if enrollment := auditedEnrollment(t, m, "grant.revoked", *auto.GrantID); enrollment != second {
		t.Fatalf("the grant.revoked audit row names enrollment %s; want the one the grant hung off, %s", enrollment, second)
	}
}

// TestTheApproversRevokeReachesTheMovedGrant pins the person's side of a move: the grant alice
// approved on one box is on her Live grants list under the box the session runs on now, because
// GrantsForApprover joins the enrollment on the grants row's own enrollment_id, and her revoke of
// it ends it. She is the approver and not the session's operator, so it withholds nothing
// (mayRevoke).
func TestTheApproversRevokeReachesTheMovedGrant(t *testing.T) {
	m, first, key, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "ALICE_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, otherPerson)
	}
	if got := recordApprover(t, m, req); got != otherPerson {
		t.Fatalf("approver = %q, want %s", got, otherPerson)
	}
	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, otherPerson)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("%s approving = %+v, %v; want a grant", otherPerson, dec, err)
	}

	second, _ := nextBox(t, m)
	moveSession(t, m, first, second)

	listed, err := m.GrantsForApprover(ctx, otherPerson)
	if err != nil || len(listed) != 1 || listed[0].GrantID != dec.GrantID || listed[0].Granted != policy.Approval {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the one grant %s she approved", otherPerson, listed, err, dec.GrantID)
	}
	if listed[0].Enrollment.RuntimeID != "box-b-"+t.Name() {
		t.Fatalf("the listed grant names box %q; want the box the session moved to, box-b-%s", listed[0].Enrollment.RuntimeID, t.Name())
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, otherPerson, record.HumanActor(otherPerson)); err != nil {
		t.Fatalf("%s revoking the moved grant: %v", otherPerson, err)
	}
	if actor, withheld := revokedAudit(t, m, dec.GrantID); actor != record.HumanActor(otherPerson) || withheld != nil {
		t.Fatalf("grant.revoked audit = %s withheld %v; want human:%s withholding nothing: the session is not hers", actor, withheld, otherPerson)
	}
	if live, err := m.LiveGrants(ctx, second); err != nil || len(live) != 0 {
		t.Fatalf("LiveGrants(the box it moved to) = %+v, %v; want nothing: the approver ended the grant", live, err)
	}
}

// TestTheOperatorsRevokeAfterTheMoveWithholdsAgainstTheBoxTheGrantHangsOffNow pins where an
// operator's withhold lands once a session has moved: RevokeByApprover reads the operator off the
// enrollment the grants row names now, and withhold writes against that same enrollment, so the
// name is withheld from the box the session runs on and its next ask for it waits on a person.
func TestTheOperatorsRevokeAfterTheMoveWithholdsAgainstTheBoxTheGrantHangsOffNow(t *testing.T) {
	m, first, key, operator := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want an automatic grant", auto, err)
	}

	second, secondKey := nextBox(t, m)
	moveSession(t, m, first, second)

	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator, record.HumanActor(operator)); err != nil {
		t.Fatalf("%s revoking the moved grant: %v", operator, err)
	}
	if names := withheldFrom(t, m, second); !slices.Equal(names, []string{"AUTO_TOKEN"}) {
		t.Fatalf("withheld from the box it moved to = %v; want [AUTO_TOKEN]: the grant hangs off that enrollment now", names)
	}
	if names := withheldFrom(t, m, first); len(names) != 0 {
		t.Fatalf("withheld from the box it left = %v; want nothing: the session is not there", names)
	}
	again, err := m.Create(ctx, second, signRequest(t, m, secondKey, "need it again", "AUTO_TOKEN"), "")
	if err != nil || again.State != "pending" || again.GrantID != nil {
		t.Fatalf("Create after the withhold = %+v, %v; want an approval request", again, err)
	}
}

// TestAWithholdMadeBeforeTheMoveStillWithholdsAfterIt pins the other order, which is the one the
// move itself must not undo: the operator takes the name back while the session is on one box, the
// session moves, and its ask from the new box is an approval request rather than the secret.
// requester reads the withheld names by the enrollment's own id, and the move carried the rows
// there.
func TestAWithholdMadeBeforeTheMoveStillWithholdsAfterIt(t *testing.T) {
	m, first, key, operator := newFixture(t)
	ctx := context.Background()
	auto, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want an automatic grant", auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator, record.HumanActor(operator)); err != nil {
		t.Fatalf("%s revoking the grant: %v", operator, err)
	}
	if names := withheldFrom(t, m, first); !slices.Equal(names, []string{"AUTO_TOKEN"}) {
		t.Fatalf("withheld from the box it asked from = %v; want [AUTO_TOKEN]", names)
	}

	second, secondKey := nextBox(t, m)
	moveSession(t, m, first, second)

	if names := withheldFrom(t, m, second); !slices.Equal(names, []string{"AUTO_TOKEN"}) {
		t.Fatalf("withheld from the box it moved to = %v; want [AUTO_TOKEN]: a withhold follows the session", names)
	}
	if names := withheldFrom(t, m, first); len(names) != 0 {
		t.Fatalf("withheld from the box it left = %v; want nothing: the row moved", names)
	}
	again, err := m.Create(ctx, second, signRequest(t, m, secondKey, "need it on the new box", "AUTO_TOKEN"), "")
	if err != nil || again.State != "pending" || again.GrantID != nil {
		t.Fatalf("Create on the box it moved to = %+v, %v; want an approval request: the withhold moved with the session", again, err)
	}
	if got := recordApprover(t, m, again); got != operator {
		t.Fatalf("approver = %q, want %s", got, operator)
	}
}
