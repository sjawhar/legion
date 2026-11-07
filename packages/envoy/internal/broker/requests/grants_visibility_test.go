// grants_visibility_test.go pins who sees a grant on the Live grants page: a grant a pod took
// without asking is on every signed-in person's list, because it is on nobody's otherwise, while
// every grant that already belongs to a person stays that person's alone.
package requests

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// handLaunchedAccount is the ServiceAccount of a pod an operator launches by hand: it holds a
// machine login of its own, BROKER_SERVICES binds it for no service's pods, and so the pod reaches
// the shared agent-tier secrets and no service's.
const handLaunchedAccount = "system:serviceaccount:legion:k8s-session"

// TestAPodsAutomaticGrantIsOnEveryPersonsGrantList: a pod takes a shared agent-tier secret without
// asking, so its grant has no approver, and a pod acts for no person, so its enrollment has no
// operator. Such a grant matched neither half of the Live-grants filter and was on nobody's page
// at all: nobody could see what a pod had taken. It is now on every signed-in person's, the people
// who already list and revoke the service login behind it. The operator's own automatic grant is
// unmoved: theirs, and nobody else's.
func TestAPodsAutomaticGrantIsOnEveryPersonsGrantList(t *testing.T) {
	m, session, sessionKey, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()

	const podUID = "pod-uid-hand-launched"
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, handLaunchedAccount, podUID)
	auto, err := m.Create(ctx, pod, signRequest(t, m, podKey, "the pod needs it", sharedToken), "")
	if err != nil || auto.State != "granted" || auto.GrantID == nil || auto.RecordID != nil {
		t.Fatalf("Create(the pod's request) = %+v, %v; want an automatic grant", auto, err)
	}
	own, err := m.Create(ctx, session, signRequest(t, m, sessionKey, "the session needs it", "AUTO_TOKEN"), "")
	if err != nil || own.State != "granted" || own.GrantID == nil {
		t.Fatalf("Create(the operator's own request) = %+v, %v; want an automatic grant", own, err)
	}

	for _, viewer := range []string{operator, otherPerson, carol} {
		listed, err := m.GrantsForApprover(ctx, viewer)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", viewer, err)
		}
		var podGrants, ownGrants int
		for _, g := range listed {
			switch g.GrantID {
			case *auto.GrantID:
				podGrants++
				if g.Granted != policy.Automatic || g.Approver != "" || g.RecordID != nil ||
					g.Enrollment.Kind != "pod" || g.Enrollment.RuntimeID != podUID || g.Enrollment.Operator != "" {
					t.Fatalf("the pod's grant as %s reads %+v, want an automatic grant of pod %s with no approver and no operator", viewer, g, podUID)
				}
			case *own.GrantID:
				ownGrants++
			default:
				t.Fatalf("GrantsForApprover(%s) listed an unexpected grant %+v", viewer, g)
			}
		}
		wantOwn := 0
		if viewer == operator {
			wantOwn = 1
		}
		if podGrants != 1 || ownGrants != wantOwn {
			t.Fatalf("GrantsForApprover(%s) = %+v; want the pod's grant once and %d of the operator's own", viewer, listed, wantOwn)
		}
	}
}

// TestAGrantUnderNoServicesLoginStaysOffEveryoneElsesList: the new rows are a service login's
// alone, so the disjunct is pinned on both of the other two columns. A pod no service's launcher
// enrolled holds a grant with no approver and no operator under a credential naming no service:
// it is on nobody's list, as before, and this is the case that fails if `c.service is not null`
// is dropped. A grant on a person's own session stays that person's alone.
func TestAGrantUnderNoServicesLoginStaysOffEveryoneElsesList(t *testing.T) {
	m, _, _, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()

	// A pod no service's launcher enrolled: no operator on the enrollment, no service on the
	// credential, and the policy gives it the shared agent-tier secret without asking.
	plain, plainKey := insertEnrollment(t, m.Store, "pod", "plain-pod-"+t.Name(), nil, nil, new(handLaunchedAccount))
	orphan, err := m.Create(ctx, plain, signRequest(t, m, plainKey, "need it", sharedToken), "")
	if err != nil || orphan.GrantID == nil {
		t.Fatalf("Create(a pod no service's launcher enrolled) = %+v, %v; want an automatic grant", orphan, err)
	}

	// A box session of the operator's own machine: its credential names no service either.
	box, key := newEnrollment(t, m.Store, "session", "box-c-"+t.Name(), new(fixtureOperator), nil)
	auto, err := m.Create(ctx, box, signRequest(t, m, key, "need it", sharedToken), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(the operator's session) = %+v, %v; want an automatic grant", auto, err)
	}

	if listed, err := m.GrantsForApprover(ctx, operator); err != nil || len(listed) != 1 || listed[0].GrantID != *auto.GrantID {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the operator's own grant alone", operator, listed, err)
	}
	for _, viewer := range []string{otherPerson, carol} {
		if listed, err := m.GrantsForApprover(ctx, viewer); err != nil || len(listed) != 0 {
			t.Fatalf("GrantsForApprover(%s) = %+v, %v; want nothing: neither grant is held by a service's login", viewer, listed, err)
		}
	}
}
