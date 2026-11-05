// session_name_holder_test.go is the rule a carry rests on: a grant follows a session name under
// one principal, so the name belongs to one principal (dispatch://INFRA-1167). Create binds the
// name it enrols to the principal its caller proved, in the enrollment's own transaction and on
// the one table the token exchange binds every token's name in (oauth_session_bindings), and a
// name another principal holds is refused outright. What the carry does once the name is one
// principal's is session_adoption_test.go's.
package enroll

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// sessionNameHolder is the principal the broker records as holding a session name, "" for a name
// no principal holds. It reads oauth_session_bindings itself, because what is being measured is
// that an enrollment writes the same row the token exchange writes.
func sessionNameHolder(t *testing.T, svc *Service, session string) string {
	t.Helper()
	var principal string
	err := svc.Store.Pool.QueryRow(context.Background(),
		`select coalesce((select principal from oauth_session_bindings where session=$1), '')`, session).Scan(&principal)
	if err != nil {
		t.Fatalf("read the holder of session name %s: %v", session, err)
	}
	return principal
}

// enrollmentsNaming counts the enrollment rows carrying a session name, so a refused enrolment
// shows as having written none.
func enrollmentsNaming(t *testing.T, svc *Service, session string) int {
	t.Helper()
	var rows int
	if err := svc.Store.Pool.QueryRow(context.Background(),
		`select count(*) from enrollments where session_id=$1`, session).Scan(&rows); err != nil {
		t.Fatalf("count the enrollments naming session %s: %v", session, err)
	}
	return rows
}

// TestASessionNameAnotherPrincipalHoldsIsRefusedAndTakesNothing: the second principal reaching for
// a name the first holds is refused the whole enrollment — ErrSessionHeld, no row of its own, and
// nothing adopted — and the grant the first principal's session was approved for stays exactly
// where it was, held by the box the session left for its own session to take.
//
// The two arms are the two ways a second principal turns up under one name: another fleet's
// Google account, whose machine enrols a session of its own, and another person, signed in on a
// box of their own.
func TestASessionNameAnotherPrincipalHoldsIsRefusedAndTakesNothing(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		boxes func(t *testing.T, svc *Service) (Credential, Credential)
	}{
		{
			name: "a second Google account of its own fleet",
			boxes: func(t *testing.T, svc *Service) (Credential, Credential) {
				t.Helper()
				other := "other-fleet@example-project.iam.gserviceaccount.com"
				first, err := svc.Credential(ctx, issueMachineCredential(t, svc,
					exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-first"), "tp-first"))
				if err != nil {
					t.Fatalf("Credential: %v", err)
				}
				second, err := svc.Credential(ctx, issueMachineCredential(t, svc,
					exchange.MachineSubject(exchange.GoogleIdentity{Account: other}, "tp-second"), "tp-second"))
				if err != nil {
					t.Fatalf("Credential: %v", err)
				}
				return first, second
			},
		},
		{
			name: "a second person on a box of their own",
			boxes: func(t *testing.T, svc *Service) (Credential, Credential) {
				t.Helper()
				return mintCredential(t, svc, str("ada@example.com"), nil, "ada-devbox"),
					mintCredential(t, svc, str("grace@example.com"), nil, "grace-devbox")
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newService(t)
			session := "one-name-two-principals"
			first, second := c.boxes(t, svc)
			owner, err := svc.Create(ctx, first, Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1", SessionID: str(session)})
			if err != nil {
				t.Fatalf("the first principal's enrollment: %v", err)
			}
			held := carryPrincipalOf(t, svc, owner.ID)
			if got := sessionNameHolder(t, svc, session); got != held {
				t.Fatalf("the name %s is held by %q, want the first principal %q: an enrolment binds the name it enrols", session, got, held)
			}
			grant := insertLiveGrant(t, svc, owner.ID)
			if err := svc.Revoke(ctx, first, owner.ID.String(), "launcher"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}

			_, err = svc.Create(ctx, second, Enrollment{Kind: "session", RuntimeID: "box-2", Thumbprint: "tp-box-2", SessionID: str(session)})
			if !errors.Is(err, ErrSessionHeld) {
				t.Fatalf("the second principal's enrollment = %v, want ErrSessionHeld: a session name belongs to one principal", err)
			}
			if !strings.Contains(err.Error(), session) {
				t.Fatalf("the refusal reads %q, want it to name the session %s the caller reached for", err, session)
			}
			// The refusal gives the refused caller nothing of whoever holds the name: neither the
			// principal the broker recorded nor the login or account inside it, which is the half
			// a reader would recognise. What the refusal says beyond the session is nobody's to
			// depend on, so none of its wording is pinned here.
			_, identity, split := strings.Cut(held, ":")
			if !split || identity == "" {
				t.Fatalf("the first principal reads %q, want <kind>:<identity>: both halves are measured against the refusal", held)
			}
			for _, leaked := range []string{held, identity} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("the refusal reads %q, want nothing of who holds the name in it (%q)", err, leaked)
				}
			}
			if rows := enrollmentsNaming(t, svc, session); rows != 1 {
				t.Fatalf("%d enrollments name %s, want the first principal's alone: a refused enrolment writes nothing", rows, session)
			}
			if got := sessionNameHolder(t, svc, session); got != held {
				t.Fatalf("the name %s is held by %q after the refusal, want the first principal %q still", session, got, held)
			}
			if holder := grantHolder(t, svc, grant); holder != owner.ID {
				t.Fatalf("grant %s moved to enrollment %s, want it left on %s: a grant follows one principal's name and no other's", grant, holder, owner.ID)
			}
			if left := readCarryState(t, svc, owner.ID); !left.Dormant || left.LiveGrants != 1 {
				t.Fatalf("the first principal's enrollment is %+v, want it still dormant holding its grant: the refused box took nothing, so the carry sweep is what ends it", left)
			}
		})
	}
}

