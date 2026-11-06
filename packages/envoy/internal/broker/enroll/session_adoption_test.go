// session_adoption_test.go is the carry's second half: the next enrollment of a session name
// under the same principal takes what the box the session left still holds, and nothing else
// takes anything (dispatch://INFRA-1167). The first half, what an end leaves standing, is
// session_carry_test.go, whose fixtures these tests build on.
package enroll

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// movedTo enrols the session name again as the box it moved to does: a runtime id of its own, a
// key of its own, and the same session name under the same principal, which is the whole of what
// the carry follows. The credential is a second one of the same holder — the same person for a
// session, the same Kubernetes service for a pod — because a new box is a new key and so a
// credential of its own.
func movedTo(t *testing.T, svc *Service, issuer *oidctest.Issuer, key *oidctest.Key, kind, session, runtime string) (Credential, Enrollment) {
	t.Helper()
	in := Enrollment{Kind: kind, RuntimeID: runtime, Thumbprint: "tp-" + runtime, SessionID: str(session)}
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox-"+runtime)
	if kind == "pod" {
		cred = mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
		in.PodToken = mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", runtime)
	}
	enr, err := svc.Create(context.Background(), cred, in)
	if err != nil {
		t.Fatalf("enrol %s again as %s runtime %s: %v", session, kind, runtime, err)
	}
	if enr.ID == uuid.Nil || enr.Existing {
		t.Fatalf("the move was answered %+v, want an enrollment of its own: the new box enrols rather than taking the old row", enr)
	}
	return cred, enr
}

// endOfTheBox is one of the two ends a move makes, each of which leaves a named session's grants
// and pending requests standing (endMayCarry): the lease lapsing when the box stops renewing,
// and the launcher's own revoke when it lets the session's process go.
type endOfTheBox struct {
	name string
	end  func(t *testing.T, svc *Service, cred Credential, enr Enrollment)
}

