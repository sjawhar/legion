// dormant_approval_test.go holds what a person's approval owes a session whose box has gone. An
// approval of a request the session's ended enrollment still holds grants onto that dormant row,
// so the enrollment the session enrols on next adopts the grant rather than the session asking
// again; an approval on a row whose lease has lapsed whilst no end is written against it yet
// grants too, when the end the sweep is about to write would leave that row dormant; an approval
// of a request on an enrollment ended for good is denied as it always was; and an approval that
// races the adoption grants to the enrollment the request names now (dispatch://INFRA-1167).
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
)

// endDormant ends enrollment id the way enroll.endEnrollment ends one that may carry: the row is
// revoked and marked dormant, and the grant or pending request hanging off it is left standing
// for the next enrollment of that session name under the same principal to adopt.
func endDormant(t *testing.T, m *Machine, id string) {
	t.Helper()
	if _, err := m.Store.Pool.Exec(context.Background(),
		`update enrollments set revoked_at=now(), dormant_since=now() where id=$1`, id); err != nil {
		t.Fatalf("end enrollment %s dormant: %v", id, err)
	}
}

// grantHolder is the enrollment grant grantID hangs off.
func grantHolder(t *testing.T, m *Machine, grantID string) string {
	t.Helper()
	var holder string
	if err := m.Store.Pool.QueryRow(context.Background(), `select enrollment_id::text from grants where id=$1`, grantID).Scan(&holder); err != nil {
		t.Fatalf("read the enrollment of grant %s: %v", grantID, err)
	}
	return holder
}

// TestAnApprovalWhileTheEnrollmentIsDormantGrantsOntoTheDormantRow is the window the carry is
// for: a session files a request, its box is replaced or its role suspended before anyone
// answers, and the person approves while the session is between enrolments. The enrollment that
// asked is ended and dormant — revoked_at set with dormant_since not null, what endEnrollment
// writes for an end that may carry — and it still holds the pending request, so the approval
// hangs the grant off it. Refusing it because the row is revoked would record the person's answer
// as approved, move the request to denied, and leave the session asking again from its next box
// for something somebody already approved.
func TestAnApprovalWhileTheEnrollmentIsDormantGrantsOntoTheDormantRow(t *testing.T) {
	m, first, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}
	endDormant(t, m, first)

	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("%s approving while the enrollment is dormant = %+v, %v; want a grant on the dormant row", approver, dec, err)
	}
	if holder := grantHolder(t, m, dec.GrantID); holder != first {
		t.Fatalf("grant %s hangs off enrollment %s, want the dormant %s the request still names", dec.GrantID, holder, first)
	}

	// The session comes back on another box, which adopts what the dormant row held (moveSession
	// moves the rows as enroll.Create's own adoption does), and reads the grant it was given
	// while it had no box at all.
	second, _ := nextBox(t, m)
	moveSession(t, m, first, second)
	if holder := grantHolder(t, m, dec.GrantID); holder != second {
		t.Fatalf("after the adoption the grant hangs off %s, want the box the session runs on now, %s", holder, second)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, second, nil); err != nil || values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("the session reading the grant from the box it enrolled on next = %v, %v; want DEEL_API_KEY=deel-v1", values, err)
	}
}

// TestAnApprovalAfterTheEnrollmentEndedForGoodIsStillDenied is the other side of that branch: a
// row revoked with nothing left to carry — dormant_since null, what endEnrollment writes for a
// person's revoke of the machine login or the sign-in, and what the carry sweep leaves behind —
// takes no grant. The approval is still recorded as the person's answer, the request moves to
// denied with the detail the approval page shows, and nothing is minted.
func TestAnApprovalAfterTheEnrollmentEndedForGoodIsStillDenied(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set revoked_at=now() where id=$1`, enr); err != nil {
		t.Fatalf("end the enrollment for good: %v", err)
	}

	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.State != "denied" || dec.GrantID != "" {
		t.Fatalf("%s approving on an enrollment ended for good = %+v, %v; want it denied with no grant", approver, dec, err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.State != "denied" || got.Detail == nil || *got.Detail != "the requesting enrollment is no longer live" {
		t.Fatalf("Get = %+v, %v; want it denied with the detail the approval page shows", got, err)
	}
	var approvals int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_request_events where record_id=$1 and event='approved'`,
		*req.RecordID).Scan(&approvals); err != nil || approvals != 1 {
		t.Fatalf("%d approved events on the record, %v; want the person's answer recorded once", approvals, err)
	}
	if _, grants, _ := heldRows(t, m); grants != 0 {
		t.Fatalf("%d grant rows after the denial, want none", grants)
	}
}

