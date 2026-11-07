// session_carry_test.go drives the REAL compiled broker on a real Postgres and measures one
// thing: a session's grant, and its pending request, follow the session to a new box
// (dispatch://INFRA-1167). A session is a name — the session_id its launcher states at
// POST /v1/enrollments — and a box is whatever holds the launcher credential that enrols it. The
// box is replaced far more often than the session it runs: a helper restarts, a machine is
// rebuilt, a person signs in on a second machine. Today each of those enrols the session again
// under a new enrollment row, and the grant a person approved stays with the row the first box
// made: the moved session is refused its own grant (requests.Machine.Values, "this request or
// grant belongs to another session"), and ending the old box's enrollment revokes that grant and
// cancels whatever it still had pending (enroll.endEnrollment), so the session asks a person
// again for a secret that person has already approved.
//
// Nothing here is a unit test. The binary is the one cmd/broker builds, the schema is a fresh one
// on the test server, every request crosses a socket, each approval is a person signing in to the
// broker's own page and pressing Approve, and the row counts are read with a database handle of
// the test's own.
//
// Three shapes of "a new box", each on a session name of its own, because they differ in what the
// broker can tell them apart by, and the carry has to hold for all three:
//
//   - The same machine credential with a new runtime id: a machine's own session restarting on the
//     same box. The identity key is a file on that box's disk, so the exchange moves the one
//     credential the machine already holds rather than minting another (exchange.MachineSubject),
//     and only the runtime id is new.
//   - A new machine credential of the same Google account: the box is replaced. A second identity
//     token of the same fleet account, presented under a key of its own, mints a second launcher
//     credential, because the key is part of the subject a credential is keyed by.
//   - A person's second sign-in: the person signs in on a second box, under a second box key, and
//     holds a credential per box (exchange.PersonSubject) with one login behind both.
//
// What every carry arm asserts is the same: the session, proving the enrollment the new box made,
// reads the same value from the same grant id, and the broker holds exactly one requests row and
// one grants row for the session name — the grant moved rather than a second one being asked for.
// Two arms beside them take the orderings the incident takes: the old box's session ending before
// the new one enrols, where the grant must still be live for the new enrollment to take, and a
// request left pending across the move, where the approval has to land on the session as it
// stands then. The last arm is the rule none of this may weaken: another session name, on either
// box, is refused that grant.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The session names this test's arms move from box to box, one per arm, so no arm's rows are
// another's: every count below is taken by session name. A name belongs to one principal
// (exchange.Set), and the principal here is the fleet's Google account for a machine's own
// session and the person for a sign-in, so each name is claimed once and traded again under the
// same principal however many boxes hold it.
const (
	carryRestartSession = "carry-same-credential"
	carryRebuildSession = "carry-new-credential"
	carrySignInSession  = "carry-second-signin"
	carryDormantSession = "carry-old-box-ended"
	carryPendingSession = "carry-pending-request"
	carryStrangerOwner  = "carry-stranger-owner"
	carryStranger       = "carry-stranger"
)

