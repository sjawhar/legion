// launcher_renewal_test.go drives POST /v1/launcher-credentials/{id}/renew, a service's machine
// login moving its own launcher credential to a new key and a new expiry, on the same real broker
// api_test.go mounts.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// wireRenewed is POST /v1/launcher-credentials/{id}/renew's answer.
type wireRenewed struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// credentialExpiry reads a launcher credential's expires_at from the broker's own row.
func (ts *testServer) credentialExpiry(t *testing.T, credentialID string) time.Time {
	t.Helper()
	var expires time.Time
	if err := ts.Store.Pool.QueryRow(context.Background(), `select expires_at from launcher_credentials where id=$1`, credentialID).Scan(&expires); err != nil {
		t.Fatalf("read launcher credential %s's expiry: %v", credentialID, err)
	}
	return expires
}

// renewTo renews credentialID, signed with current, onto next, which signs the request object
// naming service, as the Legion daemon's login does.
func (ts *testServer) renewTo(t *testing.T, current *ecdsa.PrivateKey, credentialID string, next *ecdsa.PrivateKey, service string) (int, []byte) {
	t.Helper()
	request := signMachineLoginRequest(t, next, ts.URL, "", "cluster.example", service)
	return ts.launcher(t, current, credentialID, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/renew", map[string]any{"request": request})
}

// TestAServiceLoginRenewsItsOwnCredentialOntoANewKey: the Legion daemon's service login, signed with
// its current key and carrying a request its new key signed, moves its credential to that key and to
// one full lifetime from the renewal, recorded in a launcher_credential.renewed audit row naming the
// credential as the actor and the new key. From then on the old key's proofs are refused and the new
// key's enroll pods under the same credential, which keeps its id.
func TestAServiceLoginRenewsItsOwnCredentialOntoANewKey(t *testing.T) {
	ts := newTestServer(t)
	credentialID, oldKey := ts.mintServiceLauncherCredential(t)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update launcher_credentials set expires_at = now() + interval '1 minute' where id=$1`, credentialID); err != nil {
		t.Fatalf("bring the credential close to its expiry: %v", err)
	}
	newKey := newSigningKey(t)

	before := time.Now()
	status, body := ts.renewTo(t, oldKey, credentialID, newKey, "legion-daemon")
	after := time.Now()
	if status != http.StatusOK {
		t.Fatalf("renew = %d %s, want 200", status, body)
	}
	renewed := decode[wireRenewed](t, body).ExpiresAt
	lifetime := 7 * 24 * time.Hour
	if renewed.Before(before.Add(lifetime).Add(-time.Second)) || renewed.After(after.Add(lifetime).Add(time.Second)) {
		t.Fatalf("renewed expires_at = %s, want one lifetime (%s) from the renewal, between %s and %s", renewed, lifetime, before.Add(lifetime), after.Add(lifetime))
	}
	if stored := ts.credentialExpiry(t, credentialID); !stored.Equal(renewed) {
		t.Fatalf("the credential's stored expires_at = %s, want the renewal's answer %s", stored, renewed)
	}
	if got := ts.column(t, `select actor || ' ' || (detail->>'service') || ' ' || (detail->>'key_thumbprint') from audit
		where kind='launcher_credential.renewed' and detail->>'credential_id'=$1`, credentialID); !slices.Equal(got, []string{record.LauncherActor(credentialID) + " legion-daemon " + thumbprintOf(t, newKey)}) {
		t.Fatalf("launcher_credential.renewed audit rows = %v, want one by launcher:%s naming legion-daemon and the new key", got, credentialID)
	}

	podUID := uuid.NewString()
	enroll := func(key *ecdsa.PrivateKey) (int, []byte) {
		t.Helper()
		return ts.launcher(t, key, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprintOf(t, newSigningKey(t)),
			"pod_token": ts.podToken(t, workerAccount, podUID),
		})
	}
	if status, body := enroll(oldKey); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("enroll with the old key after the renewal = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if status, body := enroll(newKey); status != http.StatusCreated {
		t.Fatalf("enroll with the new key after the renewal = %d %s, want 201", status, body)
	}
	if status, body := ts.renewTo(t, oldKey, credentialID, newSigningKey(t), "legion-daemon"); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("renew with the old key after the renewal = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	self := func(key *ecdsa.PrivateKey) (int, []byte) {
		t.Helper()
		return ts.launcher(t, key, credentialID, http.MethodGet, "/v1/launcher-credentials/self", nil)
	}
	if status, body := self(oldKey); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("GET /v1/launcher-credentials/self with the old key = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	status, body = self(newKey)
	described := decode[struct {
		CredentialID string    `json:"credential_id"`
		Service      *string   `json:"service"`
		ExpiresAt    time.Time `json:"expires_at"`
	}](t, body)
	if status != http.StatusOK || described.CredentialID != credentialID || described.Service == nil || *described.Service != "legion-daemon" || !described.ExpiresAt.Equal(renewed) {
		t.Fatalf("GET /v1/launcher-credentials/self with the new key = %d %s, want 200 naming the credential, legion-daemon and the renewed expiry", status, body)
	}
}

// TestARenewalRefusesWhatItMayNotExtend: another credential's id is 403 NOT_YOURS; a person's own
// machine login is 403 PERSON_CREDENTIAL, since its expiry asks its person to approve a new login; a
// service the broker does not register is 403 SERVICE_NOT_REGISTERED; a request naming another
// service, or carrying the credential's current key, is 400 REQUEST_INVALID;
// and a credential that is revoked, or past its expiry, is 401 LAUNCHER_INVALID, the answer every
// launcher route gives a credential that is not live. None of them moves a credential or writes a
// launcher_credential.renewed row.
func TestARenewalRefusesWhatItMayNotExtend(t *testing.T) {
	ts := newTestServer(t)
	daemon, daemonKey := ts.mintServiceLauncherCredential(t)
	daemonExpiry := ts.credentialExpiry(t, daemon)
	other, _ := ts.mintServiceLauncherCredential(t)
	otherExpiry := ts.credentialExpiry(t, other)
	if status, body := ts.launcher(t, daemonKey, daemon, http.MethodPost, "/v1/launcher-credentials/"+other+"/renew",
		map[string]any{"request": signMachineLoginRequest(t, newSigningKey(t), ts.URL, "", "cluster.example", "legion-daemon")}); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_YOURS" {
		t.Fatalf("renew another credential = %d %s, want 403 NOT_YOURS", status, body)
	}
	if status, body := ts.renewTo(t, daemonKey, daemon, newSigningKey(t), "another-service"); status != http.StatusBadRequest || decode[wireError](t, body).Code != "REQUEST_INVALID" {
		t.Fatalf("renew onto a request naming another service = %d %s, want 400 REQUEST_INVALID", status, body)
	}
	if status, body := ts.renewTo(t, daemonKey, daemon, daemonKey, "legion-daemon"); status != http.StatusBadRequest || decode[wireError](t, body).Code != "REQUEST_INVALID" {
		t.Fatalf("renew onto the current key = %d %s, want 400 REQUEST_INVALID", status, body)
	}
	// A login naming a service the broker does not register: anyone signed in may approve it, and
	// it lasts its one lifetime.
	impostorKey := newSigningKey(t)
	impostor := ts.approveMachineLogin(t, signMachineLoginRequest(t, impostorKey, ts.URL, "", "cluster.example", "not-registered"), testApprover)
	impostorExpiry := ts.credentialExpiry(t, impostor)
	if status, body := ts.renewTo(t, impostorKey, impostor, newSigningKey(t), "not-registered"); status != http.StatusForbidden || decode[wireError](t, body).Code != "SERVICE_NOT_REGISTERED" {
		t.Fatalf("renew a service the broker does not register = %d %s, want 403 SERVICE_NOT_REGISTERED", status, body)
	}
	if got := ts.credentialExpiry(t, impostor); !got.Equal(impostorExpiry) {
		t.Fatalf("the unregistered service's expiry moved from %s to %s", impostorExpiry, got)
	}
	for id, want := range map[string]time.Time{daemon: daemonExpiry, other: otherExpiry} {
		if got := ts.credentialExpiry(t, id); !got.Equal(want) {
			t.Fatalf("credential %s's expiry moved from %s to %s", id, want, got)
		}
	}

	person, personKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	personExpiry := ts.credentialExpiry(t, person)
	request := signMachineLoginRequest(t, newSigningKey(t), ts.URL, testApprover, "example-host-devbox", "")
	if status, body := ts.launcher(t, personKey, person, http.MethodPost, "/v1/launcher-credentials/"+person+"/renew", map[string]any{"request": request}); status != http.StatusForbidden || decode[wireError](t, body).Code != "PERSON_CREDENTIAL" {
		t.Fatalf("renew a person's machine login = %d %s, want 403 PERSON_CREDENTIAL", status, body)
	}
	if got := ts.credentialExpiry(t, person); !got.Equal(personExpiry) {
		t.Fatalf("the person's credential's expiry moved from %s to %s", personExpiry, got)
	}

	if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+daemon+"/revoke-by-approver", map[string]any{"approver": testApprover}); status != http.StatusOK {
		t.Fatalf("revoke the daemon's login = %d %s, want 200", status, body)
	}
	if status, body := ts.renewTo(t, daemonKey, daemon, newSigningKey(t), "legion-daemon"); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("renew a revoked credential = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}

	lapsed, lapsedKey := ts.mintServiceLauncherCredential(t)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update launcher_credentials set expires_at = now() - interval '1 minute' where id=$1`, lapsed); err != nil {
		t.Fatalf("expire the credential: %v", err)
	}
	lapsedExpiry := ts.credentialExpiry(t, lapsed)
	if status, body := ts.renewTo(t, lapsedKey, lapsed, newSigningKey(t), "legion-daemon"); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("renew an expired credential = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if got := ts.credentialExpiry(t, lapsed); !got.Equal(lapsedExpiry) {
		t.Fatalf("the expired credential's expiry moved from %s to %s", lapsedExpiry, got)
	}
	if got := ts.column(t, `select detail->>'credential_id' from audit where kind='launcher_credential.renewed'`); len(got) != 0 {
		t.Fatalf("launcher_credential.renewed audit rows = %v, want none: no renewal was allowed", got)
	}
}