// TestAnApprovalRacingTheAdoptionGrantsToTheEnrollmentTheRequestNamesNow is the race between the
// two reads one attempt makes. The attempt locks the enrollment the request names and only then
// locks the request, the order every enrollment-then-request writer here takes; an adoption that
// commits in between holds the predecessor for update and moves the request off it (enroll.adopt)
// without updating the predecessor's own row, so the share lock is granted with no recheck and
// the first read still names the box the session has left. Granting on it hangs the grant off
// that box: the moved session reads ErrNotYours and the box it left can read the secret. The
// attempt is abandoned and made again instead, and the grant lands on the enrollment the request
// names now.
//
// Winning that window against a real adoption is a matter of microseconds, so the test drives it
// through Machine.testAdoptionHook, an unexported test-only seam documented on the struct
// (enroll.Service.testConflictHook is the precedent), which runs exactly where the adoption's
// commit lands.
func TestAnApprovalRacingTheAdoptionGrantsToTheEnrollmentTheRequestNamesNow(t *testing.T) {
	m, first, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}
	second, _ := nextBox(t, m)

	var hookCalls int
	m.testAdoptionHook = func() {
		hookCalls++
		if hookCalls > 1 {
			return // the request moved during the first attempt; there is nothing left to race.
		}
		moveSession(t, m, first, second)
	}

	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("%s approving across the adoption = %+v, %v; want a grant", approver, dec, err)
	}
	if holder := grantHolder(t, m, dec.GrantID); holder != second {
		t.Fatalf("grant %s hangs off enrollment %s, want the enrollment the request names now, %s", dec.GrantID, holder, second)
	}
	if hookCalls != 2 {
		t.Fatalf("the attempt ran %d times; want two: one abandoned when the request moved and one that granted", hookCalls)
	}
	if audited := auditedEnrollment(t, m, "request.granted", dec.GrantID); audited != second {
		t.Fatalf("the request.granted audit row names enrollment %s, want %s", audited, second)
	}
	if requests, grants, records := heldRows(t, m); requests != 1 || grants != 1 || records != 1 {
		t.Fatalf("the broker holds %d requests, %d grants and %d records; want one of each: the abandoned attempt wrote nothing",
			requests, grants, records)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, second, nil); err != nil || values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("the moved session reading the grant = %v, %v; want DEEL_API_KEY=deel-v1", values, err)
	}
	if _, _, err := m.Values(ctx, dec.GrantID, first, nil); !errors.Is(err, ErrNotYours) {
		t.Fatalf("the box the session left reading the grant = %v, want ErrNotYours", err)
	}
}

// lapseLease puts enrollment id's lease an hour in the past and writes no end against the row:
// the state a box is left in between its lease lapsing and enroll.Service.EndLapsed finding it,
// which is as long as one sweep (BROKER_SWEEP_SECONDS).
func lapseLease(t *testing.T, m *Machine, id string) {
	t.Helper()
	if _, err := m.Store.Pool.Exec(context.Background(),
		`update enrollments set lease_expires_at = now() - interval '1 hour' where id=$1`, id); err != nil {
		t.Fatalf("lapse the lease of enrollment %s: %v", id, err)
	}
}

