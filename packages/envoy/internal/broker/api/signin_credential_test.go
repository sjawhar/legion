// signin_credential_test.go pins the one rule that keeps the two lists Dispatch shows a person
// apart: a person's sign-in on a machine holds a launcher credential of its own, named for the
// machine's fleet, and that credential belongs to the sign-ins list and its own revoke route and
// never to the machine-logins list — where, before this, anyone signed in could end it and the
// sign-in row behind it would mint it again at the box's next trade, so the revoke did nothing.
package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

// signInCredentialFleet is the fleet the box of these rows belongs to, and signInCredentialHost
// the host its credential records, as the exchange writes one.
const (
	signInCredentialFleet = "example-fleet"
	signInCredentialHost  = "example-project/example-zone-a/example-vm-7"
)

// signInCredential writes the launcher credential a person's sign-in holds, exactly as the token
// exchange mints one: keyed by exchange.PersonSubject (the person, the machine's Google account
// and the box key), naming the fleet's service, bound to the box key and operated by the person.
// It answers the credential's id.
func (ts *testServer) signInCredential(t *testing.T, person, thumbprint, machine string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := ts.Store.Pool.Exec(context.Background(), `insert into launcher_credentials
		(id, operator, service, subject, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,$3,$4,$5,$6,'{}'::jsonb, now() + interval '1 hour')`,
		id, person, signInCredentialFleet, exchange.PersonSubject(person, machine, thumbprint),
		signInCredentialHost, thumbprint); err != nil {
		t.Fatalf("insert the sign-in's launcher credential: %v", err)
	}
	return id
}

// TestASignInsCredentialIsNoMachineLogin: the credential a person's sign-in holds is on neither
// person's machine-logins list — not the person's own, whose sign-in it is, and not anyone
// else's, though it names a service and every service's login is listed for everyone — and the
// machine-logins revoke does not find it, so no caller can make the no-op revoke that left the
// sign-in alive. A machine's own credential, which is a machine login, is listed as before.
func TestASignInsCredentialIsNoMachineLogin(t *testing.T) {
	ts := newTestServer(t)
	ts.signIn(t, signInAda, adaBoxOne, signInFleetOne)
	credentialID := ts.signInCredential(t, signInAda, adaBoxOne, signInFleetOne)

	for _, person := range []string{signInAda, signInGrace} {
		for _, login := range ts.machineLogins(t, person) {
			if login.CredentialID == credentialID {
				t.Fatalf("the machine logins of %s hold the sign-in's credential %s: a sign-in is listed as a sign-in and nowhere else", person, credentialID)
			}
		}
	}

	for _, person := range []string{signInAda, signInGrace} {
		status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver",
			map[string]any{"approver": person})
		if refusal := decode[wireError](t, body); status != http.StatusNotFound || refusal.Code != "NOT_FOUND" {
			t.Fatalf("revoking the sign-in's credential as %s = %d %s, want 404 NOT_FOUND: the machine-logins revoke must not find it, since ending it alone ends nothing", person, status, body)
		}
	}
	if revoked := ts.credentialRevoked(t, credentialID); revoked {
		t.Fatal("the sign-in's credential is revoked after the refusals, want it untouched")
	}
}

// TestRevokingASignInEndsTheCredentialItHolds: the sign-ins route is the one that ends a sign-in
// whole — the row, so the box's next trade is answered nothing, and the credential it held, so
// the box reaches no secret in the meantime.
func TestRevokingASignInEndsTheCredentialItHolds(t *testing.T) {
	ts := newTestServer(t)
	ts.signIn(t, signInAda, adaBoxOne, signInFleetOne)
	credentialID := ts.signInCredential(t, signInAda, adaBoxOne, signInFleetOne)
	rows := ts.personSignIns(t, signInAda)
	if len(rows) != 1 {
		t.Fatalf("the sign-ins of %s = %+v, want the one just written", signInAda, rows)
	}

	status, body := ts.ui(t, http.MethodPost, "/v1/person-signins/"+rows[0].SignInID+"/revoke",
		map[string]any{"person": signInAda})
	if status != http.StatusOK {
		t.Fatalf("revoking the sign-in = %d %s, want 200", status, body)
	}
	if !ts.credentialRevoked(t, credentialID) {
		t.Fatalf("the credential %s the sign-in held is still live after the sign-in was revoked: the sign-ins route is what ends a sign-in whole", credentialID)
	}
	if rows := ts.personSignIns(t, signInAda); len(rows) != 0 {
		t.Fatalf("the sign-ins of %s after the revoke = %+v, want none", signInAda, rows)
	}
}

// credentialRevoked reads whether launcher credential id is revoked, straight from the store: no
// route answers it for a credential neither list holds.
func (ts *testServer) credentialRevoked(t *testing.T, id string) bool {
	t.Helper()
	var revoked bool
	if err := ts.Store.Pool.QueryRow(context.Background(),
		`select revoked_at is not null from launcher_credentials where id = $1`, id).Scan(&revoked); err != nil {
		t.Fatalf("read whether %s is revoked: %v", id, err)
	}
	return revoked
}
