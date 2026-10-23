// oauth_login_test.go drives a person's machine login as an OAuth device grant against the real
// broker (brokertest.Rig): a golang.org/x/oauth2 client with a DPoP transport signed by the
// machine's key asks for a device code, the person decides it through the routes Dispatch relays
// to, and the machine's token poll and refreshes carry its launcher credential.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// deviceMachine is one machine logging in: its key, the OAuth client it logs in with, and the
// context that client's requests carry the key's proofs in.
type deviceMachine struct {
	rig    *brokertest.Rig
	key    *ecdsa.PrivateKey
	config *oauth2.Config
	ctx    context.Context
}

func newDeviceMachine(t *testing.T, rig *brokertest.Rig) *deviceMachine {
	t.Helper()
	key := newSigningKey(t)
	return &deviceMachine{
		rig: rig, key: key,
		config: &oauth2.Config{
			ClientID: oauthstore.ClientID, Scopes: []string{"machine"},
			Endpoint: oauth2.Endpoint{
				DeviceAuthURL: rig.URL + "/v1/oauth/device_authorization",
				TokenURL:      rig.URL + "/v1/oauth/token",
				AuthStyle:     oauth2.AuthStyleInParams,
			},
		},
		ctx: context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: helper.NewDPoPTransport(key, http.DefaultTransport)}),
	}
}

// thumbprint is the machine key's RFC 7638 thumbprint: its request object's iss.
func (m *deviceMachine) thumbprint(t *testing.T) string {
	t.Helper()
	jkt, err := proof.Thumbprint(&m.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return jkt
}

// request signs the machine's login request object naming loginHint and service.
func (m *deviceMachine) request(t *testing.T, loginHint, service string) string {
	t.Helper()
	return signMachineLoginRequest(t, m.key, m.rig.URL, loginHint, "example-host-devbox", service)
}

// authorize asks for a device code with request and dpopJKT.
func (m *deviceMachine) authorize(request, dpopJKT string) (*oauth2.DeviceAuthResponse, error) {
	return m.config.DeviceAuth(m.ctx, oauth2.SetAuthURLParam("request", request), oauth2.SetAuthURLParam("dpop_jkt", dpopJKT))
}

// login asks for a device code for a person's machine as the rig's operator.
func (m *deviceMachine) login(t *testing.T) *oauth2.DeviceAuthResponse {
	t.Helper()
	da, err := m.authorize(m.request(t, m.rig.Operator, ""), m.thumbprint(t))
	if err != nil {
		t.Fatalf("device authorization: %v", err)
	}
	return da
}

// poll exchanges an approved device code for its tokens, polling at once.
func (m *deviceMachine) poll(t *testing.T, da *oauth2.DeviceAuthResponse) *oauth2.Token {
	t.Helper()
	da.Interval = 1
	tok, err := m.config.DeviceAccessToken(m.ctx, da)
	if err != nil {
		t.Fatalf("device token: %v", err)
	}
	return tok
}

// refresh refreshes tok, as the helper's token source does once its access token expires.
func (m *deviceMachine) refresh(tok *oauth2.Token) (*oauth2.Token, error) {
	stale := *tok
	stale.Expiry = time.Now().Add(-time.Minute)
	return m.config.TokenSource(m.ctx, &stale).Token()
}

// approvedLogin logs a machine in, has the rig's operator approve it, and polls its tokens.
func approvedLogin(t *testing.T, rig *brokertest.Rig) (*deviceMachine, *oauth2.Token, string) {
	t.Helper()
	m := newDeviceMachine(t, rig)
	da := m.login(t)
	credentialID := rig.DecideMachineLogin(t, da.UserCode, true)
	tok := m.poll(t, da)
	if got, _ := tok.Extra("credential_id").(string); got != credentialID {
		t.Fatalf("token credential_id = %v, want %s", tok.Extra("credential_id"), credentialID)
	}
	return m, tok, credentialID
}

// enrollSession enrolls a session with a launcher proof of the machine's key naming credentialID.
func enrollSession(t *testing.T, rig *brokertest.Rig, key *ecdsa.PrivateKey, credentialID string) (int, []byte) {
	t.Helper()
	sessionKey := newSigningKey(t)
	sessionThumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	p, err := proof.SignLauncher(key, credentialID, http.MethodPost, rig.URL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rig.Req(t, http.MethodPost, "/v1/enrollments", map[string]string{"Proof": p}, map[string]any{
		"kind": "session", "runtime_id": "session-" + uuid.NewString(), "operator": rig.Operator, "thumbprint": sessionThumbprint,
	})
}

// oauthErrorCode is the OAuth error code a refused x/oauth2 call carries.
func oauthErrorCode(t *testing.T, err error) (string, string) {
	t.Helper()
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want an OAuth error", err)
	}
	return re.ErrorCode, re.ErrorDescription
}

func TestADeviceLoginIsApprovedThroughDispatchsRoutesAndIssuesACredential(t *testing.T) {
	rig := brokertest.NewRig(t)
	m := newDeviceMachine(t, rig)
	da := m.login(t)
	if da.VerificationURIComplete != "" {
		t.Fatalf("verification_uri_complete = %q, want none", da.VerificationURIComplete)
	}

	status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": da.UserCode})
	if status != http.StatusOK {
		t.Fatalf("lookup = %d: %s", status, body)
	}
	looked := decode[wireRecord](t, body)
	if looked.State != "pending" || looked.Approver != rig.Operator || len(looked.Identifiers) != 1 || looked.Identifiers[0] != "example-host-devbox" {
		t.Fatalf("lookup = %+v, want a pending login for example-host-devbox approved by %s", looked, rig.Operator)
	}

	status, body = rig.UI(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": rig.Operator, "code": da.UserCode})
	if status != http.StatusOK {
		t.Fatalf("approve = %d: %s", status, body)
	}
	approved := decode[struct {
		CredentialID *string `json:"credential_id"`
	}](t, body)
	if approved.CredentialID == nil {
		t.Fatalf("approve = %s, want a credential id", body)
	}

	tok := m.poll(t, da)
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("token = %+v, want an access and a refresh token", tok)
	}
	if got, _ := tok.Extra("credential_id").(string); got != *approved.CredentialID {
		t.Fatalf("token credential_id = %v, want %s", tok.Extra("credential_id"), *approved.CredentialID)
	}
	var minted time.Time
	if err := rig.Store.Pool.QueryRow(context.Background(), `select expires_at from launcher_credentials where id=$1`, *approved.CredentialID).Scan(&minted); err != nil {
		t.Fatal(err)
	}
	raw, _ := tok.Extra("credential_expires_at").(string)
	if at, err := time.Parse(time.RFC3339, raw); err != nil || !at.Equal(minted.Truncate(time.Second)) && !at.Equal(minted) {
		t.Fatalf("token credential_expires_at = %q, want the minted credential's %s", raw, minted)
	}

	if status, body := enrollSession(t, rig, m.key, *approved.CredentialID); status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments with the login's credential = %d: %s", status, body)
	}
}