// TestTheSamePrincipalsSecondBoxTakesTheNameAndWhatItHeld is the boundary this design accepts:
// within one principal the name is asserted and not proved, so the same person's second box
// enrolling the same name is refused nothing and adopts what the box it replaced held. That is
// the carry itself, and binding the name must not stand in its way.
func TestTheSamePrincipalsSecondBoxTakesTheNameAndWhatItHeld(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	session := "one-principal-two-boxes"
	cred, old := carryEnrollment(t, svc, issuer, key, "session", session)
	grant := insertLiveGrant(t, svc, old.ID)
	if err := svc.Revoke(ctx, cred, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, moved := movedTo(t, svc, issuer, key, "session", session, session+"-2")
	if holder := grantHolder(t, svc, grant); holder != moved.ID {
		t.Fatalf("grant %s hangs off %s, want the second box's enrollment %s: one principal's second box takes the name and what it held", grant, holder, moved.ID)
	}
	if got, want := sessionNameHolder(t, svc, session), carryPrincipalOf(t, svc, moved.ID); got != want {
		t.Fatalf("the name %s is held by %q, want %q: both boxes are the one principal's", session, got, want)
	}
}

// TestAnEnrollmentNamingNoSessionBindsNothing: the pair a grant follows is a name and a
// principal, so an enrolment that states no name has no name to bind. Nothing is written to the
// table of holders and nothing is refused, however many such enrolments one credential makes.
func TestAnEnrollmentNamingNoSessionBindsNothing(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	var bound int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from oauth_session_bindings`).Scan(&bound); err != nil {
		t.Fatalf("count the session names held: %v", err)
	}
	for i, name := range []*string{nil, str("")} {
		runtime := "no-name-" + sessionIDName(name)
		if _, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtime, Thumbprint: "tp-" + runtime, SessionID: name}); err != nil {
			t.Fatalf("Create(session id %s): %v, want it refused nothing: an enrolment that names no session binds no name", sessionIDName(name), err)
		}
		var now int
		if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from oauth_session_bindings`).Scan(&now); err != nil {
			t.Fatalf("count the session names held: %v", err)
		}
		if now != bound {
			t.Fatalf("after enrolment %d, which named no session, %d session names are held, want the %d held before it", i+1, now, bound)
		}
	}
}

