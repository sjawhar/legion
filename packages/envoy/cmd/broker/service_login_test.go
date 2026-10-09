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

// TestTheBinaryLetsAnyoneDecideAServicesLoginAndOnlyItsPersonDecideAPersonsLogin drives the REAL
// compiled broker on a real Postgres, with a local OIDC issuer standing in for the cluster's
// service-account token issuer (BROKER_K8S_OIDC_ISSUER), over HTTP alone. The Legion daemon's
// login, still naming ada in its login_hint as a daemon from before this rule does, opens a record
// anyone decides; bob approves it; its credential enrolls a pod; carol, who approved nothing, lists
// it with bob as its approver and revokes it, after which the pod renews no more. A person's own
// machine login, naming ada, is refused 403 NOT_APPROVER to carol and approved by ada. Every
// request and answer is logged, with no proof, bearer or pod token, as the live run's transcript.
func TestTheBinaryLetsAnyoneDecideAServicesLoginAndOnlyItsPersonDecideAPersonsLogin(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const podAudience = "legion-broker-pod"
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
			if token, ok := fields["pod_token"].(string); ok {
				shown = strings.ReplaceAll(shown, token, "<pod token>")
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
	startLogin := func(key *ecdsa.PrivateKey, host, service, loginHint string) (code, recordID string) {
		t.Helper()
		compact, err := record.Sign(key, base, []record.AuthorizationDetail{
			{Type: record.KindLauncherCredential, Identifier: host, Service: service},
		}, "", loginHint, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := call(http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
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

	t.Log("--- the Legion daemon's login, naming ada in its login_hint, approved by bob")
	daemonKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	code, recordID := startLogin(daemonKey, "example-host-cluster", "legion-daemon", "ada@example.com")
	status, answer := ui(http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": "bob@example.com", "code": code})
	credentialID := field(answer, "credential_id")
	if status != http.StatusOK || credentialID == "" {
		t.Fatalf("bob approves the daemon's login = %d %s, want 200 with a credential_id", status, answer)
	}

	t.Log("--- the credential enrolls a pod")
	sessionKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	claims := issuer.Claims("system:serviceaccount:legion:legion-worker", podAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "pod-1"}}
	status, answer = launcher(daemonKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": "pod-1", "slot": "implementer-g1", "thumbprint": thumbprint,
		"pod_token": issuer.Mint(t, podKey, claims),
	})
	enrollmentID := field(answer, "enrollment_id")
	if status != http.StatusCreated || enrollmentID == "" {
		t.Fatalf("enroll a pod = %d %s, want 201", status, answer)
	}
	renew := func() int {
		t.Helper()
		path := "/v1/enrollments/" + enrollmentID + "/renew"
		signed, err := proof.Sign(sessionKey, enrollmentID, http.MethodPost, base+path, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, _ := call(http.MethodPost, path, map[string]string{"Proof": signed}, nil)
		return status
	}
	if status := renew(); status != http.StatusOK {
		t.Fatalf("the pod renews = %d, want 200", status)
	}

	t.Log("--- carol, who approved nothing, lists the daemon's login and revokes it")
	status, answer = ui(http.MethodGet, "/v1/launcher-credentials?approver=carol@example.com", nil)
	if status != http.StatusOK || !bytes.Contains(answer, []byte(`"credential_id":"`+credentialID+`"`)) || !bytes.Contains(answer, []byte(`"approved_by":"bob@example.com"`)) {
		t.Fatalf("carol's machine logins = %d %s, want the daemon's, approved by bob", status, answer)
	}
	status, answer = ui(http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver",
		map[string]any{"approver": "carol@example.com"})
	if status != http.StatusOK || field(answer, "state") != "revoked" {
		t.Fatalf("carol revokes it = %d %s, want 200 revoked", status, answer)
	}
	if status := renew(); status != http.StatusUnauthorized {
		t.Fatalf("the pod renews after the revoke = %d, want 401", status)
	}

	t.Log("--- ada's own machine login: refused to carol, approved by ada")
	adaKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	code, recordID = startLogin(adaKey, "example-host-devbox", "", "ada@example.com")
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