func endsOfTheBox() []endOfTheBox {
	return []endOfTheBox{
		{name: "its lease lapsed", end: func(t *testing.T, svc *Service, _ Credential, enr Enrollment) {
			t.Helper()
			lapseLease(t, svc, enr.ID)
			if _, err := svc.EndLapsed(context.Background()); err != nil {
				t.Fatalf("EndLapsed: %v", err)
			}
		}},
		{name: "its launcher revoked it", end: func(t *testing.T, svc *Service, cred Credential, enr Enrollment) {
			t.Helper()
			if err := svc.Revoke(context.Background(), cred, enr.ID.String(), "launcher"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
		}},
	}
}

// grantHolder is the enrollment a grant hangs off now.
func grantHolder(t *testing.T, svc *Service, grant uuid.UUID) uuid.UUID {
	t.Helper()
	var holder uuid.UUID
	if err := svc.Store.Pool.QueryRow(context.Background(), `select enrollment_id from grants where id=$1`, grant).Scan(&holder); err != nil {
		t.Fatalf("read the enrollment of grant %s: %v", grant, err)
	}
	return holder
}

// heldBySession is how many request and grant rows hang off every enrollment carrying the session
// name, the count that says whether the session asked again or took what it had.
func heldBySession(t *testing.T, svc *Service, session string) (requests, grants int) {
	t.Helper()
	if err := svc.Store.Pool.QueryRow(context.Background(), `select
		(select count(*) from requests r join enrollments e on e.id=r.enrollment_id where e.session_id=$1),
		(select count(*) from grants g join enrollments e on e.id=g.enrollment_id where e.session_id=$1)`, session).
		Scan(&requests, &grants); err != nil {
		t.Fatalf("read what the broker holds for session %s: %v", session, err)
	}
	return requests, grants
}

// carryPrincipalOf is the principal an enrollment records, "" for none.
func carryPrincipalOf(t *testing.T, svc *Service, id uuid.UUID) string {
	t.Helper()
	var principal *string
	if err := svc.Store.Pool.QueryRow(context.Background(), `select carry_principal from enrollments where id=$1`, id).Scan(&principal); err != nil {
		t.Fatalf("read the principal of %s: %v", id, err)
	}
	if principal == nil {
		return ""
	}
	return *principal
}

// withholdSecret records that the session of enrollment id asked for name and its operator took
// it back, the row requests.Machine evaluates a later request with.
func withholdSecret(t *testing.T, svc *Service, id uuid.UUID, name string) {
	t.Helper()
	if _, err := svc.Store.Pool.Exec(context.Background(), `insert into withheld_secrets (enrollment_id, name) values ($1,$2)`, id, name); err != nil {
		t.Fatalf("withhold %s from %s: %v", name, id, err)
	}
}

// withheldNames are the names withheld from the session of enrollment id, in name order and one
// row each, so a name the move kept twice would show as a repeat.
func withheldNames(t *testing.T, svc *Service, id uuid.UUID) []string {
	t.Helper()
	rows, err := svc.Store.Pool.Query(context.Background(), `select name from withheld_secrets where enrollment_id=$1 order by name`, id)
	if err != nil {
		t.Fatalf("read the withheld names of %s: %v", id, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a withheld name: %v", err)
		}
		names = append(names, name)
	}
	if rows.Err() != nil {
		t.Fatalf("read the withheld names of %s: %v", id, rows.Err())
	}
	return names
}

// TestAnEnrollmentRecordsThePrincipalItsCallerProved is the other half of the pair a grant
// follows. The principal is read from what the caller has already proved and from nothing it
// states: a pod's own projected token, the operator its credential carries, or the Google
// account its credential's subject names. An enrollment that states no session records none,
// because a principal with no name beside it is no pair.
func TestAnEnrollmentRecordsThePrincipalItsCallerProved(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	machineSubject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-machine")
	machine, err := svc.Credential(ctx, issueMachineCredential(t, svc, machineSubject, "tp-machine"))
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}

	for _, c := range []struct {
		name string
		cred Credential
		in   Enrollment
		want string
	}{
		{
			name: "a pod belongs to the service account its token proved",
			cred: mintCredential(t, svc, nil, str("legion-daemon"), "cluster"),
			in: Enrollment{Kind: "pod", RuntimeID: "pod-uid-1", Thumbprint: "tp-pod", SessionID: str("implementer-g1"),
				PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-uid-1")},
			want: "account:system:serviceaccount:legion:worker",
		},
		{
			name: "a session under a credential carrying an operator belongs to that person",
			cred: mintCredential(t, svc, str("ada@example.com"), nil, "devbox"),
			in:   Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box", SessionID: str("ada-devbox")},
			want: "person:ada@example.com",
		},
		{
			name: "a machine's own session belongs to its fleet's Google account",
			cred: machine,
			in:   Enrollment{Kind: "session", RuntimeID: "machine-1", Thumbprint: "tp-machine-session", SessionID: str("machine-session")},
			want: "google:" + machineAccountEmail,
		},
		{
			name: "an enrollment that names no session belongs to nobody",
			cred: mintCredential(t, svc, str("ada@example.com"), nil, "devbox"),
			in:   Enrollment{Kind: "session", RuntimeID: "box-2", Thumbprint: "tp-box-2"},
			want: "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			enr, err := svc.Create(ctx, c.cred, c.in)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := carryPrincipalOf(t, svc, enr.ID); got != c.want {
				t.Fatalf("the enrollment's principal = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTheNextEnrollmentOfASessionHoldsTheGrantTheEndedBoxHeld is the carry whole, for each kind
// of enrollment and each end a move makes: the session comes up on another box, enrols the same
// name, and the grant it was approved for hangs off the enrollment it proves now, with no second
// request row — nobody was asked again. A pod is measured only after its predecessor has ended,
// never beside a live one, because that is the only shape a pod's move takes: the daemon revokes
// the role's enrollment before the replacement pod enrols, or the lease lapses one lease after
// the pod stopped renewing.
func TestTheNextEnrollmentOfASessionHoldsTheGrantTheEndedBoxHeld(t *testing.T) {
	for _, kind := range carryKinds {
		for _, end := range endsOfTheBox() {
			t.Run(kind+", "+end.name, func(t *testing.T) {
				svc := newService(t)
				issuer, key := withPodVerifier(t, svc)
				session := kind + "-moves"
				cred, old := carryEnrollment(t, svc, issuer, key, kind, session)
				grant := insertLiveGrant(t, svc, old.ID)

				end.end(t, svc, cred, old)
				if left := readCarryState(t, svc, old.ID); !left.Ended || !left.Dormant || left.LiveGrants != 1 {
					t.Fatalf("the end left %+v, want it ended, dormant and still holding its grant: this test measures what the next enrollment takes", left)
				}

				_, moved := movedTo(t, svc, issuer, key, kind, session, session+"-2")
				if holder := grantHolder(t, svc, grant); holder != moved.ID {
					t.Fatalf("grant %s hangs off enrollment %s, want the moved session's %s: a grant follows its session name to the box it enrols on next", grant, holder, moved.ID)
				}
				if took := readCarryState(t, svc, moved.ID); took.LiveGrants != 1 {
					t.Fatalf("the moved session's enrollment holds %+v, want its one live grant", took)
				}
				if requests, grants := heldBySession(t, svc, session); requests != 1 || grants != 1 {
					t.Fatalf("the broker holds %d requests and %d grants for session %s, want one each: the session took what it had rather than asking again", requests, grants, session)
				}
				if left := readCarryState(t, svc, old.ID); !left.Ended || left.Dormant || left.LiveGrants != 0 {
					t.Fatalf("the box the session left is %+v, want it ended, no longer dormant and holding nothing", left)
				}
			})
		}
	}
}

// TestAPendingRequestFiledBeforeTheMoveIsStillPendingUnderTheNewEnrollment: what a person has not
// answered yet moves with the grants. The request is still pending, now under the enrollment the
// session proves, so the answer lands on the session as it stands when it comes rather than on
// the box that asked, and no approver's pending list lost it.
func TestAPendingRequestFiledBeforeTheMoveIsStillPendingUnderTheNewEnrollment(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	cred, old := carryEnrollment(t, svc, issuer, key, "session", "asked-before-the-move")
	request := insertPendingRequest(t, svc, old.ID)

	if err := svc.Revoke(ctx, cred, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, moved := movedTo(t, svc, issuer, key, "session", "asked-before-the-move", "asked-before-the-move-2")

	state, decidedBy, cancellations, event := requestState(t, svc, request)
	if state != "pending" || decidedBy != "" || cancellations != 0 || event != "" {
		t.Fatalf("the request is %s, decided by %q, with %d cancellations and the record event %q; want it still pending and undecided",
			state, decidedBy, cancellations, event)
	}
	var holder uuid.UUID
	if err := svc.Store.Pool.QueryRow(ctx, `select enrollment_id from requests where id=$1`, request).Scan(&holder); err != nil {
		t.Fatalf("read the request's enrollment: %v", err)
	}
	if holder != moved.ID {
		t.Fatalf("the pending request hangs off enrollment %s, want the moved session's %s", holder, moved.ID)
	}
}

// TestTheWithheldNamesMoveAndANameBothRowsHoldIsKeptOnce: a withhold is the one way a person
// takes a secret back from a session that got it without asking, so a move must not undo it. Two
// boxes of one session name can each hold one, and the successor keeps each name once, which is
// all its primary key (enrollment_id, name) can hold.
func TestTheWithheldNamesMoveAndANameBothRowsHoldIsKeptOnce(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	session := "withholds-move"
	// Two boxes of one name, each withholding a name of its own and one they share. The second
	// enrols while the first holds nothing, so it takes nothing then and both are predecessors of
	// the third.
	_, firstEnrollment := carryEnrollment(t, svc, issuer, key, "session", session)
	_, secondEnrollment := movedTo(t, svc, issuer, key, "session", session, session+"-2")
	withholdSecret(t, svc, firstEnrollment.ID, "SHARED_KEY")
	withholdSecret(t, svc, firstEnrollment.ID, "FIRST_KEY")
	withholdSecret(t, svc, secondEnrollment.ID, "SHARED_KEY")
	withholdSecret(t, svc, secondEnrollment.ID, "SECOND_KEY")

	_, third := movedTo(t, svc, issuer, key, "session", session, session+"-3")
	want := []string{"FIRST_KEY", "SECOND_KEY", "SHARED_KEY"}
	if got := withheldNames(t, svc, third.ID); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("the moved session's withheld names = %v, want %v: every name follows the session and the one both boxes held is kept once", got, want)
	}
	for _, left := range []Enrollment{firstEnrollment, secondEnrollment} {
		if got := withheldNames(t, svc, left.ID); len(got) != 0 {
			t.Fatalf("enrollment %s still withholds %v, want nothing: every row it held moved", left.ID, got)
		}
	}
}

// TestAWithholdMadeBeforeTheMoveSurvivesABoxThatEndedHoldingNothingElse is the usual order an
// operator's revoke leaves behind: the revoke of an automatic grant writes the withheld_secrets
// row and takes the grant, so the box ends holding no live grant and no pending request, its end
// is terminal and its row never goes dormant. The withhold follows the session anyway — it is a
// predecessor for its withheld names whatever state its row is in — so the next enrolment of the
// name holds the name back as the operator said, rather than handing it over automatically
// again.
func TestAWithholdMadeBeforeTheMoveSurvivesABoxThatEndedHoldingNothingElse(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	for _, kind := range carryKinds {
		for _, end := range endsOfTheBox() {
			t.Run(kind+", "+end.name, func(t *testing.T) {
				session := "withhold-alone-" + kind + "-" + uuid.NewString()[:8]
				cred, old := carryEnrollment(t, svc, issuer, key, kind, session)
				withholdSecret(t, svc, old.ID, "AUTO_TOKEN")

				end.end(t, svc, cred, old)
				if left := readCarryState(t, svc, old.ID); !left.Ended || left.Dormant {
					t.Fatalf("after %s the %s enrollment is %+v, want it ended and not dormant: it holds no grant and no pending request, so its end is terminal — which is the state the withhold has to survive",
						end.name, kind, left)
				}

				_, moved := movedTo(t, svc, issuer, key, kind, session, session+"-2")
				if got := withheldNames(t, svc, moved.ID); len(got) != 1 || got[0] != "AUTO_TOKEN" {
					t.Fatalf("after %s the %s enrollment the session moved to withholds %v, want [AUTO_TOKEN]: the operator took that name back from the session, and a new box does not give it back",
						end.name, kind, got)
				}
				if got := withheldNames(t, svc, old.ID); len(got) != 0 {
					t.Fatalf("after %s the ended %s enrollment still withholds %v, want nothing: every withheld row moves to the enrollment the session proves now",
						end.name, kind, got)
				}
			})
		}
	}
}

// TestTheAdoptionTrailNamesBothEnrollments: an operator asking why a session reads a secret it
// never asked this box for has to land on the box that did. One enrollment.adopted row on the
// successor names the predecessor it took from, and one grant.moved row per grant names that
// grant, the enrollment it hangs off now and the one it came from.
func TestTheAdoptionTrailNamesBothEnrollments(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	session := "two-grants-move"
	cred, old := carryEnrollment(t, svc, issuer, key, "session", session)
	grants := []uuid.UUID{insertLiveGrant(t, svc, old.ID), insertLiveGrant(t, svc, old.ID)}
	if err := svc.Revoke(ctx, cred, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, moved := movedTo(t, svc, issuer, key, "session", session, session+"-2")

	if rows := auditRows(t, svc, moved.ID, "enrollment.adopted"); rows != 1 {
		t.Fatalf("the moved session has %d enrollment.adopted rows, want one per predecessor it took from", rows)
	}
	var from, to string
	var moveCount int64
	if err := svc.Store.Pool.QueryRow(ctx, `select detail->>'from', detail->>'to', (detail->>'grants')::bigint
		from audit where kind='enrollment.adopted' and enrollment_id=$1`, moved.ID).Scan(&from, &to, &moveCount); err != nil {
		t.Fatalf("read the enrollment.adopted row: %v", err)
	}
	if from != old.ID.String() || to != moved.ID.String() || moveCount != int64(len(grants)) {
		t.Fatalf("the adoption is recorded from %s to %s over %d grants, want from %s to %s over %d",
			from, to, moveCount, old.ID, moved.ID, len(grants))
	}
	if rows := auditRows(t, svc, moved.ID, "grant.moved"); rows != len(grants) {
		t.Fatalf("the trail has %d grant.moved rows, want one per grant (%d)", rows, len(grants))
	}
	for _, grant := range grants {
		var named string
		if err := svc.Store.Pool.QueryRow(ctx, `select detail->>'from' from audit where kind='grant.moved' and grant_id=$1 and enrollment_id=$2`,
			grant, moved.ID).Scan(&named); err != nil {
			t.Fatalf("read the grant.moved row of %s: %v", grant, err)
		}
		if named != old.ID.String() {
			t.Fatalf("grant %s is recorded as moved from %s, want the box it was approved on, %s", grant, named, old.ID)
		}
	}
}

// TestNothingIsAdoptedForAnEmptySessionID: the pair is a name and a principal, so a launcher that
// states no name has nothing for a later enrollment to follow, however much its enrollment holds
// and whatever its credential proved.
func TestNothingIsAdoptedForAnEmptySessionID(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	for _, name := range []*string{nil, str("")} {
		t.Run("session id "+sessionIDName(name), func(t *testing.T) {
			runtime := "box-" + sessionIDName(name)
			first, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtime + "-1", Thumbprint: "tp-" + runtime + "-1", SessionID: name})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			grant := insertLiveGrant(t, svc, first.ID)
			if err := svc.Revoke(ctx, cred, first.ID.String(), "launcher"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			if left := readCarryState(t, svc, first.ID); left.Dormant || left.LiveGrants != 0 {
				t.Fatalf("ending an enrollment that names no session left %+v, want its grant revoked and no dormancy", left)
			}

			second, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtime + "-2", Thumbprint: "tp-" + runtime + "-2", SessionID: name})
			if err != nil {
				t.Fatalf("Create again: %v", err)
			}
			if got := carryPrincipalOf(t, svc, second.ID); got != "" {
				t.Fatalf("an enrollment naming no session records the principal %q, want none", got)
			}
			if holder := grantHolder(t, svc, grant); holder != first.ID {
				t.Fatalf("grant %s moved to %s, want it left on %s: an enrollment that names no session adopts nothing", grant, holder, first.ID)
			}
			if rows := auditRows(t, svc, second.ID, "enrollment.adopted"); rows != 0 {
				t.Fatalf("the second enrollment has %d enrollment.adopted rows, want none", rows)
			}
		})
	}
}

// TestAnAdoptedPredecessorIsNoLongerDormantSoTheCarrySweepTakesNothing: dormancy is "this ended
// row still holds something a session may take", and after the move it holds nothing. So the
// sweep that revokes what a dormant enrollment held finds nothing of it, even with the carry
// window turned off, which is the setting that ends every dormant enrollment at once.
func TestAnAdoptedPredecessorIsNoLongerDormantSoTheCarrySweepTakesNothing(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	session := "swept-after-the-move"
	cred, old := carryEnrollment(t, svc, issuer, key, "session", session)
	grant := insertLiveGrant(t, svc, old.ID)
	request := insertPendingRequest(t, svc, old.ID)
	if err := svc.Revoke(ctx, cred, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, moved := movedTo(t, svc, issuer, key, "session", session, session+"-2")
	// A carry window of 0 is the sweep's floor: it ends every dormant enrollment it finds, so
	// what it does not end here is not a row whose window is still running.
	ended, err := svc.EndCarried(ctx, 0)
	if err != nil {
		t.Fatalf("EndCarried: %v", err)
	}
	if len(ended) != 0 {
		t.Fatalf("the carry sweep ended %+v, want nothing: no enrollment is dormant once the session has taken what the box it left held", ended)
	}
	if took := readCarryState(t, svc, moved.ID); took.LiveGrants != 1 || took.Pending != 1 {
		t.Fatalf("the moved session holds %+v after the sweep, want its grant live and its request pending", took)
	}
	if holder := grantHolder(t, svc, grant); holder != moved.ID {
		t.Fatalf("grant %s hangs off %s after the sweep, want the moved session's %s", grant, holder, moved.ID)
	}
	if state, _, _, _ := requestState(t, svc, request); state != "pending" {
		t.Fatalf("the request is %s after the sweep, want it still pending", state)
	}
}

// sessionIDName names a session id for a subtest's name: "none" for a launcher that stated no field at all
// and "empty" for one that stated "".
func sessionIDName(name *string) string {
	if name == nil {
		return "none"
	}
	return "empty"
}
