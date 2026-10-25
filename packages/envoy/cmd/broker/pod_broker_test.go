package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// podBroker speaks HTTP to a running broker binary at base as a service and its pods do: a
// service logs in with its token, its launcher credential enrolls pods that prove their accounts
// with projected tokens the issuer signs with key for audience, and a pod asks for a secret and
// reads a grant's values. Each step logs its request and response, and fails t when the broker
// answers outside the protocol.
type podBroker struct {
	t        *testing.T
	base     string
	issuer   *oidctest.Issuer
	key      *oidctest.Key
	audience string
}

// launcher is a service's launcher credential and the key that proves it.
type launcher struct {
	key          *ecdsa.PrivateKey
	credentialID string
}

// pod is an enrolled pod's runtime ID, enrollment and the key that proves it.
type pod struct {
	runtimeID    string
	key          *ecdsa.PrivateKey
	enrollmentID string
}

// requestReply is the broker's answer to POST /v1/requests.
type requestReply struct {
	State    string          `json:"state"`
	GrantID  *string         `json:"grant_id"`
	RecordID *string         `json:"record_id"`
	Approver json.RawMessage `json:"approver"`
}

// logIn logs service in with a token for account and returns its launcher credential.
func (b podBroker) logIn(service, account string) launcher {
	b.t.Helper()
	key := proofKey(b.t)
	loginRequest, err := record.Sign(key, b.base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-" + service, Service: service},
	}, "", "", nil, time.Now())
	if err != nil {
		b.t.Fatal(err)
	}
	status, answer := b.post("/v1/launcher-credentials/token", nil, map[string]any{
		"token":   b.issuer.Mint(b.t, b.key, b.issuer.Claims(account, b.audience)),
		"request": loginRequest,
	})
	var login struct {
		CredentialID string `json:"credential_id"`
	}
	b.decode(answer, &login)
	if status != http.StatusOK || login.CredentialID == "" {
		b.t.Fatalf("%s's token login = %d %s, want 200 with a credential_id", service, status, answer)
	}
	return launcher{key: key, credentialID: login.CredentialID}
}

// enroll has by's credential enroll the pod runtimeID, proving with its projected token that it
// runs as account.
func (b podBroker) enroll(by launcher, runtimeID, account string) pod {
	b.t.Helper()
	key := proofKey(b.t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		b.t.Fatal(err)
	}
	claims := b.issuer.Claims(account, b.audience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": runtimeID}}
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodPost, b.base+"/v1/enrollments", time.Now())
	if err != nil {
		b.t.Fatal(err)
	}
	status, answer := b.post("/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "pod", "runtime_id": runtimeID, "thumbprint": thumbprint, "pod_token": b.issuer.Mint(b.t, b.key, claims),
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	b.decode(answer, &enrolled)
	if status != http.StatusCreated || enrolled.EnrollmentID == "" {
		b.t.Fatalf("enroll %s as %s = %d %s, want 201 with an enrollment_id", runtimeID, account, status, answer)
	}
	return pod{runtimeID: runtimeID, key: key, enrollmentID: enrolled.EnrollmentID}
}

// ask has p request the agent secret secretName and returns the broker's reply.
func (b podBroker) ask(p pod, secretName string) requestReply {
	b.t.Helper()
	request, err := record.Sign(p.key, b.base, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: secretName, Actions: []string{"inject"}},
	}, p.runtimeID+" needs "+secretName, "", nil, time.Now())
	if err != nil {
		b.t.Fatal(err)
	}
	status, answer := b.signed(p, "/v1/requests", map[string]any{"request": request, "session_id": nil})
	if status != http.StatusOK {
		b.t.Fatalf("%s's POST /v1/requests for %s = %d %s, want 200", p.runtimeID, secretName, status, answer)
	}
	var reply requestReply
	b.decode(answer, &reply)
	return reply
}

// values has p read the values of its grant grantID.
func (b podBroker) values(p pod, grantID string) map[string]string {
	b.t.Helper()
	status, answer := b.signed(p, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusOK {
		b.t.Fatalf("%s's grant values = %d %s, want 200", p.runtimeID, status, answer)
	}
	var values struct {
		Values map[string]string `json:"values"`
	}
	b.decode(answer, &values)
	return values.Values
}

// signed POSTs body to path with p's proof of its enrollment.
func (b podBroker) signed(p pod, path string, body any) (int, []byte) {
	b.t.Helper()
	proofHeader, err := proof.Sign(p.key, p.enrollmentID, http.MethodPost, b.base+path, time.Now())
	if err != nil {
		b.t.Fatal(err)
	}
	return b.post(path, map[string]string{"Proof": proofHeader}, body)
}

// post POSTs body as JSON to path with the auth headers, logs the exchange and returns the
// status and response body.
func (b podBroker) post(path string, auth map[string]string, body any) (int, []byte) {
	b.t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			b.t.Fatal(err)
		}
	}
	request, err := http.NewRequest(http.MethodPost, b.base+path, bytes.NewReader(payload))
	if err != nil {
		b.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range auth {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		b.t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Logf("> POST %s\n< %d %s", path, response.StatusCode, bytes.TrimSpace(answer))
	return response.StatusCode, answer
}

// decode unmarshals the JSON answer into into, failing t if it is not.
func (b podBroker) decode(answer []byte, into any) {
	b.t.Helper()
	if err := json.Unmarshal(answer, into); err != nil {
		b.t.Fatalf("decode %s: %v", answer, err)
	}
}
