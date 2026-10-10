package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestGrantsForApproverNamesWhoApproved pins that the approver list names each grant's approver:
// the operator's list holds a grant another login approved on their enrollment, and must say that
// login decided it, not leave the operator to assume they did.
func TestGrantsForApproverNamesWhoApproved(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "ALICE_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(pending) = %+v, %v", pending, err)
	}
	dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, "Alice@Example.com")
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice): %+v %v", dec, err)
	}
	for _, viewer := range []string{operator, otherPerson} {
		grants, err := m.GrantsForApprover(ctx, viewer)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", viewer, err)
		}
		if len(grants) != 1 || grants[0].GrantID != dec.GrantID || grants[0].Approver != otherPerson {
			t.Fatalf("GrantsForApprover(%s) = %+v, want grant %s approved by %s", viewer, grants, dec.GrantID, otherPerson)
		}
	}
}

// TestEachSlotOfAPodSignsItsOwnRecordAndListsItsOwnGrant pins the slot through request creation,
// the record read and the approver's grant list: a slotless pod enrollment's record is stored with
// the three-field enrollment line and a slotted one's with its slot as a fourth field, both decide
// and release through the whole chain, and each read names the slot it came from.
func TestEachSlotOfAPodSignsItsOwnRecordAndListsItsOwnGrant(t *testing.T) {
	m, _, _, approver := newFixture(t)
	ctx := context.Background()
	const podUID = "pod-uid-slots"
	type pod struct {
		slot, enrollmentID, recordID, grantID string
	}
	var pods []pod
	for _, slot := range []string{"", "implementer-g3", "reviewer-g3"} {
		id, key := newEnrollment(t, m.Store, "pod", podUID, nil, new("system:serviceaccount:legion:worker"))
		if _, err := m.Store.Pool.Exec(ctx, `update enrollments set slot=$2 where id=$1`, id, slot); err != nil {
			t.Fatalf("set slot %q: %v", slot, err)
		}
		req, err := m.Create(ctx, id, signRequest(t, m, key, "role "+slot, "DEEL_API_KEY"), "")
		if err != nil || req.State != "pending" || req.RecordID == nil {
			t.Fatalf("Create(slot %q) = %+v, %v", slot, req, err)
		}
		var body string
		if err := m.Store.Pool.QueryRow(ctx, `select body from credential_requests where id=$1`, *req.RecordID).Scan(&body); err != nil {
			t.Fatal(err)
		}
		line := "\nenrollment: pod\t" + podUID + "\t-\n"
		if slot != "" {
			line = "\nenrollment: pod\t" + podUID + "\t-\t" + slot + "\n"
		}
		if !strings.Contains(body, line) {
			t.Fatalf("stored record of slot %q:\n%q\nwant the enrollment line %q", slot, body, line)
		}
		detail, err := m.ReadRecord(ctx, *req.RecordID)
		if err != nil || detail.Enrollment == nil || detail.Enrollment.Slot != slot || detail.Enrollment.RuntimeID != podUID {
			t.Fatalf("ReadRecord(slot %q) = %+v, %v", slot, detail.Enrollment, err)
		}
		dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
		if err != nil || dec.GrantID == "" {
			t.Fatalf("ApplyDecision(slot %q) = %+v, %v", slot, dec, err)
		}
		values, _, err := m.Values(ctx, dec.GrantID, id)
		if err != nil || values["DEEL_API_KEY"] != "deel-v1" {
			t.Fatalf("Values(slot %q) = %v, %v; want the chain to verify and release", slot, values, err)
		}
		pods = append(pods, pod{slot: slot, enrollmentID: id, recordID: *req.RecordID, grantID: dec.GrantID})
	}
	grants, err := m.GrantsForApprover(ctx, approver)
	if err != nil || len(grants) != len(pods) {
		t.Fatalf("GrantsForApprover = %+v, %v; want one grant per slot", grants, err)
	}
	for _, g := range grants {
		for _, p := range pods {
			if p.grantID == g.GrantID && (g.Enrollment.Slot != p.slot || g.Enrollment.RuntimeID != podUID) {
				t.Fatalf("listed grant %s = %+v, want pod %s in slot %q", g.GrantID, g.Enrollment, podUID, p.slot)
			}
		}
	}
}

// TestPendingForApproverTerminalEventsLiteralMatchesTheList pins pendingForApproverQuery's
// literal terminal-event predicate against record.TerminalEventNames(), so a later addition or
// rename of a terminal event changes both together: the literal is what lets Postgres recognize
// credential_request_events' partial index (pendingForApproverQuery's own doc comment says why a
// bound parameter cannot).
func TestPendingForApproverTerminalEventsLiteralMatchesTheList(t *testing.T) {
	names := record.TerminalEventNames()
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("'%s'", name)
	}
	want := "event in (" + strings.Join(quoted, ",") + ")"
	if !strings.Contains(pendingForApproverQuery, want) {
		t.Fatalf("pendingForApproverQuery does not spell record.TerminalEventNames() as %q; the query and the list have drifted:\n%s", want, pendingForApproverQuery)
	}
}