// TestASessionsGrantFollowsItToANewBox is the live measurement of the carry: in each of the three
// shapes a new box takes, a session enrols, asks for the secret a person owns, has that person
// approve it on the broker's own page, reads the value, and then enrols the same session name on
// the new box — where it must read the same value from the same grant, with no second request and
// no second grant. Beside them: the old box's session ending before the new one enrols, a request
// left pending across the move, and the refusal that must stand, another session name reaching
// for that grant.
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestASessionsGrantFollowsItToANewBox(t *testing.T) {
	databaseURL := storetest.URL(t)
	// Three issuers this test controls: the cluster's, GitHub's, and the one standing in for
	// Google, which the machine grant and the person sign-in both trade tokens of. The cluster's
	// issuer serves the approval page's provider too, as the approval page's own fixture has one
	// issuer serve both (approval_page_test.go), which is what lets a person sign in and press
	// Approve against this broker.
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	github.PublishKey(t, "github-key")
	googleKey := google.PublishKey(t, "google-key")
	brokertest.SignIn(cluster, cluster.PublishKey(t, "signin-key"))

	// productionEnv already names the broker's own sign-in client, its secret file and the
	// Workspace domain every approver here belongs to, so the issuer is the one setting this
	// broker needs beyond the exchange's own.
	broker := startBroker(t, brokerOptions{Env: append(
		googleExchangeEnv(t, databaseURL, cluster, github, google, longAccessSeconds),
		"BROKER_SIGNIN_ISSUER="+cluster.URL())})
	requireHealthy(t, broker.Addr)

	ctx := context.Background()
	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the broker's database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })

	// Each subtest reads and writes through a rig of its own, holding that subtest's t: a
	// failure a helper reports belongs to the arm that drove it.
	newRig := func(t *testing.T) carryRig {
		return carryRig{t: t, ctx: ctx, addr: broker.Addr, base: "http://" + broker.Addr, db: db,
			google: google, googleKey: googleKey, signIn: cluster}
	}

	for _, shape := range []struct {
		name    string
		session string
		// The secret the session asks for, what the store holds for it, and the person who
		// approves it. A machine's own session acts for its fleet's service, so the secret the
		// fleet's own person owns is the one it must wait for; a person's signed-in session is
		// that person, so the one it must wait for is another person's.
		secret, value, approver string
		// boxes answers the box the session enrols on first and the box it moves to, in the
		// shape's own way, and asserts what makes that shape the shape it is.
		boxes func(rig carryRig) (box, box)
	}{
		{
			name: "the same machine credential with a new runtime id", session: carryRestartSession,
			secret: personSecret, value: personSecretValue, approver: fleetPerson,
			boxes: func(rig carryRig) (box, box) {
				// One box, one identity key, two trades: the helper trades again when its process
				// restarts, reading the same identity key back from the same disk
				// (helper.LoadOrMakeIdentityKey), so the exchange moves the credential the machine
				// already holds under the same id.
				key := proofKey(rig.t)
				first := rig.machineCredential(key, carryRestartSession)
				again := rig.machineCredential(key, carryRestartSession)
				if again != first {
					rig.t.Fatalf("the machine's second trade under its own identity key minted credential %s beside %s, want the one credential moved: this shape is the restart that keeps its key", again, first)
				}
				return box{key: key, credential: first}, box{key: key, credential: again}
			},
		},
		{
			name: "a new machine credential of the same Google account", session: carryRebuildSession,
			secret: personSecret, value: personSecretValue, approver: fleetPerson,
			boxes: func(rig carryRig) (box, box) {
				// The box is replaced: the same fleet account, a key of the new box's own, and so
				// a launcher credential of its own, since the key is part of the subject a
				// credential is keyed by (exchange.MachineSubject).
				firstKey, secondKey := proofKey(rig.t), proofKey(rig.t)
				first := rig.machineCredential(firstKey, carryRebuildSession)
				second := rig.machineCredential(secondKey, carryRebuildSession)
				if second == first {
					rig.t.Fatalf("both machines hold the credential %s, want one each: a rebuilt box trades a key of its own", first)
				}
				return box{key: firstKey, credential: first}, box{key: secondKey, credential: second}
			},
		},
		{
			name: "a person's second sign-in", session: carrySignInSession,
			secret: otherPersonSecret, value: otherPersonSecretValue, approver: person,
			boxes: func(rig carryRig) (box, box) {
				// One person, two boxes: `agent-secrets login` makes a box key per box, and the
				// sign-in on each holds a credential of its own, with the one login behind both.
				firstKey, secondKey := proofKey(rig.t), proofKey(rig.t)
				first := rig.signInCredential(firstKey, carrySignInSession)
				second := rig.signInCredential(secondKey, carrySignInSession)
				if second == first {
					rig.t.Fatalf("both sign-ins hold the credential %s, want one each: a sign-in is keyed on the person, the machine and the box key", first)
				}
				return box{key: firstKey, credential: first}, box{key: secondKey, credential: second}
			},
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			rig := newRig(t)
			first, second := shape.boxes(rig)
			old, grant := rig.grantOn(first, shape.session, shape.session+"-1", shape.secret, shape.value, shape.approver)

			moved := rig.enrol(second.key, second.credential, shape.session+"-2", shape.session)
			if moved.enrollmentID == old.enrollmentID {
				t.Fatalf("the new box was answered the old box's enrollment %s, so this arm measures nothing: the move must make an enrollment of its own", old.enrollmentID)
			}
			rig.requireValue(moved, grant, shape.secret, shape.value,
				"a session's grant follows the session name to the box it enrols on next, under the enrollment that box made")

			held := rig.held(shape.session)
			if held.Requests != 1 || held.Grants != 1 || held.Grant != grant {
				t.Fatalf("after the move the broker holds %+v for session %s, want one request and one grant, the grant still %s: the moved session takes the grant it had rather than asking %s again",
					held, shape.session, grant, shape.approver)
			}
			if held.GrantEnrollment != moved.enrollmentID {
				t.Fatalf("grant %s hangs off enrollment %s, want the moved session's %s: a grant that follows its session is held by the enrollment that session proves now",
					grant, held.GrantEnrollment, moved.enrollmentID)
			}

			// The old box stops last: its helper ends the session it enrolled once the new box is
			// up. That end must take nothing the session still holds.
			rig.endSession(first.key, first.credential, old)
			rig.requireValue(moved, grant, shape.secret, shape.value,
				"ending the enrollment the session moved off takes nothing from the session: that enrollment holds no grant any more")
		})
	}

	t.Run("the old box's session ends before the new box enrols", func(t *testing.T) {
		// The ordering the incident takes: the box is gone before its session comes up again
		// elsewhere, so what the new enrollment adopts is whatever the ended one left. Today the
		// end revokes the grant, which is why the arm reads the grant's own row before it enrols
		// anything: a revoked grant is nothing for a new enrollment to take.
		rig := newRig(t)
		firstKey, secondKey := proofKey(t), proofKey(t)
		firstCredential := rig.machineCredential(firstKey, carryDormantSession)
		old, grant := rig.grantOn(box{key: firstKey, credential: firstCredential},
			carryDormantSession, carryDormantSession+"-1", personSecret, personSecretValue, fleetPerson)

		rig.endSession(firstKey, firstCredential, old)
		if held := rig.held(carryDormantSession); !held.GrantsLive {
			t.Fatalf("after the old box's session ended the broker holds %+v for session %s, want its grant live: a session that ends on one box and comes up on another has not given its grant up",
				held, carryDormantSession)
		}

		secondCredential := rig.machineCredential(secondKey, carryDormantSession)
		moved := rig.enrol(secondKey, secondCredential, carryDormantSession+"-2", carryDormantSession)
		rig.requireValue(moved, grant, personSecret, personSecretValue,
			"the session comes up on the new box holding what it held on the old one, whichever of the two ended first")
		if held := rig.held(carryDormantSession); held.Requests != 1 || held.Grants != 1 || held.GrantEnrollment != moved.enrollmentID {
			t.Fatalf("the broker holds %+v for session %s, want one request and one grant held by the moved session's enrollment %s",
				held, carryDormantSession, moved.enrollmentID)
		}
	})

	t.Run("a request left pending across the move is granted to the moved session", func(t *testing.T) {
		// The pending half. The person is asked while the session runs on the first box and
		// answers after it has moved, which is minutes later and a box later: the approval has to
		// land on the session as it stands then, not on the row that asked.
		rig := newRig(t)
		firstKey, secondKey := proofKey(t), proofKey(t)
		firstCredential := rig.machineCredential(firstKey, carryPendingSession)
		secondCredential := rig.machineCredential(secondKey, carryPendingSession)
		if secondCredential == firstCredential {
			t.Fatalf("both boxes hold the credential %s, want one each", firstCredential)
		}
		pods := rig.pods()

		old := rig.enrol(firstKey, firstCredential, carryPendingSession+"-1", carryPendingSession)
		asked := pods.ask(old, personSecret)
		if asked.State != "pending" || asked.RecordID == nil {
			t.Fatalf("the session's request for %s = %+v, want it pending on a record %s decides", personSecret, asked, fleetPerson)
		}

		moved := rig.enrol(secondKey, secondCredential, carryPendingSession+"-2", carryPendingSession)
		rig.endSession(firstKey, firstCredential, old)
		held := rig.held(carryPendingSession)
		if held.Requests != 1 || held.RequestStates != "pending" {
			t.Fatalf("after the move the broker holds %+v for session %s, want its one request still pending: a request waiting on a person outlives the box it was made on",
				held, carryPendingSession)
		}
		if held.RequestEnrollment != moved.enrollmentID {
			t.Fatalf("the pending request hangs off enrollment %s, want the moved session's %s: an approval lands on the session, which is now on the new box",
				held.RequestEnrollment, moved.enrollmentID)
		}

		brokertest.Decide(t, rig.base, cluster, fleetPerson, *asked.RecordID, true)
		held = rig.held(carryPendingSession)
		if held.Grants != 1 || held.GrantEnrollment != moved.enrollmentID {
			t.Fatalf("%s's approval left the broker holding %+v for session %s, want its one grant on the moved session's enrollment %s",
				fleetPerson, held, carryPendingSession, moved.enrollmentID)
		}
		rig.requireValue(moved, held.Grant, personSecret, personSecretValue,
			"the grant a person approves after the move is the moved session's to read")
	})

	t.Run("another session name is refused the grant on either box", func(t *testing.T) {
		// The rule the carry must not weaken, and the one arm here that holds at this head too:
		// what follows a session is that session's name under one principal, so a second session
		// of the very same fleet, on the very same box or on the new one, reaches none of it.
		rig := newRig(t)
		firstKey, secondKey := proofKey(t), proofKey(t)
		firstCredential := rig.machineCredential(firstKey, carryStrangerOwner)
		owner, grant := rig.grantOn(box{key: firstKey, credential: firstCredential},
			carryStrangerOwner, carryStrangerOwner+"-1", personSecret, personSecretValue, fleetPerson)
		secondCredential := rig.machineCredential(secondKey, carryStranger)

		for _, stranger := range []struct {
			name string
			pod  pod
		}{
			{"on the box the grant was approved on", rig.enrol(firstKey, firstCredential, carryStranger+"-1", carryStranger)},
			{"on the box the session would move to", rig.enrol(secondKey, secondCredential, carryStranger+"-2", carryStranger)},
		} {
			status, answer := rig.readValues(stranger.pod, grant)
			if status != http.StatusForbidden || decodeError(t, answer).Code != "NOT_YOURS" {
				t.Fatalf("session %s %s reading %s's grant %s = %d %s, want 403 NOT_YOURS: a grant follows one session name and no other",
					carryStranger, stranger.name, carryStrangerOwner, grant, status, bytes.TrimSpace(answer))
			}
		}
		rig.requireValue(owner, grant, personSecret, personSecretValue,
			"the refusals took nothing from the session the grant belongs to")
	})
}