func TestOnlyTheNamedPersonApprovesADeviceLogin(t *testing.T) {
	rig := brokertest.NewRig(t)
	m := newDeviceMachine(t, rig)
	da := m.login(t)
	_, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": da.UserCode})
	looked := decode[wireRecord](t, body)
	status, body := rig.UI(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": "mallory@example.com", "code": da.UserCode})
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
		t.Fatalf("approve as mallory = %d %s, want 403 NOT_APPROVER", status, body)
	}
}

func TestADeviceLoginNamingAServiceIsRefused(t *testing.T) {
	rig := brokertest.NewRig(t)
	m := newDeviceMachine(t, rig)
	_, err := m.authorize(m.request(t, "", "legion-daemon"), m.thumbprint(t))
	if code, desc := oauthErrorCode(t, err); code != "invalid_request" || !strings.HasPrefix(desc, "SERVICE_LOGS_IN_WITH_TOKEN: ") {
		t.Fatalf("device authorization naming a service = %s %q, want invalid_request SERVICE_LOGS_IN_WITH_TOKEN", code, desc)
	}
}

func TestADeviceRequestObjectIsSingleUse(t *testing.T) {
	rig := brokertest.NewRig(t)
	m := newDeviceMachine(t, rig)
	request := m.request(t, rig.Operator, "")
	if _, err := m.authorize(request, m.thumbprint(t)); err != nil {
		t.Fatalf("first device authorization: %v", err)
	}
	_, err := m.authorize(request, m.thumbprint(t))
	if code, desc := oauthErrorCode(t, err); code != "invalid_request" || !strings.HasPrefix(desc, "REQUEST_INVALID: ") {
		t.Fatalf("replayed request object = %s %q, want invalid_request REQUEST_INVALID", code, desc)
	}
}

