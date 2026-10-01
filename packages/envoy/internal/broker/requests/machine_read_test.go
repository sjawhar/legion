package requests

import (
	"context"
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
	dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, "Alice")
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice): %+v %v", dec, err)
	}
	for _, viewer := range []string{operator, "alice"} {
		grants, err := m.GrantsForApprover(ctx, viewer)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", viewer, err)
		}
		if len(grants) != 1 || grants[0].GrantID != dec.GrantID || grants[0].Approver != "alice" {
			t.Fatalf("GrantsForApprover(%s) = %+v, want grant %s approved by alice", viewer, grants, dec.GrantID)
		}
	}
}
