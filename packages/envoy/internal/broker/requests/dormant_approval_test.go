// dormant_approval_test.go holds what a person's approval owes a session whose box has gone: an
// approval of a request the session's ended enrollment still holds grants onto that dormant row,
// so the enrollment the session enrols on next adopts the grant rather than the session asking
// again; an approval of a request on an enrollment ended for good is denied as it always was; and
// an approval that races the adoption grants to the enrollment the request names now
// (dispatch://INFRA-1167).
package requests

import (
	"context"
	"errors"
	"testing"
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