func TestADeviceLoginsKeyMustBeItsRequestObjects(t *testing.T) {
	rig := brokertest.NewRig(t)
	m := newDeviceMachine(t, rig)
	other := newDeviceMachine(t, rig)
	_, err := m.authorize(m.request(t, rig.Operator, ""), other.thumbprint(t))
	if code, desc := oauthErrorCode(t, err); code != "invalid_request" || !strings.HasPrefix(desc, "REQUEST_INVALID: ") {
		t.Fatalf("dpop_jkt of another key = %s %q, want invalid_request REQUEST_INVALID", code, desc)
	}
	var n int
	if err := rig.Store.Pool.QueryRow(context.Background(), `select count(*) from credential_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("credential_requests after the refusal = %d (%v), want 0", n, err)
	}
}

func TestARefreshMovesTheCredentialsExpiry(t *testing.T) {
	rig := brokertest.NewRig(t)
	m, tok, credentialID := approvedLogin(t, rig)
	ctx := context.Background()
	// An earlier expiry than a fresh refresh would give, so the move is visible.
	if _, err := rig.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() + interval '1 day' where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	refreshed, err := m.refresh(tok)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var expires time.Time
	if err := rig.Store.Pool.QueryRow(ctx, `select expires_at from launcher_credentials where id=$1`, credentialID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	lifetime := 7 * 24 * time.Hour
	if expires.Before(before.Add(lifetime-time.Minute)) || expires.After(time.Now().Add(lifetime+time.Minute)) {
		t.Fatalf("expires_at after the refresh = %s, want about now + %s", expires, lifetime)
	}
	if got, _ := refreshed.Extra("credential_id").(string); got != credentialID {
		t.Fatalf("refreshed credential_id = %v, want %s", refreshed.Extra("credential_id"), credentialID)
	}
	raw, _ := refreshed.Extra("credential_expires_at").(string)
	if at, err := time.Parse(time.RFC3339, raw); err != nil || at.Sub(expires).Abs() > time.Second {
		t.Fatalf("refreshed credential_expires_at = %q, want %s", raw, expires)
	}
	status, body := rig.UI(t, http.MethodGet, "/v1/launcher-credentials?approver="+rig.Operator, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/launcher-credentials = %d: %s", status, body)
	}
	listed := decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, body)
	if len(listed.Credentials) != 1 || !listed.Credentials[0].ExpiresAt.Equal(expires) {
		t.Fatalf("listed = %+v, want the one login expiring %s", listed.Credentials, expires)
	}
}

func TestARevokedMachineRefusesItsNextCallAndItsRefresh(t *testing.T) {
	rig := brokertest.NewRig(t)
	m, tok, credentialID := approvedLogin(t, rig)
	status, body := enrollSession(t, rig, m.key, credentialID)
	if status != http.StatusCreated {
		t.Fatalf("enroll = %d: %s", status, body)
	}
	enrollmentID := decode[struct {
		ID string `json:"enrollment_id"`
	}](t, body).ID

	status, body = rig.UI(t, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver", map[string]any{"approver": rig.Operator})
	if status != http.StatusOK {
		t.Fatalf("revoke = %d: %s", status, body)
	}
	if status, body := enrollSession(t, rig, m.key, credentialID); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("enroll after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	var live int
	if err := rig.Store.Pool.QueryRow(context.Background(), `select count(*) from enrollments where launcher_credential_id=$1 and revoked_at is null`, credentialID).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live sessions of the revoked login = %d (%v), want 0 (enrollment %s)", live, err, enrollmentID)
	}
	_, err := m.refresh(tok)
	if code, _ := oauthErrorCode(t, err); code != "invalid_grant" {
		t.Fatalf("refresh after the revoke = %s, want invalid_grant", code)
	}
}

func TestAnUnrefreshedLoginLapses(t *testing.T) {
	rig := brokertest.NewRig(t)
	m, tok, credentialID := approvedLogin(t, rig)
	ctx := context.Background()
	if _, err := rig.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 minute' where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Store.Pool.Exec(ctx, `update oauth_refresh_tokens set expires_at = now() - interval '1 minute' where credential_id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	if status, body := enrollSession(t, rig, m.key, credentialID); status != http.StatusUnauthorized {
		t.Fatalf("enroll after the lapse = %d %s, want 401", status, body)
	}
	_, err := m.refresh(tok)
	if code, _ := oauthErrorCode(t, err); code != "invalid_grant" {
		t.Fatalf("refresh after the lapse = %s, want invalid_grant", code)
	}
}

// TestARefreshOfALapsedCredentialIsRefused pins the refresh's liveness check on its own: a
// credential past its expiry is not revived by a refresh token that has not itself expired.
func TestARefreshOfALapsedCredentialIsRefused(t *testing.T) {
	rig := brokertest.NewRig(t)
	m, tok, credentialID := approvedLogin(t, rig)
	if _, err := rig.Store.Pool.Exec(context.Background(), `update launcher_credentials set expires_at = now() - interval '1 minute' where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	_, err := m.refresh(tok)
	if code, _ := oauthErrorCode(t, err); code != "invalid_grant" {
		t.Fatalf("refresh of a lapsed credential = %s, want invalid_grant", code)
	}
}

// TestADeviceCodeOfARevokedLoginIsRefused pins the device code grant's liveness check: a login
// revoked between its approval and its poll issues no tokens.
func TestADeviceCodeOfARevokedLoginIsRefused(t *testing.T) {
	rig := brokertest.NewRig(t)
	m := newDeviceMachine(t, rig)
	da := m.login(t)
	credentialID := rig.DecideMachineLogin(t, da.UserCode, true)
	status, body := rig.UI(t, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver", map[string]any{"approver": rig.Operator})
	if status != http.StatusOK {
		t.Fatalf("revoke = %d: %s", status, body)
	}
	da.Interval = 1
	_, err := m.config.DeviceAccessToken(m.ctx, da)
	if code, _ := oauthErrorCode(t, err); code != "invalid_grant" {
		t.Fatalf("poll of a revoked login = %s, want invalid_grant", code)
	}
	var active int
	if err := rig.Store.Pool.QueryRow(context.Background(), `select (select count(*) from oauth_refresh_tokens where credential_id=$1 and active)
		+ (select count(*) from oauth_access_tokens where credential_id=$1 and active)`, credentialID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active token rows of the revoked login = %d (%v), want 0", active, err)
	}
}

func TestADeniedDeviceLoginIsAccessDenied(t *testing.T) {
	rig := brokertest.NewRig(t)
	m := newDeviceMachine(t, rig)
	da := m.login(t)
	rig.DecideMachineLogin(t, da.UserCode, false)
	da.Interval = 1
	_, err := m.config.DeviceAccessToken(m.ctx, da)
	if code, _ := oauthErrorCode(t, err); code != "access_denied" {
		t.Fatalf("poll of a denied login = %s, want access_denied", code)
	}
}

// TestADeviceLoginSpendsItsPersonsBucket pins the per-login limit the old route spends: the rig's
// limits allow five logins per person in a burst, so the sixth naming the same person is 429.
func TestADeviceLoginSpendsItsPersonsBucket(t *testing.T) {
	rig := brokertest.NewRig(t)
	for i := range 6 {
		m := newDeviceMachine(t, rig)
		_, err := m.authorize(m.request(t, rig.Operator, ""), m.thumbprint(t))
		if i < 5 {
			if err != nil {
				t.Fatalf("device authorization %d: %v", i+1, err)
			}
			continue
		}
		var re *oauth2.RetrieveError
		if !errors.As(err, &re) || re.Response.StatusCode != http.StatusTooManyRequests || !strings.HasPrefix(re.ErrorDescription, "RATE_LIMITED: ") || re.Response.Header.Get("Retry-After") == "" {
			t.Fatalf("sixth device authorization = %v, want 429 RATE_LIMITED with Retry-After", err)
		}
	}
}

// lockCredential opens a transaction on its own connection holding credentialID's row lock, as a
// revoke or a refresh holds it, and answers that transaction's backend pid.
func lockCredential(t *testing.T, rig *brokertest.Rig, credentialID string) (pgx.Tx, int) {
	t.Helper()
	ctx := context.Background()
	tx, err := rig.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var pid int
	if err := tx.QueryRow(ctx, `select pg_backend_pid() from launcher_credentials where id=$1 for no key update`, credentialID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return tx, pid
}

// awaitWaiters waits until n backends wait, directly or behind another waiter, on holder's lock.
func awaitWaiters(t *testing.T, rig *brokertest.Rig, holder, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := rig.Store.Pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity a
			where $1 = any(pg_blocking_pids(a.pid)) or exists (select 1 from pg_stat_activity b
				where $1 = any(pg_blocking_pids(b.pid)) and b.pid = any(pg_blocking_pids(a.pid)))`, holder).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends wait on the credential's lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// revokeAsync revokes credentialID as the rig's operator through the route Dispatch relays to,
// answering its status on the channel.
func revokeAsync(rig *brokertest.Rig, credentialID string) <-chan int {
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, rig.URL+"/v1/launcher-credentials/"+credentialID+"/revoke-by-approver",
			strings.NewReader(`{"approver":"`+rig.Operator+`"}`))
		req.Header.Set("Authorization", "Bearer "+brokertest.UIToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	return done
}

type refreshResult struct {
	tok *oauth2.Token
	err error
}

func refreshAsync(m *deviceMachine, tok *oauth2.Token) <-chan refreshResult {
	done := make(chan refreshResult, 1)
	go func() {
		refreshed, err := m.refresh(tok)
		done <- refreshResult{refreshed, err}
	}()
	return done
}

// activeTokens counts the access and refresh tokens still active for credentialID.
func activeTokens(t *testing.T, rig *brokertest.Rig, credentialID string) int {
	t.Helper()
	var n int
	if err := rig.Store.Pool.QueryRow(context.Background(), `select (select count(*) from oauth_access_tokens where credential_id=$1 and active)
		+ (select count(*) from oauth_refresh_tokens where credential_id=$1 and active)`, credentialID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestARevokeQueuedBehindARefreshEndsItsNewTokens pins that a refresh holds its credential's row
// from its liveness check until its new tokens commit: a revoke that arrives in that window waits,
// then ends the tokens the refresh just issued, so none stays active for the revoked login.
func TestARevokeQueuedBehindARefreshEndsItsNewTokens(t *testing.T) {
	rig := brokertest.NewRig(t)
	m, tok, credentialID := approvedLogin(t, rig)
	holder, pid := lockCredential(t, rig, credentialID)
	refreshed := refreshAsync(m, tok)
	awaitWaiters(t, rig, pid, 1)
	revoked := revokeAsync(rig, credentialID)
	awaitWaiters(t, rig, pid, 2)
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := <-refreshed
	if r.err != nil {
		t.Fatalf("refresh ahead of the revoke: %v", r.err)
	}
	if status := <-revoked; status != http.StatusOK {
		t.Fatalf("revoke behind the refresh = %d, want 200", status)
	}
	if n := activeTokens(t, rig, credentialID); n != 0 {
		t.Fatalf("active tokens of the revoked login = %d, want 0", n)
	}
	_, err := m.refresh(r.tok)
	if code, _ := oauthErrorCode(t, err); code != "invalid_grant" {
		t.Fatalf("refresh with the token issued before the revoke = %s, want invalid_grant", code)
	}
}

// TestARefreshQueuedBehindARevokeIsRefused pins the other order: a refresh that reaches the
// credential's row after a revoke took it finds the login ended and issues nothing.
func TestARefreshQueuedBehindARevokeIsRefused(t *testing.T) {
	rig := brokertest.NewRig(t)
	m, tok, credentialID := approvedLogin(t, rig)
	holder, pid := lockCredential(t, rig, credentialID)
	revoked := revokeAsync(rig, credentialID)
	awaitWaiters(t, rig, pid, 1)
	refreshed := refreshAsync(m, tok)
	awaitWaiters(t, rig, pid, 2)
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := <-revoked; status != http.StatusOK {
		t.Fatalf("revoke ahead of the refresh = %d, want 200", status)
	}
	r := <-refreshed
	if code, _ := oauthErrorCode(t, r.err); code != "invalid_grant" {
		t.Fatalf("refresh behind the revoke = %s, want invalid_grant", code)
	}
	if n := activeTokens(t, rig, credentialID); n != 0 {
		t.Fatalf("active tokens of the revoked login = %d, want 0", n)
	}
}