// box is one box as this test drives it: the launcher credential it holds and the key that proves
// it. A machine's box holds the credential its token exchange minted against its identity key; a
// person's box holds the one their sign-in minted against the box key `agent-secrets login` made.
type box struct {
	key        *ecdsa.PrivateKey
	credential string
}

// carryRig is the one running broker this test drives, everything it needs to reach it, and the
// database handle it counts rows with. Each arm builds one holding its own t.
type carryRig struct {
	t    *testing.T
	ctx  context.Context
	addr string
	base string
	db   *pgx.Conn
	// The issuer standing in for Google, and the key it signs with: the machine identity tokens
	// and the person identity tokens the exchange trades are minted here.
	google    *oidctest.Issuer
	googleKey *oidctest.Key
	// The approval page's provider, where the person who owns a secret signs in to decide it.
	signIn *oidctest.Issuer
}

// pods is the podBroker every enrolment, request and release here goes through, the one the
// machine half of the token exchange uses (token_exchange_test.go). It needs no issuer of its own:
// nothing here proves a projected pod token, since a session enrols on the launcher credential
// the exchange minted and nothing else.
func (r carryRig) pods() podBroker {
	return podBroker{t: r.t, base: r.base}
}

// machineCredential trades a machine's own Google identity token, with a DPoP proof of key, for
// the launcher credential the exchange mints for it: the whole of what a machine does before it
// enrols anything. The machine is the pinned fleet's, on the instance its entry allows.
func (r carryRig) machineCredential(key *ecdsa.PrivateKey, session string) string {
	r.t.Helper()
	status, body := newLiveBroker(r.addr, key).exchange(r.t, machineForm(
		mintMachineToken(r.t, r.google, r.googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName), session))
	credential, _ := body[oauthwire.ExtraCredentialID].(string)
	if status != http.StatusOK || credential == "" {
		r.t.Fatalf("the machine's exchange for session %s = %d %v, want 200 with the launcher credential it mints", session, status, body)
	}
	return credential
}

