package enroll

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// carryKinds are the two enrollment kinds a session's grant has to follow, each named as its
// launcher names it. Both carry: a session's box is replaced by a restart, a rebuild or a second
// sign-in, and a pod is replaced by its own role generation, whose predecessor the daemon revokes
// before the successor enrols.
var carryKinds = []string{"session", "pod"}

// carryEnrollment enrols one named session of kind, under the credential that kind's launcher
// holds: a person's own machine login for a session, a Kubernetes service's login for a pod. name
// is the runtime id, the session id and the thumbprint's suffix at once, so every row of one
// subtest is that subtest's.
func carryEnrollment(t *testing.T, svc *Service, issuer *oidctest.Issuer, key *oidctest.Key, kind, name string) (Credential, Enrollment) {
	t.Helper()
	ctx := context.Background()
	in := Enrollment{Kind: kind, RuntimeID: name, Thumbprint: "tp-" + name, SessionID: str(name)}
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	if kind == "pod" {
		cred = mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
		in.PodToken = mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", name)
	}
	enr, err := svc.Create(ctx, cred, in)
	if err != nil {
		t.Fatalf("Create(%s enrollment %s): %v", kind, name, err)
	}
	return cred, enr
}

// carryState is what an end left on an enrollment: whether the row has ended, whether it is
// dormant (holding what it held for the session's next enrollment), when it ended, and how many
// live grants and pending requests still hang off it.
type carryState struct {
	Ended      bool
	Dormant    bool
	EndedAt    time.Time
	LiveGrants int
	Pending    int
}

func readCarryState(t *testing.T, svc *Service, id uuid.UUID) carryState {
	t.Helper()
	var s carryState
	var endedAt *time.Time
	if err := svc.Store.Pool.QueryRow(context.Background(), `select revoked_at, dormant_since is not null,
		(select count(*) from grants where enrollment_id=$1 and revoked_at is null),
		(select count(*) from requests where enrollment_id=$1 and state='pending')
		from enrollments where id=$1`, id).Scan(&endedAt, &s.Dormant, &s.LiveGrants, &s.Pending); err != nil {
		t.Fatalf("read enrollment %s: %v", id, err)
	}
	if endedAt != nil {
		s.Ended, s.EndedAt = true, *endedAt
	}
	return s
}

// lapse puts an enrollment's lease in the past, the state EndLapsed selects.
func lapseLease(t *testing.T, svc *Service, id uuid.UUID) {
	t.Helper()
	if _, err := svc.Store.Pool.Exec(context.Background(), `update enrollments set lease_expires_at = now() - interval '1 minute' where id=$1`, id); err != nil {
		t.Fatalf("lapse the lease of %s: %v", id, err)
	}
}

// backdateDormancy moves an enrollment's dormant_since into the past, so the carry sweep reads it
// as a window that has run out.
func backdateDormancy(t *testing.T, svc *Service, id uuid.UUID, by time.Duration) {
	t.Helper()
	if _, err := svc.Store.Pool.Exec(context.Background(), `update enrollments set dormant_since = dormant_since - make_interval(secs => $2) where id=$1`,
		id, by.Seconds()); err != nil {
		t.Fatalf("backdate the dormancy of %s: %v", id, err)
	}
}

// clearCarryPrincipal nulls an enrollment's carry_principal, the one row shape Create cannot
// make and the deploy is full of: every enrollment live when migration 0027 applies has the
// column null, every daemon pod enrollment among them, and those name a session
// (packages/daemon/internal/supervise/secrets.go).
func clearCarryPrincipal(t *testing.T, svc *Service, id uuid.UUID) {
	t.Helper()
	if _, err := svc.Store.Pool.Exec(context.Background(), `update enrollments set carry_principal=null where id=$1`, id); err != nil {
		t.Fatalf("clear the carry principal of %s: %v", id, err)
	}
}

// auditRows counts the audit rows of kind written for an enrollment.
func auditRows(t *testing.T, svc *Service, id uuid.UUID, kind string) int {
	t.Helper()
	var rows int
	if err := svc.Store.Pool.QueryRow(context.Background(), `select count(*) from audit where kind=$2 and enrollment_id=$1`, id, kind).Scan(&rows); err != nil {
		t.Fatalf("count %s audit rows of %s: %v", kind, id, err)
	}
	return rows
}

