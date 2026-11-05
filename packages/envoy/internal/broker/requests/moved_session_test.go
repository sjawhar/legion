// moved_session_test.go holds what a session's move to another box asks of this package: an
// approval given after the move lands on the enrollment the session proves then
// (dispatch://INFRA-1167).
package requests

import (
	"context"
	"testing"
)

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
	if values, _, err := m.Values(ctx, dec.GrantID, second, nil); err != nil || values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("the moved session reading its new grant = %v, %v; want DEEL_API_KEY=deel-v1", values, err)
	}
}