// credentialKey reads a launcher credential's current key thumbprint from the broker's own row.
func (ts *testServer) credentialKey(t *testing.T, credentialID string) string {
	t.Helper()
	var thumbprint string
	if err := ts.Store.Pool.QueryRow(context.Background(), `select key_thumbprint from launcher_credentials where id=$1`, credentialID).Scan(&thumbprint); err != nil {
		t.Fatalf("read launcher credential %s's key: %v", credentialID, err)
	}
	return thumbprint
}

// TestTwoProofsThatAuthenticatedTheOldKeyCannotBothWin: two renewal requests signed with the same
// current key can both pass the handler's authentication before either locks the credential. Renew
// binds that proof's thumbprint to the row it locks, so exactly one updates it; the other gets 401
// rather than replacing the winner's new key.
func TestTwoProofsThatAuthenticatedTheOldKeyCannotBothWin(t *testing.T) {
	ts := newTestServer(t)
	credentialID, oldKey := ts.mintServiceLauncherCredential(t)
	keys := [2]*ecdsa.PrivateKey{newSigningKey(t), newSigningKey(t)}
	type answer struct {
		status int
		body   []byte
	}
	answers := make(chan answer, len(keys))
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, key := range keys {
		group.Add(1)
		go func(next *ecdsa.PrivateKey) {
			defer group.Done()
			<-start
			status, body := ts.renewTo(t, oldKey, credentialID, next, "legion-daemon")
			answers <- answer{status: status, body: body}
		}(key)
	}
	close(start)
	group.Wait()
	close(answers)

	var winner *ecdsa.PrivateKey
	for answer := range answers {
		switch answer.status {
		case http.StatusOK:
			if winner != nil {
				t.Fatalf("two renewals succeeded, second body %s", answer.body)
			}
			if got := ts.credentialKey(t, credentialID); got == thumbprintOf(t, keys[0]) {
				winner = keys[0]
			} else {
				winner = keys[1]
			}
		case http.StatusUnauthorized:
			if decode[wireError](t, answer.body).Code != "LAUNCHER_INVALID" {
				t.Fatalf("losing renewal = %d %s, want 401 LAUNCHER_INVALID", answer.status, answer.body)
			}
		default:
			t.Fatalf("renewal = %d %s, want one 200 and one 401", answer.status, answer.body)
		}
	}
	if winner == nil {
		t.Fatal("no renewal succeeded")
	}
	if got, want := ts.credentialKey(t, credentialID), thumbprintOf(t, winner); got != want {
		t.Fatalf("broker key = %s, want the winning renewal's key %s", got, want)
	}
}