// insertSignIn records person's sign-in on the box whose launcher credential cred is, the pair
// RevokePersonSignIn reads: the sign-in holds the box key's thumbprint, which is how that revoke
// finds the credential the sign-in's trades minted (a live credential holds one key). It answers
// the sign-in's id.
func insertSignIn(t *testing.T, svc *Service, cred Credential, person string) string {
	t.Helper()
	ctx := context.Background()
	var thumbprint string
	if err := svc.Store.Pool.QueryRow(ctx, `select key_thumbprint from launcher_credentials where id=$1`, cred.ID).Scan(&thumbprint); err != nil {
		t.Fatalf("read the credential's key thumbprint: %v", err)
	}
	var id string
	if err := svc.Store.Pool.QueryRow(ctx, `insert into person_signins (person, thumbprint, machine_account)
		values (lower($1), $2, 'fleet@example.iam.gserviceaccount.com') returning id::text`, person, thumbprint).Scan(&id); err != nil {
		t.Fatalf("insert the person's sign-in: %v", err)
	}
	return id
}

// TestAnEndOfTheBoxLeavesANamedSessionsGrantAndPendingRequestStanding is the carry's first half:
// the two ends a move makes — the lease lapsing when the box stops renewing (EndLapsed) and the
// launcher's own revoke when it lets the session's process go (Revoke) — take nothing from a
// session that named itself and still holds something. The row is marked ended and dormant, its
// grant stays live, its pending request stays pending, and no approver's list loses it.
func TestAnEndOfTheBoxLeavesANamedSessionsGrantAndPendingRequestStanding(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	for _, kind := range carryKinds {
		for _, end := range []struct {
			name string
			// tag names this end's rows, so the two ends of one kind never share a runtime id.
			tag string
			end func(cred Credential, enr Enrollment)
		}{
			{"its lease lapsed", "lapse", func(cred Credential, enr Enrollment) {
				lapseLease(t, svc, enr.ID)
				ended, err := svc.EndLapsed(ctx)
				if err != nil {
					t.Fatalf("EndLapsed: %v", err)
				}
				if len(ended) != 1 || ended[0].ID != enr.ID.String() {
					t.Fatalf("EndLapsed ended %+v, want the one lapsed enrollment %s", ended, enr.ID)
				}
				if !ended[0].Dormant || ended[0].GrantsRevoked != 0 || ended[0].RequestsCancelled != 0 {
					t.Fatalf("EndLapsed reported %+v, want it left dormant having revoked and cancelled nothing", ended[0])
				}
			}},
			{"its launcher revoked it", "revoke", func(cred Credential, enr Enrollment) {
				if err := svc.Revoke(ctx, cred, enr.ID.String(), "launcher:test"); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
			}},
		} {
			t.Run(kind+", "+end.name, func(t *testing.T) {
				cred, enr := carryEnrollment(t, svc, issuer, key, kind, "carry-"+kind+"-"+end.tag)
				grant := insertLiveGrant(t, svc, enr.ID)
				pending := insertPendingRequest(t, svc, enr.ID)

				end.end(cred, enr)

				got := readCarryState(t, svc, enr.ID)
				if !got.Ended || !got.Dormant || got.LiveGrants != 1 || got.Pending != 1 {
					t.Fatalf("after %s the %s enrollment is %+v, want it ended and dormant, still holding its one grant %s and its one pending request %s: the session is on another box, and what it holds is the session's",
						end.name, kind, got, grant, pending)
				}
				if state, by, audits, event := requestState(t, svc, pending); state != "pending" || by != "" || audits != 0 || event != "" {
					t.Fatalf("the pending request of the dormant %s enrollment: state=%s decided_by=%s audit rows=%d record event=%q, want it untouched: an approver answers it on the box the session runs on now",
						kind, state, by, audits, event)
				}
				if _, live, err := svc.Lookup(ctx, enr.ID.String()); err != nil || live {
					t.Fatalf("Lookup(the dormant %s enrollment) = live=%v err=%v, want not live: the key it was bound to died with the box", kind, live, err)
				}
			})
		}
	}
}

