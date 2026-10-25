package brokertest

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TB is the part of testing.TB a Pods calls, so a command drives a running broker with the same
// code a test does: Dispatch's browser suite seeds its pending requests through ./requestseed.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Pods speaks HTTP to a running broker binary at Base as a service and its pods do: a service
// logs in with its token, its launcher credential enrolls pods that prove their accounts with
// projected tokens, and a pod asks for a secret, reads its request's state and a grant's values,
// and cancels a request. Each step logs its request and response, and fails T when the broker
// answers outside the protocol.
type Pods struct {
	T    TB
	Base string
	// Token is account's projected ServiceAccount token for the audience the broker takes pod
	// tokens for (BROKER_K8S_OIDC_AUDIENCE), bound to the pod podUID, or to no pod when podUID is
	// empty: IssuerTokens for an issuer in the test's own process.
	Token func(account, podUID string) string
}

// IssuerTokens is a Pods' Token, minted by issuer with key for audience.
func IssuerTokens(t oidctest.TB, issuer *oidctest.Issuer, key *oidctest.Key, audience string) func(account, podUID string) string {
	return func(account, podUID string) string {
		claims := issuer.Claims(account, audience)
		if podUID != "" {
			claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
		}
		return issuer.Mint(t, key, claims)
	}
}

// Launcher is a service's launcher credential and the key that proves it.
type Launcher struct {
	Key          *ecdsa.PrivateKey
	CredentialID string
}

// Pod is an enrolled pod's runtime ID, enrollment and the key that proves it.
type Pod struct {
	RuntimeID    string
	Key          *ecdsa.PrivateKey
	EnrollmentID string
}

// RequestReply is the broker's answer to POST /v1/requests and GET /v1/requests/{id}.
type RequestReply struct {
	RequestID  string          `json:"request_id"`
	State      string          `json:"state"`
	GrantID    *string         `json:"grant_id"`
	RecordID   *string         `json:"record_id"`
	Approver   json.RawMessage `json:"approver"`
	ApproveURL string          `json:"approve_url"`
}

// LogIn logs service in with a token for account and returns its launcher credential.
func (b Pods) LogIn(service, account string) Launcher {
	b.T.Helper()
	key := b.newKey()
	loginRequest, err := record.Sign(key, b.Base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-" + service, Service: service},
	}, "", "", nil, time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's login request: %v", service, err)
	}
	status, answer := b.send(http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{
		"token":   b.Token(account, ""),
		"request": loginRequest,
	})
	var login struct {
		CredentialID string `json:"credential_id"`
	}
	b.decode(answer, &login)
	if status != http.StatusOK || login.CredentialID == "" {
		b.T.Fatalf("%s's token login = %d %s, want 200 with a credential_id", service, status, answer)
	}
	return Launcher{Key: key, CredentialID: login.CredentialID}
}

// Enroll has by's credential enroll the pod runtimeID, proving with its projected token that it
// runs as account.
func (b Pods) Enroll(by Launcher, runtimeID, account string) Pod {
	b.T.Helper()
	key := b.newKey()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		b.T.Fatalf("thumbprint %s's key: %v", runtimeID, err)
	}
	signed, err := proof.SignLauncher(by.Key, by.CredentialID, http.MethodPost, b.Base+"/v1/enrollments", time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's enrollment: %v", runtimeID, err)
	}
	status, answer := b.send(http.MethodPost, "/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "pod", "runtime_id": runtimeID, "thumbprint": thumbprint, "pod_token": b.Token(account, runtimeID),
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	b.decode(answer, &enrolled)
	if status != http.StatusCreated || enrolled.EnrollmentID == "" {
		b.T.Fatalf("enroll %s as %s = %d %s, want 201 with an enrollment_id", runtimeID, account, status, answer)
	}
	return Pod{RuntimeID: runtimeID, Key: key, EnrollmentID: enrolled.EnrollmentID}
}

// Ask has p request the agent secret secretName and returns the broker's reply.
func (b Pods) Ask(p Pod, secretName string) RequestReply {
	b.T.Helper()
	request, err := record.Sign(p.Key, b.Base, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: secretName, Actions: []string{"inject"}},
	}, p.RuntimeID+" needs "+secretName, "", nil, time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's request for %s: %v", p.RuntimeID, secretName, err)
	}
	status, answer := b.signed(p, http.MethodPost, "/v1/requests", map[string]any{"request": request, "session_id": nil})
	if status != http.StatusOK {
		b.T.Fatalf("%s's POST /v1/requests for %s = %d %s, want 200", p.RuntimeID, secretName, status, answer)
	}
	var reply RequestReply
	b.decode(answer, &reply)
	return reply
}

// State reads request id's state as p, the pod that made it.
func (b Pods) State(p Pod, id string) RequestReply {
	b.T.Helper()
	path := "/v1/requests/" + id
	status, answer := b.signed(p, http.MethodGet, path, nil)
	if status != http.StatusOK {
		b.T.Fatalf("%s's GET %s = %d %s, want 200", p.RuntimeID, path, status, answer)
	}
	var reply RequestReply
	b.decode(answer, &reply)
	return reply
}

// Cancel has p, the pod that made request id, withdraw it while it is pending.
func (b Pods) Cancel(p Pod, id string) {
	b.T.Helper()
	path := "/v1/requests/" + id + "/cancel"
	if status, answer := b.signed(p, http.MethodPost, path, nil); status != http.StatusOK {
		b.T.Fatalf("%s's POST %s = %d %s, want 200", p.RuntimeID, path, status, answer)
	}
}

// Values has p read the values of its grant grantID.
func (b Pods) Values(p Pod, grantID string) map[string]string {
	b.T.Helper()
	status, answer := b.signed(p, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusOK {
		b.T.Fatalf("%s's grant values = %d %s, want 200", p.RuntimeID, status, answer)
	}
	var values struct {
		Values map[string]string `json:"values"`
	}
	b.decode(answer, &values)
	return values.Values
}

// newKey is a fresh key for a launcher credential or a pod to prove itself with.
func (b Pods) newKey() *ecdsa.PrivateKey {
	b.T.Helper()
	key, err := proof.NewKey()
	if err != nil {
		b.T.Fatalf("generate key: %v", err)
	}
	return key
}

// signed sends body to path with p's proof of its enrollment.
func (b Pods) signed(p Pod, method, path string, body any) (int, []byte) {
	b.T.Helper()
	proofHeader, err := proof.Sign(p.Key, p.EnrollmentID, method, b.Base+path, time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's proof for %s %s: %v", p.RuntimeID, method, path, err)
	}
	return b.send(method, path, map[string]string{"Proof": proofHeader}, body)
}

// send sends body as JSON to path with the auth headers, logs the exchange with neither the
// headers nor the body, since each can carry a credential, and returns the status and the
// response body.
func (b Pods) send(method, path string, auth map[string]string, body any) (int, []byte) {
	b.T.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			b.T.Fatalf("encode the body of %s %s: %v", method, path, err)
		}
	}
	request, err := http.NewRequest(method, b.Base+path, bytes.NewReader(payload))
	if err != nil {
		b.T.Fatalf("%s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range auth {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		b.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		b.T.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	b.T.Logf("> %s %s\n< %d %s", method, path, response.StatusCode, bytes.TrimSpace(answer))
	return response.StatusCode, answer
}

// decode unmarshals the JSON answer into into, failing T if it is not.
func (b Pods) decode(answer []byte, into any) {
	b.T.Helper()
	if err := json.Unmarshal(answer, into); err != nil {
		b.T.Fatalf("decode %s: %v", answer, err)
	}
}
