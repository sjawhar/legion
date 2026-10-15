// token_login_test.go drives POST /v1/launcher-credentials/token, a service logging in with its own
// projected Kubernetes ServiceAccount token, which no person approves, on the same real broker
// api_test.go mounts.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// wireTokenLogin is POST /v1/launcher-credentials/token's answer.
type wireTokenLogin struct {
	CredentialID string    `json:"credential_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// accountToken mints a projected ServiceAccount token for account, bound to podUID ("" for none,
// as `kubectl create token` mints), issued a few seconds ago and expiring lifetime from now
// (negative for one already expired), signed by the local OIDC issuer the test server trusts.
func (ts *testServer) accountToken(t *testing.T, account, podUID string, lifetime time.Duration) string {
	t.Helper()
	claims := ts.podIssuer.Claims(account, podAudience)
	now := time.Now()
	claims["iat"] = now.Add(min(lifetime, 0) - 5*time.Second).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = now.Add(lifetime).Unix()
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	return ts.podIssuer.Mint(t, ts.podKey, claims)
}

// tokenLogin posts token and request to POST /v1/launcher-credentials/token.
func (ts *testServer) tokenLogin(t *testing.T, token, request string) (int, []byte) {
	t.Helper()
	return ts.req(t, http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{"token": token, "request": request})
}

// daemonRequest is the request object the Legion daemon signs for its token login: one
// launcher_credential detail naming legion-daemon, signed by key.
func (ts *testServer) daemonRequest(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	return signMachineLoginRequest(t, key, ts.URL, "", "legion-daemon-test-7d9f", "legion-daemon")
}

// enrollPodWith enrolls a fresh pod under credentialID with key, answering the broker's status.
func (ts *testServer) enrollPodWith(t *testing.T, key *ecdsa.PrivateKey, credentialID string) (int, []byte) {
	t.Helper()
	podUID := uuid.NewString()
	return ts.launcher(t, key, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprintOf(t, newSigningKey(t)),
		"pod_token": ts.podToken(t, workerAccount, podUID),
	})
}

// TestAServiceLogsInWithItsOwnTokenAndNoOneApproves: the daemon's ServiceAccount token and a
// request object its new key signed issue a launcher credential at once, with no machine-login
// record and nothing pending for anyone; it expires with the token, and its key enrolls a pod. The
// broker's audit records the login with the account, the pod the token is bound to and the key.
func TestAServiceLogsInWithItsOwnTokenAndNoOneApproves(t *testing.T) {
	ts := newTestServer(t)
	key := newSigningKey(t)
	daemonPod := uuid.NewString()
	before := time.Now()
	status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, daemonPod, 20*time.Minute), ts.daemonRequest(t, key))
	if status != http.StatusOK {
		t.Fatalf("token login = %d %s, want 200", status, body)
	}
	login := decode[wireTokenLogin](t, body)
	if want := before.Add(20 * time.Minute).Truncate(time.Second); login.ExpiresAt.Before(want.Add(-time.Second)) || login.ExpiresAt.After(want.Add(2*time.Second)) {
		t.Fatalf("expires_at = %s, want the token's own expiry, about %s", login.ExpiresAt, want)
	}
	if status, body := ts.enrollPodWith(t, key, login.CredentialID); status != http.StatusCreated {
		t.Fatalf("enroll a pod with the token-issued credential = %d %s, want 201", status, body)
	}
	if got := ts.column(t, `select count(*)::text from credential_requests`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("credential-request records = %v, want none: no person approves a token login", got)
	}
	if got := ts.column(t, `select coalesce(service,'') || ' ' || host || ' ' || coalesce(subject,'') from launcher_credentials where id=$1`, login.CredentialID); !slices.Equal(got, []string{"legion-daemon legion-system/legion-daemon-test " + daemonAccount}) {
		t.Fatalf("launcher credential = %v, want legion-daemon on host legion-system/legion-daemon-test for %s", got, daemonAccount)
	}
	if got := ts.column(t, `select actor || ' ' || (detail->>'subject') || ' ' || (detail->>'pod_uid') || ' ' || (detail->>'key_thumbprint')
		from audit where kind='launcher_credential.token_login'`); !slices.Equal(got, []string{record.LauncherActor(login.CredentialID) + " " + daemonAccount + " " + daemonPod + " " + thumbprintOf(t, key)}) {
		t.Fatalf("token_login audit rows = %v, want one naming the account, its pod and the key", got)
	}
	if got := ts.column(t, `select ((detail->>'token_expires_at')::timestamptz = (detail->>'expires_at')::timestamptz)::text from audit where kind='launcher_credential.token_login'`); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("token_login audit row's token_expires_at matches its expires_at = %v, want true for a 20-minute token", got)
	}
}

// TestATokenLoginIsCappedAtAnHour: a token whose expiry lies past an hour from the login, within
// the skew MaxTokenLoginLifetime allows, issues a credential that still expires an hour after the
// login, and the audit row records the token's own later expiry.
func TestATokenLoginIsCappedAtAnHour(t *testing.T) {
	ts := newTestServer(t)
	before := time.Now()
	status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, "", time.Hour+40*time.Second), ts.daemonRequest(t, newSigningKey(t)))
	if status != http.StatusOK {
		t.Fatalf("token login = %d %s, want 200", status, body)
	}
	if got := decode[wireTokenLogin](t, body).ExpiresAt; got.Before(before.Add(time.Hour-time.Second)) || got.After(time.Now().Add(time.Hour)) {
		t.Fatalf("expires_at = %s, want an hour after the login", got)
	}
	if got := ts.column(t, `select ((detail->>'token_expires_at')::timestamptz > (detail->>'expires_at')::timestamptz)::text from audit where kind='launcher_credential.token_login'`); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("token_login audit row's token_expires_at after its expires_at = %v, want true", got)
	}
}

// TestATokenLoginRefusesAKeyAnotherLoginHolds: a token login whose request a key signed that a
// person's live machine login already holds is 409 KEY_HOLDS_LIVE_CREDENTIAL, issues nothing, and
// leaves the person's credential on its key.
func TestATokenLoginRefusesAKeyAnotherLoginHolds(t *testing.T) {
	ts := newTestServer(t)
	personsID, key := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, "", time.Hour), ts.daemonRequest(t, key))
	if status != http.StatusConflict || decode[wireError](t, body).Code != "KEY_HOLDS_LIVE_CREDENTIAL" {
		t.Fatalf("token login with a person's key = %d %s, want 409 KEY_HOLDS_LIVE_CREDENTIAL", status, body)
	}
	if got := ts.column(t, `select count(*)::text from launcher_credentials where subject is not null`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("token-issued credentials = %v, want none", got)
	}
	if got := ts.column(t, `select key_thumbprint from launcher_credentials where id=$1`, personsID); !slices.Equal(got, []string{thumbprintOf(t, key)}) {
		t.Fatalf("the person's credential's key = %v, want it unchanged", got)
	}
}

// TestAForgedTokenFloodIsLimitedByAddress: every token login spends its source address's bucket
// before the token is verified, whatever account it claims, so tokens that name the daemon's
// listed account but that the issuer never signed are refused 429 once the address's bucket is
// empty, rather than each costing a verification (and possibly a fetch of the issuer's keys).
func TestAForgedTokenFloodIsLimitedByAddress(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.LauncherLimits = &api.LauncherLimits{
			PerAddress: ratelimit.Limit{Every: time.Hour, Burst: 2},
			PerLogin:   ratelimit.Limit{Every: time.Hour, Burst: 100},
		}
	})
	forger := oidctest.NewKey(t, "forger")
	for i, want := range []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusTooManyRequests} {
		claims := ts.podIssuer.Claims(daemonAccount, podAudience)
		status, body := ts.tokenLogin(t, ts.podIssuer.Mint(t, forger, claims), ts.daemonRequest(t, newSigningKey(t)))
		if status != want {
			t.Fatalf("forged token claiming %s, attempt %d = %d %s, want %d", daemonAccount, i, status, body, want)
		}
	}
}

// TestAVerifiedAccountSpendsItsOwnBucketToo: a verified token login spends its account's own
// bucket after its address's, so the daemon's account logs in only as often as that bucket allows
// even from an address with requests to spare.
func TestAVerifiedAccountSpendsItsOwnBucketToo(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.LauncherLimits = &api.LauncherLimits{
			PerAddress: ratelimit.Limit{Every: time.Hour, Burst: 100},
			PerLogin:   ratelimit.Limit{Every: time.Hour, Burst: 2},
		}
	})
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, "", time.Hour), ts.daemonRequest(t, newSigningKey(t))); status != want {
			t.Fatalf("the daemon's login %d = %d %s, want %d", i, status, body, want)
		}
	}
}

// TestARestartLogsInAgainAndKeepsItsOneCredential is a daemon's restart: a second token login for
// the same account, with a new key and a fresh token, answers the same credential, moved to the new
// key, so no person is asked and the pods the first boot enrolled stay the daemon's own. The old
// key's proofs stop authenticating at once; the new key enrolls a pod and ends the pod the first
// boot enrolled.
func TestARestartLogsInAgainAndKeepsItsOneCredential(t *testing.T) {
	ts := newTestServer(t)
	firstKey := newSigningKey(t)
	status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, uuid.NewString(), time.Hour), ts.daemonRequest(t, firstKey))
	if status != http.StatusOK {
		t.Fatalf("first boot's token login = %d %s, want 200", status, body)
	}
	first := decode[wireTokenLogin](t, body)
	status, body = ts.enrollPodWith(t, firstKey, first.CredentialID)
	if status != http.StatusCreated {
		t.Fatalf("first boot's enrollment = %d %s, want 201", status, body)
	}
	earlier := decode[wireEnrolled](t, body).EnrollmentID

	restartKey := newSigningKey(t)
	status, body = ts.tokenLogin(t, ts.accountToken(t, daemonAccount, uuid.NewString(), time.Hour), ts.daemonRequest(t, restartKey))
	if status != http.StatusOK {
		t.Fatalf("restart's token login = %d %s, want 200", status, body)
	}
	if restart := decode[wireTokenLogin](t, body); restart.CredentialID != first.CredentialID {
		t.Fatalf("restart's credential = %s, want the account's one credential %s", restart.CredentialID, first.CredentialID)
	}
	if status, body := ts.enrollPodWith(t, firstKey, first.CredentialID); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("the first boot's key after the restart = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if status, body := ts.enrollPodWith(t, restartKey, first.CredentialID); status != http.StatusCreated {
		t.Fatalf("the restart's key enrolls a pod = %d %s, want 201", status, body)
	}
	if status, body := ts.launcher(t, restartKey, first.CredentialID, http.MethodDelete, "/v1/enrollments/"+earlier, nil); status != http.StatusNoContent {
		t.Fatalf("the restart ends the pod the first boot enrolled = %d %s, want 204", status, body)
	}
	if got := ts.column(t, `select count(*)::text from launcher_credentials`); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("launcher credentials = %v, want the account's one", got)
	}
}

// TestATokenLoginRefusesWhatDoesNotProveTheAccount: a token for a ServiceAccount
// BROKER_SERVICE_LOGINS does not name is 403 ACCOUNT_NOT_LISTED; an expired token is 401
// TOKEN_EXPIRED; a token for another audience, or signed by a key the issuer never published, is 401
// TOKEN_INVALID; a request naming another service is 403 ACCOUNT_NOT_LISTED; and a request object
// replayed is 400 REQUEST_INVALID. None issues a credential.
func TestATokenLoginRefusesWhatDoesNotProveTheAccount(t *testing.T) {
	ts := newTestServer(t)
	wrongAudience := ts.podIssuer.Claims(daemonAccount, "somebody-else")
	replayed := ts.daemonRequest(t, newSigningKey(t))
	if status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, "", time.Hour), replayed); status != http.StatusOK {
		t.Fatalf("the first use of the request = %d %s, want 200", status, body)
	}
	if _, err := ts.Store.Pool.Exec(context.Background(), `update launcher_credentials set revoked_at=now()`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, token, request string
		status               int
		code                 string
	}{
		{"another ServiceAccount's token", ts.accountToken(t, workerAccount, uuid.NewString(), time.Hour), ts.daemonRequest(t, newSigningKey(t)), http.StatusForbidden, "ACCOUNT_NOT_LISTED"},
		{"an expired token", ts.accountToken(t, daemonAccount, uuid.NewString(), -time.Minute), ts.daemonRequest(t, newSigningKey(t)), http.StatusUnauthorized, "TOKEN_EXPIRED"},
		{"a token that lives three hours", ts.accountToken(t, daemonAccount, "", 3*time.Hour), ts.daemonRequest(t, newSigningKey(t)), http.StatusUnauthorized, "TOKEN_TOO_LONG_LIVED"},
		{"a token for another audience", ts.podIssuer.Mint(t, ts.podKey, wrongAudience), ts.daemonRequest(t, newSigningKey(t)), http.StatusUnauthorized, "TOKEN_INVALID"},
		{"a token the issuer never signed", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln", ts.daemonRequest(t, newSigningKey(t)), http.StatusUnauthorized, "TOKEN_INVALID"},
		{"a request naming another service", ts.accountToken(t, daemonAccount, "", time.Hour), signMachineLoginRequest(t, newSigningKey(t), ts.URL, "", "legion-daemon-test-7d9f", "other-service"), http.StatusForbidden, "ACCOUNT_NOT_LISTED"},
		{"a replayed request object", ts.accountToken(t, daemonAccount, "", time.Hour), replayed, http.StatusBadRequest, "REQUEST_INVALID"},
	} {
		if status, body := ts.tokenLogin(t, c.token, c.request); status != c.status || decode[wireError](t, body).Code != c.code {
			t.Errorf("%s = %d %s, want %d %s", c.name, status, body, c.status, c.code)
		}
	}
	if got := ts.column(t, `select count(*)::text from launcher_credentials where revoked_at is null`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("live launcher credentials = %v, want none: every refused login issues nothing", got)
	}
}

// TestAnAccountTakenOffTheListLosesItsCredential: once BROKER_SERVICE_LOGINS no longer names the
// daemon's account, the credential its token login issued stops authenticating at its next call,
// and its next login is refused.
func TestAnAccountTakenOffTheListLosesItsCredential(t *testing.T) {
	ts := newTestServer(t)
	credentialID, key := ts.mintServiceLauncherCredential(t)
	if status, body := ts.enrollPodWith(t, key, credentialID); status != http.StatusCreated {
		t.Fatalf("enroll while listed = %d %s, want 201", status, body)
	}
	delete(ts.ServiceLogins, daemonAccount)
	if status, body := ts.enrollPodWith(t, key, credentialID); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("enroll once unlisted = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, "", time.Hour), ts.daemonRequest(t, newSigningKey(t))); status != http.StatusForbidden || decode[wireError](t, body).Code != "ACCOUNT_NOT_LISTED" {
		t.Fatalf("token login once unlisted = %d %s, want 403 ACCOUNT_NOT_LISTED", status, body)
	}
}

// TestAServiceDeviceCodeLoginIsRefused: a device-code login naming a service is refused 400
// SERVICE_LOGS_IN_WITH_TOKEN and leaves nothing pending in anyone's list.
func TestAServiceDeviceCodeLoginIsRefused(t *testing.T) {
	ts := newTestServer(t)
	for _, hint := range []string{"", testApprover} {
		status, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil,
			map[string]any{"request": signMachineLoginRequest(t, newSigningKey(t), ts.URL, hint, "cluster.example", "legion-daemon")})
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "SERVICE_LOGS_IN_WITH_TOKEN" {
			t.Fatalf("a service's device-code login (login_hint %q) = %d %s, want 400 SERVICE_LOGS_IN_WITH_TOKEN", hint, status, body)
		}
	}
	if got := ts.column(t, `select count(*)::text from credential_requests`); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("credential-request records = %v, want none", got)
	}
}

// TestAnyoneRevokesAServicesTokenLoginAndItLogsInAgain: the daemon's token login is listed for every
// signed-in person, naming no approver, and any of them may revoke it; the revoke ends every pod it
// enrolled at once, and the daemon's next token login, its account still listed, is a new
// credential, which enrolls again. A person's own machine list never shows it.
func TestAnyoneRevokesAServicesTokenLoginAndItLogsInAgain(t *testing.T) {
	ts := newTestServer(t)
	credentialID, key := ts.mintServiceLauncherCredential(t)
	status, body := ts.enrollPodWith(t, key, credentialID)
	if status != http.StatusCreated {
		t.Fatalf("enroll a pod = %d %s, want 201", status, body)
	}
	enrollment := decode[wireEnrolled](t, body).EnrollmentID
	for _, person := range []string{testApprover, "bob@example.com"} {
		logins := ts.machineLogins(t, person)
		if len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Service == nil || *logins[0].Service != "legion-daemon" ||
			logins[0].ApprovedBy != "" || logins[0].Subject == nil || *logins[0].Subject != daemonAccount || logins[0].Host != "legion-system/legion-daemon-test" {
			t.Fatalf("machine logins of %s = %+v, want the daemon's token login, approved by no one", person, logins)
		}
	}
	if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver", map[string]any{"approver": "bob@example.com"}); status != http.StatusOK {
		t.Fatalf("bob revokes the daemon's token login = %d %s, want 200", status, body)
	}
	if got := ts.column(t, `select (revoked_at is not null)::text from enrollments where id=$1`, enrollment); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("the pod's enrollment after the revoke: revoked = %v, want true", got)
	}
	if status, body := ts.enrollPodWith(t, key, credentialID); status != http.StatusUnauthorized {
		t.Fatalf("the revoked credential enrolls = %d %s, want 401", status, body)
	}
	again, againKey := ts.mintServiceLauncherCredential(t)
	if again == credentialID {
		t.Fatal("the daemon's next token login answered the revoked credential, want a new one")
	}
	if status, body := ts.enrollPodWith(t, againKey, again); status != http.StatusCreated {
		t.Fatalf("the new credential enrolls = %d %s, want 201", status, body)
	}
}