// TestTheCarrySweepRevokesWhatADormantEnrollmentHeldOnceItsWindowPasses: a dormant enrollment is
// left alone while its window runs and ended for good once it has passed — its grants revoked,
// its pending requests cancelled, and its own enrollment.carry_expired audit row naming why — and
// the end it already had is the one it keeps, since the box ended then and not now. A second
// sweep finds it no longer dormant and changes nothing.
func TestTheCarrySweepRevokesWhatADormantEnrollmentHeldOnceItsWindowPasses(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	for _, kind := range carryKinds {
		t.Run(kind, func(t *testing.T) {
			cred, enr := carryEnrollment(t, svc, issuer, key, kind, "sweep-"+kind)
			insertLiveGrant(t, svc, enr.ID)
			pending := insertPendingRequest(t, svc, enr.ID)
			if err := svc.Revoke(ctx, cred, enr.ID.String(), "launcher:test"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			dormant := readCarryState(t, svc, enr.ID)
			if !dormant.Dormant {
				t.Fatalf("the %s enrollment is %+v after its launcher ended it, want it dormant", kind, dormant)
			}

			ended, err := svc.EndCarried(ctx, time.Hour)
			if err != nil || len(ended) != 0 {
				t.Fatalf("EndCarried inside the window ended %+v (%v), want nothing: the session has the whole window to come up on another box", ended, err)
			}
			if got := readCarryState(t, svc, enr.ID); got.LiveGrants != 1 || got.Pending != 1 {
				t.Fatalf("inside the window the %s enrollment holds %+v, want its grant and its pending request untouched", kind, got)
			}

			backdateDormancy(t, svc, enr.ID, 2*time.Hour)
			ended, err = svc.EndCarried(ctx, time.Hour)
			if err != nil {
				t.Fatalf("EndCarried: %v", err)
			}
			if len(ended) != 1 || ended[0].ID != enr.ID.String() || ended[0].SessionID != "sweep-"+kind ||
				ended[0].GrantsRevoked != 1 || ended[0].RequestsCancelled != 1 {
				t.Fatalf("EndCarried past the window ended %+v, want the one dormant %s enrollment %s with its one grant revoked and its one request cancelled",
					ended, kind, enr.ID)
			}
			got := readCarryState(t, svc, enr.ID)
			if got.Dormant || got.LiveGrants != 0 || got.Pending != 0 {
				t.Fatalf("after the carry window passed the %s enrollment is %+v, want it holding nothing and dormant no longer: nobody took what it held", kind, got)
			}
			if !got.EndedAt.Equal(dormant.EndedAt) {
				t.Fatalf("the swept %s enrollment now reads as having ended at %s, want the %s it ended at: the sweep finishes that end rather than making a second one",
					kind, got.EndedAt, dormant.EndedAt)
			}
			if rows := auditRows(t, svc, enr.ID, "enrollment.carry_expired"); rows != 1 {
				t.Fatalf("enrollment.carry_expired audit rows for the swept %s enrollment = %d, want one naming why its grants went", kind, rows)
			}
			if state, by, audits, event := requestState(t, svc, pending); state != "cancelled" || by != "broker" || audits != 1 || event != "cancelled by broker" {
				t.Fatalf("the pending request of the swept %s enrollment: state=%s decided_by=%s audit rows=%d record event=%q, want cancelled by broker with a cancelled record event",
					kind, state, by, audits, event)
			}

			again, err := svc.EndCarried(ctx, time.Hour)
			if err != nil || len(again) != 0 {
				t.Fatalf("a second EndCarried ended %+v (%v), want nothing: a row that holds nothing is dormant no longer", again, err)
			}
		})
	}
}

// TestTheCarrySweepTurnedOffEndsADormantEnrollmentAtOnce pins the floor of
// BROKER_SESSION_CARRY_SECONDS: 0 is no carry at all, so the next sweep ends every dormant
// enrollment it finds and the broker behaves as it did before the carry.
func TestTheCarrySweepTurnedOffEndsADormantEnrollmentAtOnce(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred, enr := carryEnrollment(t, svc, nil, nil, "session", "carry-off")
	insertLiveGrant(t, svc, enr.ID)
	if err := svc.Revoke(ctx, cred, enr.ID.String(), "launcher:test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	ended, err := svc.EndCarried(ctx, 0)
	if err != nil || len(ended) != 1 || ended[0].GrantsRevoked != 1 {
		t.Fatalf("EndCarried(0) = %+v, %v, want the dormant enrollment ended with its one grant revoked", ended, err)
	}
	if got := readCarryState(t, svc, enr.ID); got.Dormant || got.LiveGrants != 0 {
		t.Fatalf("with the carry off the enrollment is %+v, want it holding nothing", got)
	}
}

// TestAPersonsRevokeTakesTheSessionsGrantAtOnce: the three ends a person makes are terminal,
// whatever the session still holds and whichever kind it is. A person revoking their own machine
// login, a service's login, or their sign-in on a box means every secret that login reached, so
// the grant is revoked and the pending request cancelled in the same transaction, with no
// dormancy and nothing left for another box to adopt.
func TestAPersonsRevokeTakesTheSessionsGrantAtOnce(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	for _, path := range []struct {
		name string
		kind string
		// credential mints the login the enrollment is made under, and revoke ends it as that
		// path's person does.
		credential func(name string) Credential
		revoke     func(cred Credential)
	}{
		{
			name: "the person's own machine login", kind: "session",
			credential: func(string) Credential { return mintApprovedCredential(t, svc, "ada@example.com", nil, "devbox") },
			revoke: func(cred Credential) {
				if err := svc.RevokeOwnCredential(ctx, cred.ID.String(), "ada@example.com", "human:ada@example.com"); err != nil {
					t.Fatalf("RevokeOwnCredential: %v", err)
				}
			},
		},
		{
			name: "a service's machine login", kind: "pod",
			credential: func(string) Credential {
				return mintApprovedCredential(t, svc, "ada@example.com", str("legion-daemon"), "cluster")
			},
			revoke: func(cred Credential) {
				if err := svc.RevokeCredential(ctx, cred.ID.String(), "ada@example.com", "human:ada@example.com"); err != nil {
					t.Fatalf("RevokeCredential: %v", err)
				}
			},
		},
		{
			name: "the person's sign-in on the box", kind: "session",
			credential: func(string) Credential { return mintCredential(t, svc, str("ada@example.com"), nil, "signin-box") },
			revoke: func(cred Credential) {
				signIn := insertSignIn(t, svc, cred, "ada@example.com")
				if _, err := svc.RevokePersonSignIn(ctx, signIn, "ada@example.com", "human:ada@example.com"); err != nil {
					t.Fatalf("RevokePersonSignIn: %v", err)
				}
			},
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			name := "terminal-" + path.kind + "-" + uuid.NewString()[:8]
			cred := path.credential(name)
			in := Enrollment{Kind: path.kind, RuntimeID: name, Thumbprint: "tp-" + name, SessionID: str(name)}
			if path.kind == "pod" {
				in.PodToken = mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", name)
			}
			enr, err := svc.Create(ctx, cred, in)
			if err != nil {
				t.Fatalf("Create(%s): %v", path.kind, err)
			}
			insertLiveGrant(t, svc, enr.ID)
			pending := insertPendingRequest(t, svc, enr.ID)

			path.revoke(cred)

			got := readCarryState(t, svc, enr.ID)
			if !got.Ended || got.Dormant || got.LiveGrants != 0 || got.Pending != 0 {
				t.Fatalf("after %s revoked it the enrollment is %+v, want it ended, never dormant, holding nothing: a person's revoke is the end of the session too",
					path.name, got)
			}
			if state, by, audits, event := requestState(t, svc, pending); state != "cancelled" || by != "human:ada@example.com" || audits != 1 || event != "cancelled by human:ada@example.com" {
				t.Fatalf("the pending request after %s revoked it: state=%s decided_by=%s audit rows=%d record event=%q, want cancelled by the person",
					path.name, state, by, audits, event)
			}
		})
	}
}

// TestRevokingAMachineLoginTakesBackWhatItsDormantEnrollmentHeld is the hole a carry would leave
// otherwise: a dormant enrollment's row is already marked revoked, so a read of the login's live
// enrollments alone would pass over it and leave its grant for the next enrollment of that session
// name to adopt — on a box the person has not revoked. The revoke takes the dormant rows too.
func TestRevokingAMachineLoginTakesBackWhatItsDormantEnrollmentHeld(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintApprovedCredential(t, svc, "ada@example.com", nil, "devbox")
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "dormant-then-revoked",
		Thumbprint: "tp-dormant-then-revoked", SessionID: str("dormant-then-revoked")})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	insertLiveGrant(t, svc, enr.ID)
	pending := insertPendingRequest(t, svc, enr.ID)
	if err := svc.Revoke(ctx, cred, enr.ID.String(), "launcher:test"); err != nil {
		t.Fatalf("Revoke(the launcher's own end, which leaves it dormant): %v", err)
	}
	if got := readCarryState(t, svc, enr.ID); !got.Dormant || got.LiveGrants != 1 {
		t.Fatalf("the enrollment is %+v after its launcher ended it, want it dormant holding its grant", got)
	}

	if err := svc.RevokeOwnCredential(ctx, cred.ID.String(), "ada@example.com", "human:ada@example.com"); err != nil {
		t.Fatalf("RevokeOwnCredential: %v", err)
	}
	got := readCarryState(t, svc, enr.ID)
	if got.Dormant || got.LiveGrants != 0 || got.Pending != 0 {
		t.Fatalf("after the person revoked the machine login the dormant enrollment is %+v, want it holding nothing: the person took that secret back", got)
	}
	if state, by, _, _ := requestState(t, svc, pending); state != "cancelled" || by != "human:ada@example.com" {
		t.Fatalf("the dormant enrollment's pending request after the login's revoke = %s by %s, want cancelled by the person", state, by)
	}
}