// TestAPodsEnrollmentAndTheExchangeHoldItsNameUnderTheSameAccount is the one thing this refusal
// may not break. A Legion role's pod trades a token under its session name before or after it
// enrols under that same name: the exchange binds the name to exchange.AccountPrincipal of the
// ServiceAccount subject the pod's projected token proved (exchange.Set.Pod), and the enrolment
// binds it to exchange.AccountPrincipal of the subject the same kind of token proved
// (carryPrincipal). Those are one string, and this measures it both ways — by comparing the
// principal the exchange decides with the one the enrolment recorded, and by letting each of the
// two claim a name the other has already claimed, which the table itself would refuse if they
// disagreed.
func TestAPodsEnrollmentAndTheExchangeHoldItsNameUnderTheSameAccount(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	const subject = "system:serviceaccount:legion:worker"
	// The exchange as the cluster's deployment lists this account: the entry fixes no session
	// name, so the name the pod asks under is the name it writes, which is the name it enrols.
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: subject, Project: "legion", Person: "ada@example.com"}}, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	podToken := func(uid string) map[string]any {
		return map[string]any{"sub": subject, "kubernetes.io": map[string]any{"pod": map[string]any{"uid": uid}}}
	}
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	binder := oauthstore.New(svc.Store.Pool)
	hour := time.Now().Add(time.Hour)

	t.Run("the enrolment records the principal the exchange decides", func(t *testing.T) {
		const session, uid = "implementer-g1", "pod-uid-enrolled-first"
		enr, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, subject, uid)})
		if err != nil {
			t.Fatalf("the pod's enrolment: %v", err)
		}
		claims, err := set.Pod(podToken(uid), session)
		if err != nil {
			t.Fatalf("the exchange's reading of the same pod's token: %v", err)
		}
		recorded := carryPrincipalOf(t, svc, enr.ID)
		if claims.Principal != recorded {
			t.Fatalf("the exchange calls this pod %q and its enrolment calls it %q, want one spelling: a pod that has exchanged under its name must not be refused that name by its own enrolment",
				claims.Principal, recorded)
		}
		if got := sessionNameHolder(t, svc, session); got != claims.Principal {
			t.Fatalf("the name %s is held by %q, want the exchange's own %q", session, got, claims.Principal)
		}
		// And the exchange's own claim of the name the enrolment has bound is no refusal.
		if _, err := binder.BindSession(ctx, session, claims.Principal, false, hour); err != nil {
			t.Fatalf("the pod's trade under the name it enrolled = %v, want it claimed: the pod exchanges under its own name after enrolling", err)
		}
	})

	t.Run("the pod enrols the name it has already traded under", func(t *testing.T) {
		const session, uid = "implementer-g2", "pod-uid-traded-first"
		claims, err := set.Pod(podToken(uid), session)
		if err != nil {
			t.Fatalf("the exchange's reading of the pod's token: %v", err)
		}
		if _, err := binder.BindSession(ctx, session, claims.Principal, false, hour); err != nil {
			t.Fatalf("the pod's first trade: %v", err)
		}
		enr, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, subject, uid)})
		if err != nil {
			t.Fatalf("the pod's enrolment after its trade = %v, want it enrolled: the name is already its own", err)
		}
		if got := carryPrincipalOf(t, svc, enr.ID); got != claims.Principal {
			t.Fatalf("the enrolment records %q, want the principal its trade bound the name to, %q", got, claims.Principal)
		}
	})
}

// TestARevokedSignInTradesNothingSoItsBoxReachesNoEnrollment: a box whose key the person revoked
// never reaches an enrolment to be refused one. The revoke ends both halves at once
// (RevokePersonSignIn): the sign-in row, so the box's next trade is store.ErrSignInRevoked and it
// is minted no credential to enrol with, and the launcher credential that sign-in's trades had
// already minted, so the credential it still holds authenticates nothing either
// (ErrUnauthenticated). Nothing about a session name is involved in either.
func TestARevokedSignInTradesNothingSoItsBoxReachesNoEnrollment(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "signin-box")
	signIn := insertSignIn(t, svc, cred, "ada@example.com")
	var thumbprint string
	if err := svc.Store.Pool.QueryRow(ctx, `select key_thumbprint from launcher_credentials where id=$1`, cred.ID).Scan(&thumbprint); err != nil {
		t.Fatalf("read the box key's thumbprint: %v", err)
	}
	if _, err := svc.RevokePersonSignIn(ctx, signIn, "ada@example.com", "human:ada@example.com"); err != nil {
		t.Fatalf("RevokePersonSignIn: %v", err)
	}

	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := store.RecordPersonSignIn(ctx, tx, "ada@example.com", thumbprint, "fleet@example.iam.gserviceaccount.com"); !errors.Is(err, store.ErrSignInRevoked) {
		t.Fatalf("the revoked box key's next trade = %v, want %v: the box trades nothing, so it is minted no credential to enrol with", err, store.ErrSignInRevoked)
	}
	if _, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "revoked-box", Thumbprint: "tp-revoked-box", SessionID: str("revoked-box-session")}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an enrolment under the revoked sign-in's credential = %v, want ErrUnauthenticated", err)
	}
	if got := sessionNameHolder(t, svc, "revoked-box-session"); got != "" {
		t.Fatalf("the name revoked-box-session is held by %q, want nobody: the refused enrolment bound nothing", got)
	}
}
