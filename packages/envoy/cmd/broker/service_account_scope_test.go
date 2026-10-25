package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestAnAccountProvesOnlyTheServiceItIsListedFor drives the REAL compiled broker on a real
// Postgres, over HTTP alone, with two registered services that each log in with a token of their
// own: example-service runs its pods as two accounts, other-service as one. A pod's service is
// its launcher's only when BROKER_SERVICES lists the pod's account for that same service, so an
// account listed for one service proves nothing under another service's launcher credential: a
// pod enrolled by other-service's credential while running as one of example-service's accounts,
// and a pod enrolled by example-service's credential while running as other-service's account,
// are each denied both services' secrets, with no grant, no record and no approver. The pod
// running as other-service's own account under its credential is granted that service's secret,
// so the denials are the account check and not a service that cannot be served.
func TestAnAccountProvesOnlyTheServiceItIsListedFor(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		otherService = "other-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		otherLogin   = "system:serviceaccount:other-system:other-service-login"
		accountA     = "system:serviceaccount:example:example-worker-a"
		accountB     = "system:serviceaccount:example-other:example-worker-b"
		accountD     = "system:serviceaccount:other:other-worker"
		secretName   = "SERVICE_KEY"
		otherSecret  = "OTHER_KEY"
		kmsKey       = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	fakeSecrets := `{"secrets": [
		{"name": "example/agent-secrets/service-key", "kms_key_id": "` + kmsKey + `", "tags": {"owner": "` + service + `", "tier": "agent"}, "value": "service-value"},
		{"name": "example/agent-secrets/other-key", "kms_key_id": "` + kmsKey + `", "tags": {"owner": "` + otherService + `", "tier": "agent"}, "value": "other-value"}
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
		"BROKER_SERVICE_LOGINS="+service+"="+loginAccount+" "+otherService+"="+otherLogin,
		"BROKER_SERVICES="+service+"="+accountA+" "+service+"="+accountB+" "+otherService+"="+accountD,
	)
	if addr == "" {
		t.Fatalf("the broker logged no bound address:\n%s", boot)
	}
	requireHealthy(t, addr)
	base := "http://" + addr

	post := func(path string, auth map[string]string, body any) (int, []byte) {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			if payload, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		for name, value := range auth {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("> POST %s\n< %d %s", path, response.StatusCode, bytes.TrimSpace(answer))
		return response.StatusCode, answer
	}
	decode := func(answer []byte, into any) {
		t.Helper()
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode %s: %v", answer, err)
		}
	}

	type launcher struct {
		key          *ecdsa.PrivateKey
		credentialID string
	}
	logIn := func(name, account string) launcher {
		t.Helper()
		key := proofKey(t)
		loginRequest, err := record.Sign(key, base, []record.AuthorizationDetail{
			{Type: record.KindLauncherCredential, Identifier: "example-host-" + name, Service: name},
		}, "", "", nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := post("/v1/launcher-credentials/token", nil, map[string]any{
			"token":   issuer.Mint(t, podKey, issuer.Claims(account, podAudience)),
			"request": loginRequest,
		})
		var login struct {
			CredentialID string `json:"credential_id"`
		}
		decode(answer, &login)
		if status != http.StatusOK || login.CredentialID == "" {
			t.Fatalf("%s's token login = %d %s, want 200 with a credential_id", name, status, answer)
		}
		return launcher{key: key, credentialID: login.CredentialID}
	}

	type pod struct {
		key          *ecdsa.PrivateKey
		enrollmentID string
	}
	enroll := func(by launcher, uid, account string) pod {
		t.Helper()
		key := proofKey(t)
		thumbprint, err := proof.Thumbprint(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		claims := issuer.Claims(account, podAudience)
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
		signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodPost, base+"/v1/enrollments", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := post("/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
			"kind": "pod", "runtime_id": uid, "thumbprint": thumbprint, "pod_token": issuer.Mint(t, podKey, claims),
		})
		var enrolled struct {
			EnrollmentID string `json:"enrollment_id"`
		}
		decode(answer, &enrolled)
		if status != http.StatusCreated || enrolled.EnrollmentID == "" {
			t.Fatalf("enroll %s as %s = %d %s, want 201 with an enrollment_id", uid, account, status, answer)
		}
		return pod{key: key, enrollmentID: enrolled.EnrollmentID}
	}

	type created struct {
		State    string          `json:"state"`
		GrantID  *string         `json:"grant_id"`
		RecordID *string         `json:"record_id"`
		Approver json.RawMessage `json:"approver"`
	}
	ask := func(p pod, name string) created {
		t.Helper()
		request, err := record.Sign(p.key, base, []record.AuthorizationDetail{
			{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}},
		}, "needs "+name, "", nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		signed, err := proof.Sign(p.key, p.enrollmentID, http.MethodPost, base+"/v1/requests", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := post("/v1/requests", map[string]string{"Proof": signed}, map[string]any{"request": request, "session_id": nil})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/requests for %s = %d %s, want 200", name, status, answer)
		}
		var reply created
		decode(answer, &reply)
		return reply
	}

	exampleLauncher := logIn(service, loginAccount)
	otherLauncher := logIn(otherService, otherLogin)

	t.Log("--- other-service's own pod, under its credential, is granted its secret")
	own := ask(enroll(otherLauncher, "pod-other-own", accountD), otherSecret)
	if own.State != "granted" || own.GrantID == nil || own.RecordID != nil || string(own.Approver) != "null" {
		t.Fatalf("other-service's own pod's request = %+v (approver %s), want granted with no record and approver null", own, own.Approver)
	}

	for who, p := range map[string]pod{
		"a pod under other-service's credential running as example-service's account A": enroll(otherLauncher, "pod-other-as-a", accountA),
		"a pod under other-service's credential running as example-service's account B": enroll(otherLauncher, "pod-other-as-b", accountB),
		"a pod under example-service's credential running as other-service's account":   enroll(exampleLauncher, "pod-example-as-d", accountD),
	} {
		for _, name := range []string{secretName, otherSecret} {
			t.Logf("--- %s asks for %s", who, name)
			reply := ask(p, name)
			if reply.State != "denied" || reply.GrantID != nil || reply.RecordID != nil || string(reply.Approver) != "null" {
				t.Errorf("%s asking for %s = %+v (approver %s), want denied with no grant, no record and approver null", who, name, reply, reply.Approver)
			}
		}
	}
}