// TestAnEndThatCarriesNothingEndsTheEnrollmentAsItAlwaysDid: dormancy is for a named session
// holding something, and nothing else. An enrollment that names a session and holds nothing, and
// one that holds a grant and names no session — a launcher that stated none — are both ended
// outright, grants revoked and requests cancelled, with no dormant_since for the carry sweep to
// find.
func TestAnEndThatCarriesNothingEndsTheEnrollmentAsItAlwaysDid(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	for _, tc := range []struct {
		name       string
		sessionID  *string
		holdsGrant bool
	}{
		{"a named session holding nothing", str("empty-session"), false},
		{"a grant under an enrollment naming no session", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimeID := "no-carry-" + uuid.NewString()[:8]
			enr, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtimeID,
				Thumbprint: "tp-" + runtimeID, SessionID: tc.sessionID})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tc.holdsGrant {
				insertLiveGrant(t, svc, enr.ID)
			}
			lapseLease(t, svc, enr.ID)
			ended, err := svc.EndLapsed(ctx)
			if err != nil || len(ended) != 1 || ended[0].ID != enr.ID.String() {
				t.Fatalf("EndLapsed = %+v, %v, want the one lapsed enrollment %s", ended, err, enr.ID)
			}
			if ended[0].Dormant {
				t.Fatalf("EndLapsed left %s dormant, want it ended outright: %s is nothing to carry", tc.name, tc.name)
			}
			if got := readCarryState(t, svc, enr.ID); !got.Ended || got.Dormant || got.LiveGrants != 0 {
				t.Fatalf("%s after its lease lapsed is %+v, want it ended, not dormant, holding no live grant", tc.name, got)
			}
			if rows := auditRows(t, svc, enr.ID, "enrollment.expired"); rows != 1 {
				t.Fatalf("enrollment.expired audit rows for %s = %d, want one", tc.name, rows)
			}
		})
	}
}