// signInCredential trades the person's own Google identity token, with the machine's as the actor
// token and a DPoP proof of the box key, for the launcher credential their sign-in holds: what
// `agent-secrets login` leaves on one box.
func (r carryRig) signInCredential(key *ecdsa.PrivateKey, session string) string {
	r.t.Helper()
	status, body := newLiveBroker(r.addr, key).exchange(r.t, signInForm(
		mintPersonToken(r.t, r.google, r.googleKey, fleetPerson, deviceAudience, true, signInDomain),
		mintMachineToken(r.t, r.google, r.googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName),
		session))
	credential, _ := body[oauthwire.ExtraCredentialID].(string)
	if status != http.StatusOK || credential == "" {
		r.t.Fatalf("%s's sign-in trade for session %s = %d %v, want 200 with the launcher credential the sign-in holds", fleetPerson, session, status, body)
	}
	return credential
}

// enrol has the launcher credential of b enrol a session of the name sessionID under the runtime
// id runtimeID, as a helper does at every start: a key of the session's own, and the session name
// in the enrol body, which is the name a grant follows. podBroker.enrolSession states no session
// name, and the name is the whole subject here, so this is the enrolment every arm makes.
func (r carryRig) enrol(launcherKey *ecdsa.PrivateKey, credentialID, runtimeID, sessionID string) pod {
	r.t.Helper()
	pods := r.pods()
	key := proofKey(r.t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		r.t.Fatal(err)
	}
	signed, err := proof.SignLauncher(launcherKey, credentialID, http.MethodPost, r.base+"/v1/enrollments", time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	status, answer := pods.post("/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "session", "runtime_id": runtimeID, "thumbprint": thumbprint, "session_id": sessionID,
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	pods.decode(answer, &enrolled)
	if status != http.StatusCreated || enrolled.EnrollmentID == "" {
		r.t.Fatalf("enrolling session %s as runtime %s under credential %s = %d %s, want 201 with an enrollment_id", sessionID, runtimeID, credentialID, status, answer)
	}
	return pod{runtimeID: runtimeID, key: key, enrollmentID: enrolled.EnrollmentID}
}

// grantOn is the state every arm starts from: a session of the name sessionID enrols on the box b,
// asks for secret, waits (nobody but approver can answer), approver approves it on the broker's own
// page, and the session reads the value back. It answers the session and the grant id it now holds.
func (r carryRig) grantOn(b box, sessionID, runtimeID, secret, value, approver string) (pod, string) {
	r.t.Helper()
	pods := r.pods()
	session := r.enrol(b.key, b.credential, runtimeID, sessionID)
	asked := pods.ask(session, secret)
	if asked.State != "pending" || asked.RecordID == nil || asked.GrantID != nil {
		r.t.Fatalf("session %s asking for %s = %+v, want it pending with no grant on a record %s decides", sessionID, secret, asked, approver)
	}
	brokertest.Decide(r.t, r.base, r.signIn, approver, *asked.RecordID, true)
	held := r.held(sessionID)
	if held.Requests != 1 || held.Grants != 1 {
		r.t.Fatalf("after %s approved, the broker holds %+v for session %s, want one request and one grant", approver, held, sessionID)
	}
	if got := pods.values(session, held.Grant)[secret]; got != value {
		r.t.Fatalf("session %s read %s = %q from grant %s, want %q", sessionID, secret, got, held.Grant, value)
	}
	return session, held.Grant
}

// readValues has p release grantID's values and answers what the broker said, refusal and all: the
// one call every carry arm turns on.
func (r carryRig) readValues(p pod, grantID string) (int, []byte) {
	r.t.Helper()
	return r.pods().signed(p, "/v1/grants/"+grantID+"/values", nil)
}

// requireValue fails unless p reads secret=value from grantID, quoting the broker's own answer and
// saying what that read proves. proves is the sentence the arm is making, so a failure says which
// of them the broker refused.
func (r carryRig) requireValue(p pod, grantID, secret, value, proves string) {
	r.t.Helper()
	status, answer := r.readValues(p, grantID)
	if status != http.StatusOK {
		r.t.Fatalf("the session (runtime %s, enrollment %s) reading grant %s = %d %s, want 200 with %s: %s",
			p.runtimeID, p.enrollmentID, grantID, status, bytes.TrimSpace(answer), secret, proves)
	}
	var released struct {
		Values map[string]string `json:"values"`
	}
	r.pods().decode(answer, &released)
	if got := released.Values[secret]; got != value {
		r.t.Fatalf("the session (runtime %s, enrollment %s) read %s = %q from grant %s, want %q: %s",
			p.runtimeID, p.enrollmentID, secret, got, grantID, value, proves)
	}
}

// endSession ends p's enrollment as the launcher that made it does, DELETE /v1/enrollments/{id}
// under a launcher proof. It is the step the incident turns on: ending an enrollment is what
// revokes its grants and cancels its pending requests (enroll.endEnrollment).
func (r carryRig) endSession(launcherKey *ecdsa.PrivateKey, credentialID string, p pod) {
	r.t.Helper()
	path := "/v1/enrollments/" + p.enrollmentID
	signed, err := proof.SignLauncher(launcherKey, credentialID, http.MethodDelete, r.base+path, time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodDelete, r.base+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Proof", signed)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.t.Fatalf("DELETE %s: %v", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Logf("> DELETE %s\n< %d %s", path, response.StatusCode, bytes.TrimSpace(answer))
	if response.StatusCode != http.StatusNoContent {
		r.t.Fatalf("ending enrollment %s = %d %s, want 204", p.enrollmentID, response.StatusCode, answer)
	}
}

// sessionHeld is what the broker holds for one session name, as the test's own handle reads it:
// how many requests and grants rows hang off the enrollments carrying that name, which enrollment
// each hangs off, the request states, and whether every grant is still live. Grant and
// GrantEnrollment are each every row's value joined by commas, which is the one row's value while
// there is one, so a failure prints what the broker holds rather than one row of it.
type sessionHeld struct {
	Requests, Grants  int
	Grant             string
	GrantEnrollment   string
	GrantsLive        bool
	RequestStates     string
	RequestEnrollment string
}

// heldBySession reads every count and id sessionHeld carries in one statement. Every read is an
// aggregate, so a session holding two grants or none answers rather than failing the scan: what
// the broker holds is the arm's to judge.
const heldBySession = `select
	(select count(*) from requests r join enrollments e on e.id = r.enrollment_id where e.session_id = $1),
	(select count(*) from grants g join enrollments e on e.id = g.enrollment_id where e.session_id = $1),
	coalesce((select string_agg(g.id::text, ',' order by g.created_at) from grants g join enrollments e on e.id = g.enrollment_id where e.session_id = $1), ''),
	coalesce((select string_agg(g.enrollment_id::text, ',' order by g.created_at) from grants g join enrollments e on e.id = g.enrollment_id where e.session_id = $1), ''),
	coalesce((select bool_and(g.revoked_at is null) from grants g join enrollments e on e.id = g.enrollment_id where e.session_id = $1), false),
	coalesce((select string_agg(r.state, ',' order by r.created_at) from requests r join enrollments e on e.id = r.enrollment_id where e.session_id = $1), ''),
	coalesce((select string_agg(r.enrollment_id::text, ',' order by r.created_at) from requests r join enrollments e on e.id = r.enrollment_id where e.session_id = $1), '')`

// held is what the broker holds for the session name, read with the test's own handle on the
// schema storetest made for this test. The rows are read by session name and never by enrollment,
// because the name is the thing that is supposed to survive the move: a count taken per
// enrollment would answer 1 and 1 for a session whose grant was left behind on the box it moved
// off.
func (r carryRig) held(session string) sessionHeld {
	r.t.Helper()
	var held sessionHeld
	if err := r.db.QueryRow(r.ctx, heldBySession, session).Scan(&held.Requests, &held.Grants,
		&held.Grant, &held.GrantEnrollment, &held.GrantsLive, &held.RequestStates, &held.RequestEnrollment); err != nil {
		r.t.Fatalf("read what the broker holds for session %s: %v", session, err)
	}
	r.t.Logf("the broker holds %+v for session %s", held, session)
	return held
}