// nameTheSession writes onto enrollment id what enroll.Service.Create writes for a named
// session: the session's name, and the principal that session belongs to (enroll.carryPrincipal
// spells it). The pair is what a later enrolment adopts by, so it is also what decides whether
// an end of the box leaves the row dormant. Either may be empty here, which writes null, for the
// rows no end of the box could leave dormant.
func nameTheSession(t *testing.T, m *Machine, id, session, principal string) {
	t.Helper()
	if _, err := m.Store.Pool.Exec(context.Background(),
		`update enrollments set session_id=$2, carry_principal=$3 where id=$1`,
		id, nullable(session), nullable(principal)); err != nil {
		t.Fatalf("name the session of enrollment %s: %v", id, err)
	}
}

// sweptLapsed runs the sweep that ends every enrollment whose lease has lapsed
// (enroll.Service.EndLapsed, which requests.Sweeper ticks), keyed by enrollment id. Each row's
// Dormant is enroll.endEnrollment's own answer about that row: the end left what the row held
// standing for the session's next box rather than revoking it.
func sweptLapsed(t *testing.T, m *Machine) map[string]enroll.LapsedEnrollment {
	t.Helper()
	ended, err := (&enroll.Service{Store: m.Store}).EndLapsed(context.Background())
	if err != nil {
		t.Fatalf("end the lapsed enrollments: %v", err)
	}
	swept := make(map[string]enroll.LapsedEnrollment, len(ended))
	for _, e := range ended {
		swept[e.ID] = e
	}
	return swept
}

// enrollmentEnd is what an end wrote on enrollment id: whether it is revoked at all, and whether
// it is dormant, which is revoked with what it held left standing for the session's next box.
func enrollmentEnd(t *testing.T, m *Machine, id string) (revoked, dormant bool) {
	t.Helper()
	if err := m.Store.Pool.QueryRow(context.Background(),
		`select revoked_at is not null, dormant_since is not null from enrollments where id=$1`, id).
		Scan(&revoked, &dormant); err != nil {
		t.Fatalf("read the end of enrollment %s: %v", id, err)
	}
	return revoked, dormant
}

// grantRevoked reports whether grant grantID has been revoked.
func grantRevoked(t *testing.T, m *Machine, grantID string) bool {
	t.Helper()
	var revoked bool
	if err := m.Store.Pool.QueryRow(context.Background(),
		`select revoked_at is not null from grants where id=$1`, grantID).Scan(&revoked); err != nil {
		t.Fatalf("read whether grant %s is revoked: %v", grantID, err)
	}
	return revoked
}