// TestAnEnrollmentWithNoPrincipalEndsAsItAlwaysDidAndOneWithAPrincipalGoesDormant: dormancy is
// the promise that a later enrolment can take what the ended row still holds, and adopt matches
// on the whole pair (carry_principal, session_id), so a row with no principal can be taken by
// nobody. Leaving it dormant would only delay its revoke by the carry window and leave a card in
// an approver's Inbox that no approval could grant. Every enrollment live when migration 0027
// applies is such a row, so the probe reads the principal as well as the name. A named
// enrollment that does record one goes dormant on either end of its box, as it always did.
func TestAnEnrollmentWithNoPrincipalEndsAsItAlwaysDidAndOneWithAPrincipalGoesDormant(t *testing.T) {
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	for _, tc := range []struct {
		name string
		// principal says whether the row keeps the principal its caller proved, or is left as
		// the rows the deploy migrates are, with a session name and no principal at all.
		principal bool
	}{
		{"no principal, as every row live at the deploy has", false},
		{"the principal its caller proved", true},
	} {
		for _, end := range endsOfTheBox() {
			t.Run(tc.name+", "+end.name, func(t *testing.T) {
				session := "principal-probe-" + uuid.NewString()[:8]
				cred, enr := carryEnrollment(t, svc, issuer, key, "pod", session)
				grant := insertLiveGrant(t, svc, enr.ID)
				pending := insertPendingRequest(t, svc, enr.ID)
				if !tc.principal {
					clearCarryPrincipal(t, svc, enr.ID)
				}

				end.end(t, svc, cred, enr)

				held := 0
				if tc.principal {
					held = 1
				}
				got := readCarryState(t, svc, enr.ID)
				if !got.Ended || got.Dormant != tc.principal || got.LiveGrants != held || got.Pending != held {
					t.Fatalf("after %s the enrollment with %s is %+v, want it ended, dormant=%v, holding %d live grant (%s) and %d pending request (%s): a row nothing can adopt is ended for good, and one a session can come back to is kept",
						end.name, tc.name, got, tc.principal, held, grant, held, pending)
				}
				wantState := "cancelled"
				if tc.principal {
					wantState = "pending"
				}
				if state, _, _, _ := requestState(t, svc, pending); state != wantState {
					t.Fatalf("after %s the request of the enrollment with %s is %s, want %s: an approval must never land on a session nothing can take up",
						end.name, tc.name, state, wantState)
				}
			})
		}
	}
}