// TestPendingForApproverAvoidsSequentialScans pins PendingForApprover's query plan the way
// Dispatch's api.TestListIssueAsksOpenQueryUsesAsksOpenIndex pins queryOwnerAsks's: Postgres
// automatically switches a prepared statement from a per-execution custom plan (built for that
// call's actual bound values) to a cached generic plan starting on its 6th execution, which an
// empty test table cannot trigger organically, so force_generic_plan pins every execution
// (including the first) to that shape instead of relying on the cost-based switch.
// enable_seqscan = off then makes a sequential scan of either table the query touches the
// planner's last resort rather than an ordinary choice it might make anyway for a near-empty
// table: with both set, this test fails the moment either index path the query depends on stops
// applying — credential_requests_approver (migration 0011) for the approver filter, or
// credential_request_decision (migration 0005) for the launcher_credential branch's NOT EXISTS.
func TestPendingForApproverAvoidsSequentialScans(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("force generic plan mode: %v", err)
	}
	if _, err := tx.Exec(ctx, "prepare pending_query (text, text) as "+pendingForApproverQuery); err != nil {
		t.Fatalf("prepare pending query: %v", err)
	}
	execute := fmt.Sprintf("execute pending_query('nobody@example.com', '%s')", record.AnyoneApprover)
	for i := range 6 {
		if _, err := tx.Exec(ctx, execute); err != nil {
			t.Fatalf("execute pending query %d: %v", i, err)
		}
	}
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+execute).Scan(&planJSON); err != nil {
		t.Fatalf("explain execute pending query: %v", err)
	}
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil {
		t.Fatalf("decode explain output: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain plans = %#v, want one plan", plans)
	}
	for _, relation := range []string{"credential_requests", "credential_request_events"} {
		if explaintest.SeqScansRelation(t, plans[0].Plan, relation) {
			t.Fatalf("pending query generic plan sequentially scans %s; want an index scan:\n%s", relation, planJSON)
		}
	}
}

// TestReadRecordAndPendingNameBothSessionIDsWhenTheyDiffer pins LEGION-587: an agent_secret
// record's session names the enrollment's own session_id (stated at enroll time) and the
// request's own override (stated in its body at Create) independently, through both reads that
// answer one (ReadRecord and PendingForApprover).
func TestReadRecordAndPendingNameBothSessionIDsWhenTheyDiffer(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set session_id=$2 where id=$1`, enr, "sess-enrollment-1"); err != nil {
		t.Fatalf("set enrollment session_id: %v", err)
	}
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "sess-request-2")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create = %+v, %v", pending, err)
	}
	want := Session{Request: "sess-request-2", Enrollment: "sess-enrollment-1"}

	detail, err := m.ReadRecord(ctx, *pending.RecordID)
	if err != nil || detail.Session != want {
		t.Fatalf("ReadRecord.Session = %+v, %v, want %+v", detail.Session, err, want)
	}

	rows, err := m.PendingForApprover(ctx, approver)
	if err != nil || len(rows) != 1 || rows[0].Session != want {
		t.Fatalf("PendingForApprover = %+v, %v, want one row naming %+v", rows, err, want)
	}
}

// TestReadRecordKeepsTheEnrollmentSessionIDAfterRevocation pins that an id outlives its
// enrollment's revocation: enrollments.session_id is never cleared when revoked_at is set, so a
// decided record's own read still names the session that enrolled.
func TestReadRecordKeepsTheEnrollmentSessionIDAfterRevocation(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set session_id=$2 where id=$1`, enr, "sess-enrollment-revoked"); err != nil {
		t.Fatalf("set enrollment session_id: %v", err)
	}
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create = %+v, %v", pending, err)
	}
	dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v", dec, err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set revoked_at=now() where id=$1`, enr); err != nil {
		t.Fatalf("revoke enrollment: %v", err)
	}

	detail, err := m.ReadRecord(ctx, *pending.RecordID)
	if err != nil || detail.Session.Enrollment != "sess-enrollment-revoked" {
		t.Fatalf("ReadRecord.Session after revocation = %+v, %v, want enrollment sess-enrollment-revoked", detail.Session, err)
	}
}

// TestReadRecordNamesNoSessionForAMachineLogin pins that a launcher_credential record - which has
// no request row and no requesting enrollment at all - always answers both session ids empty.
func TestReadRecordNamesNoSessionForAMachineLogin(t *testing.T) {
	m, _, key, _ := newFixture(t)
	ctx := context.Background()
	body := record.Body{
		Approver:        "alice",
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		ExpiresAt:       time.Now().Add(time.Hour),
		LifetimeSeconds: 3600,
		Request:         signRequest(t, m, key, "login", "example-host"),
		RulesVersion:    "rules-v1",
	}
	recordID := body.ID()
	if _, err := m.Store.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, expires_at) values ($1,$2,'launcher_credential',$3,$4)`,
		recordID, body.Canonical(), body.Approver, body.ExpiresAt); err != nil {
		t.Fatalf("insert machine login record: %v", err)
	}
	detail, err := m.ReadRecord(ctx, recordID)
	if err != nil || detail.Session != (Session{}) {
		t.Fatalf("ReadRecord(machine login).Session = %+v, %v, want both empty", detail.Session, err)
	}
}
