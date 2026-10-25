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

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestOneServicesTwoAccountsEachGetItsSecret drives the REAL compiled broker on a real Postgres,
// with a local OIDC issuer standing in for the cluster's service-account token issuer, over HTTP
// alone. BROKER_SERVICES names two accounts for one service, so the broker boots serving the
// secret that service owns instead of refusing its owner tag, which proves its policy loader was
// given the service and not the accounts. The service logs in once with its own token, and its one
// credential enrolls three pods, each proving with its projected token that it runs as its own
// account. The pods running as either of the service's accounts are each granted its secret at
// once, naming no approver, and read its value; the pod running as an account no entry names is
// denied it at once, with no grant, no record and no approver to ask.
func TestOneServicesTwoAccountsEachGetItsSecret(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		accountA     = "system:serviceaccount:example:example-worker-a"
		accountB     = "system:serviceaccount:example-other:example-worker-b"
		accountC     = "system:serviceaccount:example:example-worker-c"
		secretName   = "SERVICE_KEY"
		secretValue  = "service-value"
		kmsKey       = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	fakeSecrets := `{"secrets": [
		{"name": "example/agent-secrets/service-key", "kms_key_id": "` + kmsKey + `", "tags": {"owner": "` + service + `", "tier": "agent"}, "value": "` + secretValue + `"}
	]}`
	if err := os.WriteFile(fakeSecretsFile, []byte(fakeSecrets), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, boot := startBroker(t,
		"BROKER_DATABASE_URL="+databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER="+issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE="+podAudience,
		"BROKER_SERVICE_LOGINS="+service+"="+loginAccount,
		"BROKER_SERVICES="+service+"="+accountA+" "+service+"="+accountB,
	)
	if addr == "" {
		t.Fatalf("the broker logged no bound address:\n%s", boot)
	}
	requireHealthy(t, addr)
	if strings.Contains(boot, "agent secret policy refused") || strings.Contains(boot, policy.ReasonOwnerTagMalformed) {
		t.Fatalf("boot log refuses the secret %s owns, which BROKER_SERVICES registers for two accounts:\n%s", service, boot)
	}
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
		t.Logf("> %s %s\n< %d %s", method, path, response.StatusCode, bytes.TrimSpace(answer))
		return response.StatusCode, answer
	}
	decode := func(answer []byte, into any) {
		t.Helper()
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode %s: %v", answer, err)
		}
	}
	session := func(key *ecdsa.PrivateKey, enrollmentID, path string, body any) (int, []byte) {
		t.Helper()
		signed, err := proof.Sign(key, enrollmentID, http.MethodPost, base+path, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return call(http.MethodPost, path, map[string]string{"Proof": signed}, body)
	}

	t.Log("--- the service logs in once with its own token")
	launcherKey := proofKey(t)
	loginRequest, err := record.Sign(launcherKey, base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-cluster", Service: service},
	}, "", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, answer := call(http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{
		"token":   issuer.Mint(t, podKey, issuer.Claims(loginAccount, podAudience)),
		"request": loginRequest,
	})
	var login struct {
		CredentialID string `json:"credential_id"`
	}
	decode(answer, &login)
	if status != http.StatusOK || login.CredentialID == "" {
		t.Fatalf("the service's token login = %d %s, want 200 with a credential_id", status, answer)
	}

	type pod struct {
		name, account, enrollmentID string
		key                         *ecdsa.PrivateKey
	}
	pods := []*pod{{name: "A", account: accountA}, {name: "B", account: accountB}, {name: "C", account: accountC}}
	t.Log("--- the one credential enrolls a pod as each of accounts A, B and C")
	for _, p := range pods {
		p.key = proofKey(t)
		thumbprint, err := proof.Thumbprint(&p.key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		uid := "pod-" + strings.ToLower(p.name)
		claims := issuer.Claims(p.account, podAudience)
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
		signed, err := proof.SignLauncher(launcherKey, login.CredentialID, http.MethodPost, base+"/v1/enrollments", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := call(http.MethodPost, "/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
			"kind": "pod", "runtime_id": uid, "thumbprint": thumbprint, "pod_token": issuer.Mint(t, podKey, claims),
		})
		var enrolled struct {
			EnrollmentID string `json:"enrollment_id"`
		}
		decode(answer, &enrolled)
		if status != http.StatusCreated || enrolled.EnrollmentID == "" {
			t.Fatalf("enroll pod %s as %s = %d %s, want 201 with an enrollment_id", p.name, p.account, status, answer)
		}
		p.enrollmentID = enrolled.EnrollmentID
	}

	type created struct {
		State    string          `json:"state"`
		GrantID  *string         `json:"grant_id"`
		RecordID *string         `json:"record_id"`
		Approver json.RawMessage `json:"approver"`
	}
	ask := func(p *pod) created {
		t.Helper()
		request, err := record.Sign(p.key, base, []record.AuthorizationDetail{
			{Type: record.KindAgentSecret, Identifier: secretName, Actions: []string{"inject"}},
		}, "pod "+p.name+" needs it", "", nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := session(p.key, p.enrollmentID, "/v1/requests", map[string]any{"request": request, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("pod %s's POST /v1/requests = %d %s, want 200", p.name, status, answer)
		}
		var reply created
		decode(answer, &reply)
		return reply
	}

	for _, p := range pods[:2] {
		t.Logf("--- pod %s, running as one of the service's accounts, is granted its secret at once", p.name)
		reply := ask(p)
		if reply.State != "granted" || reply.GrantID == nil || reply.RecordID != nil || string(reply.Approver) != "null" {
			t.Fatalf("pod %s's request = %+v (approver %s), want granted with a grant, no record and approver null", p.name, reply, reply.Approver)
		}
		status, answer := session(p.key, p.enrollmentID, "/v1/grants/"+*reply.GrantID+"/values", nil)
		var values struct {
			Values map[string]string `json:"values"`
		}
		decode(answer, &values)
		if status != http.StatusOK || values.Values[secretName] != secretValue {
			t.Fatalf("pod %s's grant values = %d %s, want 200 with %s=%s", p.name, status, answer, secretName, secretValue)
		}
	}

	t.Log("--- pod C, running as an account no entry names, is denied it at once with no one to ask")
	reply := ask(pods[2])
	if reply.State != "denied" || reply.GrantID != nil || reply.RecordID != nil || string(reply.Approver) != "null" {
		t.Fatalf("pod C's request = %+v (approver %s), want denied with no grant, no record and approver null", reply, reply.Approver)
	}
}