// TestAnApprovalOnALapsedRowTheSweepHasNotEndedGrantsAndTheSweepThenCarriesIt is the window
// between a box losing its lease and enroll.Service.EndLapsed ending that row, which is as long
// as one sweep. The row is not revoked and its lease is not live, and the end the sweep is about
// to write would leave it dormant: it names a session, it records the principal that session
// belongs to, and it holds the pending request. So the approval grants onto it. Denying here
// while granting the same approval a second later — once the sweep has written dormant_since on
// the same row holding the same rows — would make the sweep's timing the person's answer.
//
// The sweep then runs and the test follows the grant the rest of the way: the row is dormant
// with the grant standing, and the session's next enrolment of the same name adopts it
// (moveSession moves the rows as enroll.Create's own adoption does).
func TestAnApprovalOnALapsedRowTheSweepHasNotEndedGrantsAndTheSweepThenCarriesIt(t *testing.T) {
	m, first, key, approver := newFixture(t)
	ctx := context.Background()
	nameTheSession(t, m, first, "sess-lapsed-carry", exchange.PersonPrincipal(fixtureOperator))
	req, err := m.Create(ctx, first, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}
	lapseLease(t, m, first)

	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("%s approving while the lease has lapsed and no end is written yet = %+v, %v; want a grant on the row the sweep will leave dormant",
			approver, dec, err)
	}
	if holder := grantHolder(t, m, dec.GrantID); holder != first {
		t.Fatalf("grant %s hangs off enrollment %s, want the lapsed %s the request still names", dec.GrantID, holder, first)
	}

	// The sweep catches up. It ends the row as a lapse ends one, and what the approval left on
	// the row is what makes that end dormant rather than terminal.
	ended, swept := sweptLapsed(t, m)[first]
	if !swept || !ended.Dormant || ended.GrantsRevoked != 0 || ended.RequestsCancelled != 0 {
		t.Fatalf("EndLapsed on %s = %+v (it swept the row: %t); want it ended dormant with nothing revoked and nothing cancelled",
			first, ended, swept)
	}
	if revoked, dormant := enrollmentEnd(t, m, first); !revoked || !dormant {
		t.Fatalf("the swept row is revoked=%t dormant=%t; want both: ended, holding the grant for the session's next box", revoked, dormant)
	}
	if grantRevoked(t, m, dec.GrantID) {
		t.Fatalf("the sweep revoked grant %s; want it standing for the session to take", dec.GrantID)
	}

	// The session comes back on another box, which adopts the grant the approval left on the
	// dormant row, and reads the secret it was approved for while it had no box at all.
	second, _ := nextBox(t, m)
	moveSession(t, m, first, second)
	if holder := grantHolder(t, m, dec.GrantID); holder != second {
		t.Fatalf("after the adoption the grant hangs off %s, want the box the session runs on now, %s", holder, second)
	}
	if values, _, err := m.Values(ctx, dec.GrantID, second, nil); err != nil || values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("the session reading the grant from the box it enrolled on next = %v, %v; want DEEL_API_KEY=deel-v1", values, err)
	}
}

