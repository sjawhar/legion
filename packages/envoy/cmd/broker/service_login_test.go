package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestTheBinaryLogsTheDaemonInWithItsTokenAndOnlyAPersonDecidesTheirOwnLogin drives the REAL
// compiled broker on a real Postgres, with a local OIDC issuer standing in for the cluster's
// service-account token issuer (BROKER_K8S_OIDC_ISSUER), over HTTP alone. The Legion daemon's
// device-code login is refused; its token login, for the account BROKER_SERVICE_LOGINS names, issues
// a credential at once, which enrolls a pod whose renewals re-verify its own projected token; a
// restart's token login answers the same credential on a new key, after which the first key is
// refused and the new one enrolls; carol, whom no one asked, lists the daemon's login, naming no
// approver, and revokes it, after which the pod renews no more and the daemon's next login is a new
// credential. A person's own machine login, naming ada, is refused 403 NOT_APPROVER to carol and approved
// by ada. Every request and answer is logged, with no proof, bearer or token, as the live run's
// transcript.
func TestTheBinaryLogsTheDaemonInWithItsTokenAndOnlyAPersonDecidesTheirOwnLogin(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const podAudience = "legion-broker-pod"
	const daemonAccount = "system:serviceaccount:legion-system:legion-daemon-test"
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, _ := startBroker(t,
		"BROKER_DATABASE_URL="+databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER="+issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE="+podAudience,
		"BROKER_SERVICE_LOGINS=legion-daemon="+daemonAccount,
	)
	base := "http://" + addr

	call := func(method, path string, auth map[string]string, body any) (int, []byte) {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			if payload, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		for name, value := range auth {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		shown := string(payload)
		if fields, ok := body.(map[string]any); ok {
			for _, secret := range []string{"pod_token", "token"} {
				if token, ok := fields[secret].(string); ok {
					shown = strings.ReplaceAll(shown, token, "<"+secret+">")
				}
			}
		}
		t.Logf("> %s %s %s\n< %d %s", method, path, shown, response.StatusCode, bytes.TrimSpace(answer))
		return response.StatusCode, answer
	}
	ui := func(method, path string, body any) (int, []byte) {
		t.Helper()
		return call(method, path, map[string]string{"Authorization": "Bearer " + testUIToken}, body)
	}
	launcher := func(key *ecdsa.PrivateKey, credentialID, method, path string, body any) (int, []byte) {
		t.Helper()
		signed, err := proof.SignLauncher(key, credentialID, method, base+path, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return call(method, path, map[string]string{"Proof": signed}, body)
	}
	field := func(answer []byte, name string) string {
		t.Helper()
		var fields map[string]any
		if err := json.Unmarshal(answer, &fields); err != nil {
			t.Fatalf("decode %s: %v", answer, err)
		}
		value, _ := fields[name].(string)
		return value
	}
	signRequest := func(key *ecdsa.PrivateKey, host, service, loginHint string) string {
		t.Helper()
		compact, err := record.Sign(key, base, []record.AuthorizationDetail{
			{Type: record.KindLauncherCredential, Identifier: host, Service: service},
		}, "", loginHint, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return compact
	}
	startLogin := func(key *ecdsa.PrivateKey, host, loginHint string) (code, recordID string) {
		t.Helper()
		status, answer := call(http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": signRequest(key, host, "", loginHint)})
		if status != http.StatusAccepted {
			t.Fatalf("start the login = %d %s, want 202", status, answer)
		}
		code = field(answer, "code")
		status, answer = ui(http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
		if status != http.StatusOK {
			t.Fatalf("look the login up = %d %s", status, answer)
		}
		return code, field(answer, "record_id")
	}
	tokenLogin := func(key *ecdsa.PrivateKey) string {
		t.Helper()
		status, answer := call(http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{
			"token":   issuer.Mint(t, podKey, issuer.Claims(daemonAccount, podAudience)),
			"request": signRequest(key, "example-host-cluster", "legion-daemon", ""),
		})
		credentialID := field(answer, "credential_id")
		if status != http.StatusOK || credentialID == "" {
			t.Fatalf("the daemon's token login = %d %s, want 200 with a credential_id", status, answer)
		}
		return credentialID
	}
	podToken := func(uid string) string {
		claims := issuer.Claims("system:serviceaccount:legion:legion-worker", podAudience)
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
		return issuer.Mint(t, podKey, claims)
	}
	expiredToken := func(uid string) string {
		claims := issuer.Claims("system:serviceaccount:legion:legion-worker", podAudience)
		claims["iat"], claims["exp"] = time.Now().Add(-20*time.Minute).Unix(), time.Now().Add(-10*time.Minute).Unix()
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
		return issuer.Mint(t, podKey, claims)
	}
	enroll := func(key *ecdsa.PrivateKey, credentialID, podUID string, sessionKey *ecdsa.PrivateKey) (int, []byte) {
		t.Helper()
		thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		return launcher(key, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprint,
			"pod_token": podToken(podUID),
		})
	}

	t.Log("--- the Legion daemon's device-code login is refused")
	daemonKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	status, answer := call(http.MethodPost, "/v1/launcher-credentials", nil,
		map[string]any{"request": signRequest(daemonKey, "example-host-cluster", "legion-daemon", "ada@example.com")})
	if status != http.StatusBadRequest || field(answer, "code") != "SERVICE_LOGS_IN_WITH_TOKEN" {
		t.Fatalf("the daemon's device-code login = %d %s, want 400 SERVICE_LOGS_IN_WITH_TOKEN", status, answer)
	}

	t.Log("--- the daemon logs in with its token, and its credential enrolls a pod")
	credentialID := tokenLogin(daemonKey)
	sessionKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	status, answer = enroll(daemonKey, credentialID, "pod-1", sessionKey)
	enrollmentID := field(answer, "enrollment_id")
	if status != http.StatusCreated || enrollmentID == "" {
		t.Fatalf("enroll a pod = %d %s, want 201", status, answer)
	}
	renewAt := func(body any, at time.Time) (int, string) {
		t.Helper()
		path := "/v1/enrollments/" + enrollmentID + "/renew"
		signed, err := proof.Sign(sessionKey, enrollmentID, http.MethodPost, base+path, at)
		if err != nil {
			t.Fatal(err)
		}
		status, answer := call(http.MethodPost, path, map[string]string{"Proof": signed}, body)
		return status, field(answer, "code")
	}
	renew := func(body any) (int, string) { t.Helper(); return renewAt(body, time.Now()) }

	t.Log("--- the pod renews: no token accepted until its first token renewal, another pod's token and its own expired token refused, a skewed proof retryable, then no token refused")
	if status, code := renew(nil); status != http.StatusOK {
		t.Fatalf("the new pod renews with no token = %d %s, want 200 (the ratchet is not set yet)", status, code)
	}
	if status, code := renew(map[string]any{"pod_token": podToken("pod-2")}); status != http.StatusForbidden || code != "POD_IDENTITY_MISMATCH" {
		t.Fatalf("the pod renews with another pod's token = %d %s, want 403 POD_IDENTITY_MISMATCH", status, code)
	}
	if status, code := renew(map[string]any{"pod_token": expiredToken("pod-1")}); status != http.StatusForbidden || code != "POD_TOKEN_EXPIRED" {
		t.Fatalf("the pod renews with its own token past its expiry = %d %s, want 403 POD_TOKEN_EXPIRED", status, code)
	}
	if status, code := renewAt(map[string]any{"pod_token": podToken("pod-1")}, time.Now().Add(-10*time.Minute)); status != http.StatusUnauthorized || code != "PROOF_INVALID" {
		t.Fatalf("the pod renews with a proof signed 10 minutes ago = %d %s, want 401 PROOF_INVALID", status, code)
	}
	if status, code := renew(map[string]any{"pod_token": podToken("pod-1")}); status != http.StatusOK {
		t.Fatalf("the pod renews with its own token = %d %s, want 200", status, code)
	}
	if status, code := renew(nil); status != http.StatusForbidden || code != "POD_TOKEN_REQUIRED" {
		t.Fatalf("the pod renews with no token after renewing with it = %d %s, want 403 POD_TOKEN_REQUIRED", status, code)
	}

	t.Log("--- a restart logs in again: the same credential, on a new key")
	restartKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if again := tokenLogin(restartKey); again != credentialID {
		t.Fatalf("the restart's credential = %s, want the account's one, %s", again, credentialID)
	}
	otherSession, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if status, answer := enroll(daemonKey, credentialID, "pod-3", otherSession); status != http.StatusUnauthorized {
		t.Fatalf("the first key after the restart = %d %s, want 401", status, answer)
	}
	if status, answer := enroll(restartKey, credentialID, "pod-3", otherSession); status != http.StatusCreated {
		t.Fatalf("the restart's key enrolls a pod = %d %s, want 201", status, answer)
	}
	if status, code := renew(map[string]any{"pod_token": podToken("pod-1")}); status != http.StatusOK {
		t.Fatalf("the pod enrolled before the restart renews = %d %s, want 200", status, code)
	}

	t.Log("--- carol, whom no one asked, lists the daemon's token login and revokes it")
	status, answer = ui(http.MethodGet, "/v1/launcher-credentials?approver=carol@example.com", nil)
	if status != http.StatusOK || !bytes.Contains(answer, []byte(`"credential_id":"`+credentialID+`"`)) || !bytes.Contains(answer, []byte(`"approved_by":""`)) {
		t.Fatalf("carol's machine logins = %d %s, want the daemon's token login, approved by no one", status, answer)
	}
	status, answer = ui(http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver",
		map[string]any{"approver": "carol@example.com"})
	if status != http.StatusOK || field(answer, "state") != "revoked" {
		t.Fatalf("carol revokes it = %d %s, want 200 revoked", status, answer)
	}
	if status, code := renew(map[string]any{"pod_token": podToken("pod-1")}); status != http.StatusUnauthorized || code != "ENROLLMENT_NOT_LIVE" {
		t.Fatalf("the pod renews after the revoke = %d %s, want 401 ENROLLMENT_NOT_LIVE", status, code)
	}
	if again := tokenLogin(proofKey(t)); again == credentialID {
		t.Fatal("the daemon's token login after the revoke answered the revoked credential, want a new one")
	}

	t.Log("--- ada's own machine login: refused to carol, approved by ada")
	adaKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	code, recordID := startLogin(adaKey, "example-host-devbox", "ada@example.com")
	status, answer = ui(http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": "carol@example.com", "code": code})
	if status != http.StatusForbidden || field(answer, "code") != "NOT_APPROVER" {
		t.Fatalf("carol approves ada's machine login = %d %s, want 403 NOT_APPROVER", status, answer)
	}
	status, answer = ui(http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": "ada@example.com", "code": code})
	if status != http.StatusOK || field(answer, "credential_id") == "" {
		t.Fatalf("ada approves her machine login = %d %s, want 200 with a credential_id", status, answer)
	}
}

// proofKey is a fresh signing key, failing t if none can be made.
func proofKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}
