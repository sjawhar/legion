package requests

import (
	"context"
	"strings"
	"testing"
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