// TestAnApprovalOnALapsedRowWithNoPrincipalIsStillDenied is the other side of that window. The
// row names a session but records no principal, which is every enrollment that was live when
// migration 0027 applied and every later one the broker could name no principal for
// (enroll.carryPrincipal), so nothing could ever adopt what it holds and the end the sweep is
// about to write revokes rather than carries. The approval is recorded as the person's answer
// and then denied, as it always was.
//
// The row is left holding a second request nobody answers, so the sweep's answer afterwards
// turns on the missing principal rather than on the denial having emptied the row.
func TestAnApprovalOnALapsedRowWithNoPrincipalIsStillDenied(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	nameTheSession(t, m, enr, "sess-lapsed-no-principal", "")
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record %s decides", req, err, approver)
	}
	undecided, err := m.Create(ctx, enr, signRequest(t, m, key, "this one too", "SHARED_KEY"), "")
	if err != nil || undecided.State != "pending" {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want a second pending request nobody answers", undecided, err)
	}
	lapseLease(t, m, enr)

	dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
	if err != nil || dec.State != "denied" || dec.GrantID != "" {
		t.Fatalf("%s approving on a lapsed row no end could leave dormant = %+v, %v; want it denied with no grant", approver, dec, err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.State != "denied" || got.Detail == nil || *got.Detail != "the requesting enrollment is no longer live" {
		t.Fatalf("Get = %+v, %v; want it denied with the detail the approval page shows", got, err)
	}
	if _, grants, _ := heldRows(t, m); grants != 0 {
		t.Fatalf("%d grant rows after the denial, want none", grants)
	}

	// The sweep ends the row for good, which is what the denial answered to: it revokes nothing,
	// since there is nothing to revoke, and cancels the request still pending on it.
	ended, swept := sweptLapsed(t, m)[enr]
	if !swept || ended.Dormant || ended.RequestsCancelled != 1 {
		t.Fatalf("EndLapsed on %s = %+v (it swept the row: %t); want it ended for good, cancelling the one request still pending",
			enr, ended, swept)
	}
	if revoked, dormant := enrollmentEnd(t, m, enr); !revoked || dormant {
		t.Fatalf("the swept row is revoked=%t dormant=%t; want revoked and not dormant: no session could take what it held", revoked, dormant)
	}
}

// TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes is what holds mayGrant's third case
// and enroll.endEnrollment's dormancy probe together. They are one rule written twice, because
// enroll's copy is unexported, and the whole of the third case is that an approval on a lapsed
// row is granted exactly when the end that follows would leave that row dormant. Were the two to
// drift, an approval would be granted onto a row the sweep then revokes, or denied on a row the
// sweep then carries.
//
// So both are run over every shape of row a lapse can find — a session name or none, a principal
// or none, and holding a pending request, a live grant or nothing — and each case states the
// answer it wants as well, so the test says what the rule is rather than only that the two
// copies agree.
func TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes(t *testing.T) {
	m, _, _, _ := newFixture(t)
	ctx := context.Background()
	shapes := []struct {
		name              string
		named, principled bool
	}{
		{"a session name and a principal", true, true},
		{"a session name and no principal", true, false},
		{"a principal and no session name", false, true},
		{"neither", false, false},
	}
	holdings := []struct {
		name  string
		holds bool
		fill  func(t *testing.T, id string, key *ecdsa.PrivateKey)
	}{
		{"holding nothing", false, func(*testing.T, string, *ecdsa.PrivateKey) {}},
		{"holding a pending request", true, func(t *testing.T, id string, key *ecdsa.PrivateKey) {
			req, err := m.Create(ctx, id, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
			if err != nil || req.State != "pending" {
				t.Fatalf("Create a pending request on %s = %+v, %v", id, req, err)
			}
		}},
		{"holding a live grant", true, func(t *testing.T, id string, key *ecdsa.PrivateKey) {
			req, err := m.Create(ctx, id, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "")
			if err != nil || req.GrantID == nil {
				t.Fatalf("Create an automatic grant on %s = %+v, %v", id, req, err)
			}
		}},
	}

	type lapsedRow struct {
		name     string
		id       string
		mayGrant bool
		want     bool
	}
	var rows []lapsedRow
	for _, shape := range shapes {
		for _, holding := range holdings {
			box := fmt.Sprintf("box-%d-%s", len(rows), t.Name())
			id, key := newEnrollment(t, m.Store, "session", box, new(fixtureOperator), nil)
			session := ""
			if shape.named {
				session = "sess-" + box
			}
			principal := ""
			if shape.principled {
				principal = exchange.PersonPrincipal(fixtureOperator)
			}
			nameTheSession(t, m, id, session, principal)
			holding.fill(t, id, key)
			lapseLease(t, m, id)
			rows = append(rows, lapsedRow{
				name:     shape.name + ", " + holding.name,
				id:       id,
				mayGrant: wouldGoDormant(t, m, id),
				want:     shape.named && shape.principled && holding.holds,
			})
		}
	}

	// One sweep ends them all, and reports per row what endEnrollment did to it.
	swept := sweptLapsed(t, m)
	for _, row := range rows {
		ended, found := swept[row.id]
		if !found {
			t.Fatalf("%s: the sweep did not end enrollment %s, whose lease has lapsed", row.name, row.id)
		}
		if row.mayGrant != row.want {
			t.Errorf("%s: an approval on the lapsed row is granted = %t, want %t", row.name, row.mayGrant, row.want)
		}
		if ended.Dormant != row.mayGrant {
			t.Errorf("%s: an approval on the lapsed row is granted = %t while the end that followed left it dormant = %t; the two rules have drifted",
				row.name, row.mayGrant, ended.Dormant)
		}
	}
}

// wouldGoDormant runs this package's own enrollmentWouldGoDormant over one row: mayGrant's third
// case, which is what decides whether an approval lands on a row whose lease has lapsed.
func wouldGoDormant(t *testing.T, m *Machine, id string) bool {
	t.Helper()
	var dormant bool
	if err := m.Store.Pool.QueryRow(context.Background(),
		`select `+enrollmentWouldGoDormant+` from enrollments e where e.id=$1`, id).Scan(&dormant); err != nil {
		t.Fatalf("read whether enrollment %s would go dormant: %v", id, err)
	}
	return dormant
}
