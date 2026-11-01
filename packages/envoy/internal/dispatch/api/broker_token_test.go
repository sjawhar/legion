// broker_token_test.go drives the real server with the fifth credential kind: an access token the
// secrets broker issued, presented under the RFC 9449 DPoP scheme. The broker is a stub answering
// what each test says a token is, and every proof comes from internal/broker/helper's DPoP
// transport — the producer a real caller uses — so no second signer of this package's own can
// drift from it. Both stand-ins are internal/brokertoken/brokertokentest's, the ones that
// package's own suite drives.
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

const (
	// brokerTestSession is the session the broker says its tokens act for, and brokerTestPerson
	// the person it says they belong to: one of the two people newTestServer seeds (alice and
	// bob), so a token this suite presents names someone the server already knows.
	brokerTestSession = "ses_b1c2d3e4f5a6"
	brokerTestPerson  = "alice"
	// brokerTestProject is the one project a named-scope token may write.
	brokerTestProject = "BRK"
	// brokerTestAddress is the source address the verifier's per-address bucket is keyed on.
	brokerTestAddress = "203.0.113.9:40001"
)

// liveNamedToken is the broker's answer for a live token bound to jkt and scoped to one
// project: one the RFC 8693 exchange issued for acting as a session, which is what every token
// Dispatch acts on is.
func liveNamedToken(jkt string) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active:       true,
		Session:      brokerTestSession,
		Person:       brokerTestPerson,
		ProjectScope: brokertoken.ProjectScopeNamed,
		Project:      brokerTestProject,
		Scope:        "dispatch.write",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		ClientID:     brokertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// machineLoginToken is the broker's answer for a machine login's own access token: the
// credential the agent-secrets helper holds to ask the broker for everything else. It is live
// and bound to the caller's key like any other, and the broker names no session, no person and
// no project scope for it, no exchange having decided one.
func machineLoginToken(jkt string) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active:     true,
		Scope:      "machine",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
		ClientID:   "agent-secrets-helper",
		Thumbprint: jkt,
	}
}

// brokerCaller is an agent holding one key, producing the proofs its real client produces.
type brokerCaller struct {
	caller *brokertokentest.Caller
	jkt    string
}

// newBrokerCaller mints a caller's key and the transport that proves it.
func newBrokerCaller(t *testing.T) *brokerCaller {
	t.Helper()
	caller := brokertokentest.NewCaller(t)
	return &brokerCaller{caller: caller, jkt: caller.JKT()}
}

// presenting is the request a caller sends Dispatch: the server-side request for path, carrying
// token under the DPoP scheme and a proof of this caller's key signed for that method and target.
// Each call signs a fresh proof, as a real client does.
func (c *brokerCaller) presenting(t *testing.T, method, path, token string, body any) *http.Request {
	t.Helper()
	signed := c.caller.Signed(t, method, testServerURL+path, token)
	return brokerRequest(t, method, path, body, signed.Header.Get("Authorization"), signed.Header.Get("DPoP"))
}

// presentingUpload is the multipart upload a caller sends Dispatch, carrying token under the
// DPoP scheme and a fresh proof for that target. A proof names the method and the target URI
// and never the body, so the upload's bytes are the request's own, as they are for a browser.
func (c *brokerCaller) presentingUpload(t *testing.T, path, token, filename, contentType string, content []byte) *http.Request {
	t.Helper()
	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": filename}, filename, contentType, content)
	if err != nil {
		t.Fatalf("build the upload: %v", err)
	}
	signed := c.caller.Signed(t, http.MethodPost, testServerURL+path, token)
	request := httptest.NewRequest(http.MethodPost, path, body)
	request.RemoteAddr = brokerTestAddress
	request.Header.Set("Content-Type", bodyType)
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	return request
}

// brokerRequest is the server-side request for path with the credential headers given, so a test
// can replay one proof, present a token under another scheme, or send a body the proof never
// covered (a proof names the method and the target URI, never the body).
func brokerRequest(t *testing.T, method, path string, body any, authorization, dpop string) *http.Request {
	t.Helper()
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = brokerTestAddress
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if dpop != "" {
		request.Header.Set("DPoP", dpop)
	}
	return request
}

// brokerTokenRig is the server under test, the broker it asks and the store behind it.
type brokerTokenRig struct {
	handler  http.Handler
	deps     Deps
	database *store.Store
	broker   *brokertokentest.StubBroker
}

// newBrokerTokenRig is the package's own test server (newTestServer) with the broker-token check
// wired to a stub broker: the replay store and the rest of the API on one database, as a
// deployment has them. Each option is applied to the verifier's own options before it is built,
// for a test that needs a bucket it can reach.
func newBrokerTokenRig(t *testing.T, options ...func(*brokertoken.Options)) *brokerTokenRig {
	t.Helper()
	broker := brokertokentest.NewStubBroker(t)
	handler, database, deps := newTestServer(t, testServerOptions{
		brokerTokens: func(database *store.Store) *brokertoken.Verifier {
			opts := brokertoken.Options{
				BrokerURL: broker.Server.URL,
				Account:   brokertoken.NewAccountToken(accountTokenFile(t, credentialTestAccountToken)),
				Origin:    testServerURL,
				Replay:    store.NewDPoPProofs(database.Pool),
				// What Dispatch itself passes: the check keys its bucket on the address
				// Dispatch's own proxy-trust rule reads, never one of its own.
				Address: auth.ForwardedAddress,
			}
			for _, option := range options {
				option(&opts)
			}
			verifier, err := brokertoken.New(opts)
			if err != nil {
				t.Fatalf("build the broker token verifier: %v", err)
			}
			return verifier
		},
	})
	return &brokerTokenRig{handler: handler, deps: deps, database: database, broker: broker}
}

// serve carries one request through the server under test.
func (r *brokerTokenRig) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response
}

// whoamiAnswer is what GET /api/v1/whoami tells an agent caller.
type whoamiAnswer struct {
	Kind    string  `json:"kind"`
	Login   string  `json:"login"`
	Owner   *string `json:"owner"`
	Service *string `json:"service"`
	Session *string `json:"session"`
	Project *string `json:"project"`
}

// refusal is the code and message a refused request carries.
type refusalAnswer struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// TestALiveBrokerTokenWithASoundProofIsAccepted: the two checks together admit the caller, and
// whoami reports the session the token proved, the person it acts for and the project it may
// write.
func TestALiveBrokerTokenWithASoundProofIsAccepted(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-the-broker-issued"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("whoami under a broker token: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := decodeBody[whoamiAnswer](t, response)
	if answer.Kind != "agent" || answer.Session == nil || *answer.Session != brokerTestSession {
		t.Fatalf("whoami = %+v, want the session the token names", answer)
	}
	if answer.Owner == nil || *answer.Owner != brokerTestPerson {
		t.Fatalf("whoami owner = %v, want %q", answer.Owner, brokerTestPerson)
	}
	if answer.Project == nil || *answer.Project != brokerTestProject {
		t.Fatalf("whoami project = %v, want %q", answer.Project, brokerTestProject)
	}
	if answer.Service != nil {
		t.Fatalf("whoami service = %v, want null: no service-account token authenticated this", *answer.Service)
	}
}

// TestAPersonScopedBrokerTokenCarriesNoProject: a dev box session's token names no single
// project, so the caller carries none at all rather than an empty one.
func TestAPersonScopedBrokerTokenCarriesNoProject(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-dev-box-session-token"
	answer := liveNamedToken(caller.jkt)
	answer.ProjectScope, answer.Project = brokertoken.ProjectScopePerson, ""
	rig.broker.Says(token, answer)

	response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("whoami under a person-scoped token: status=%d body=%s", response.Code, response.Body.String())
	}
	decided := decodeBody[whoamiAnswer](t, response)
	if decided.Project != nil {
		t.Fatalf("whoami project = %q, want null for a person-scoped token", *decided.Project)
	}
	if decided.Session == nil || *decided.Session != brokerTestSession {
		t.Fatalf("whoami = %+v, want the session the token names", decided)
	}
}

// TestABrokerTokenWritesAsTheSessionItProves: the body's actor does not decide the session, which
// the token carries. An absent one and a matching one are both taken, a matching one's origin is
// kept because nothing else carries it, and the write is recorded as the token's session with the
// person and project the token named. The project the writes go into is made by a person: a
// token scoped to one project may not create another (project_scope.go).
func TestABrokerTokenWritesAsTheSessionItProves(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-writes"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the project as a person: status=%d body=%s", created.Code, created.Body.String())
	}

	issue := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject,
		"title":   "Written by the token's session",
		"spec":    "# Spec",
		"actor": map[string]any{
			"kind":   "session",
			"id":     brokerTestSession,
			"origin": map[string]string{"cwd": "/workspace", "host": "example-host-one", "pane": "%7"},
		},
	}))
	if issue.Code != http.StatusCreated {
		t.Fatalf("create an issue with the matching actor: status=%d body=%s", issue.Code, issue.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key

	noActor := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", token, map[string]any{
		"body": "Written with no actor in the body.",
	}))
	if noActor.Code != http.StatusCreated {
		t.Fatalf("comment with no actor in the body: status=%d body=%s", noActor.Code, noActor.Body.String())
	}

	listed := rig.serve(withPerson(brokerRequest(t, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "", ""), brokerTestPerson))
	if listed.Code != http.StatusOK {
		t.Fatalf("read the issue's events: status=%d body=%s", listed.Code, listed.Body.String())
	}
	log := decodeBody[[]struct {
		Type  string `json:"type"`
		Actor struct {
			Kind    string  `json:"kind"`
			ID      string  `json:"id"`
			Owner   *string `json:"owner"`
			Project *string `json:"project"`
			Origin  *struct {
				Cwd  string `json:"cwd"`
				Host string `json:"host"`
				Pane string `json:"pane"`
			} `json:"origin"`
		} `json:"actor"`
	}](t, listed)
	if len(log) == 0 {
		t.Fatalf("the issue has no events: %s", listed.Body.String())
	}
	actor := log[0].Actor
	if actor.Kind != "session" || actor.ID != brokerTestSession {
		t.Fatalf("the write's actor = %s:%s, want the session the token proved", actor.Kind, actor.ID)
	}
	if actor.Owner == nil || *actor.Owner != brokerTestPerson {
		t.Fatalf("the write's owner = %v, want %q", actor.Owner, brokerTestPerson)
	}
	if actor.Project == nil || *actor.Project != brokerTestProject {
		t.Fatalf("the write's project = %v, want %q", actor.Project, brokerTestProject)
	}
	if actor.Origin == nil || actor.Origin.Cwd != "/workspace" || actor.Origin.Host != "example-host-one" || actor.Origin.Pane != "%7" {
		t.Fatalf("the write's origin = %+v, want the body's cwd, host and pane kept", actor.Origin)
	}
}

// withPerson signs a request in as a person through the test header identity.
func withPerson(request *http.Request, login string) *http.Request {
	request.Header.Set("X-Dispatch-User", login)
	return request
}

// TestABrokerTokenRefusesABodyActorNamingAnotherSession: the one thing a body actor may not do is
// name a writer that is not the credential's, and the refusal names the session the token carries
// so the caller can see what it should have sent.
func TestABrokerTokenRefusesABodyActorNamingAnotherSession(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-whose-body-lies"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")

	for name, actor := range map[string]any{
		"another session": map[string]string{"kind": "session", "id": "ses_someone_else"},
		"a person":        map[string]string{"kind": "user", "id": brokerTestPerson},
	} {
		t.Run(name, func(t *testing.T) {
			response := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+own.issue+"/comments", token, map[string]any{
				"body": "Written as someone else.", "actor": actor,
			}))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s, want 403", response.Code, response.Body.String())
			}
			refusal := decodeBody[refusalAnswer](t, response)
			if refusal.Code != "ACTOR_SESSION_MISMATCH" || !strings.Contains(refusal.Error, brokerTestSession) {
				t.Fatalf("refusal = %+v, want ACTOR_SESSION_MISMATCH naming %s", refusal, brokerTestSession)
			}
		})
	}
}

// TestABrokerTokenProvedByAnotherKeyIsRefused: a sound proof signed by a key the token is not
// bound to is what a stolen token looks like, and it gets its own code.
func TestABrokerTokenProvedByAnotherKeyIsRefused(t *testing.T) {
	rig := newBrokerTokenRig(t)
	owner, thief := newBrokerCaller(t), newBrokerCaller(t)
	const token = "a-token-bound-to-its-owners-key"
	rig.broker.Says(token, liveNamedToken(owner.jkt))

	response := rig.serve(thief.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", response.Code, response.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, response); refusal.Code != "BROKER_TOKEN_KEY_MISMATCH" {
		t.Fatalf("refusal = %+v, want BROKER_TOKEN_KEY_MISMATCH", refusal)
	}
}

// TestBrokerTokenRefusalsEachHaveTheirOwnCode: a token the broker does not hold as live, a proof
// already spent, and a broker that answers nothing are three different things to a caller, and
// only the last is a 503 — a caller told its token was refused would discard a live one.
func TestBrokerTokenRefusalsEachHaveTheirOwnCode(t *testing.T) {
	t.Run("not live", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-the-broker-revoked"
		rig.broker.Says(token, brokertoken.Introspection{Active: false})

		response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_ACTIVE" {
			t.Fatalf("status=%d refusal=%+v, want 401 BROKER_TOKEN_NOT_ACTIVE", response.Code, refusal)
		}
		if strings.Contains(refusal.Error, token) {
			t.Fatalf("the refusal repeats the token: %q", refusal.Error)
		}
	})
	t.Run("a spent proof", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-whose-proof-is-captured"
		rig.broker.Says(token, liveNamedToken(caller.jkt))

		presented := caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil)
		replay := brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil,
			presented.Header.Get("Authorization"), presented.Header.Get("DPoP"))
		if first := rig.serve(presented); first.Code != http.StatusOK {
			t.Fatalf("the first presentation: status=%d body=%s", first.Code, first.Body.String())
		}
		response := rig.serve(replay)
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_PROOF_INVALID" {
			t.Fatalf("status=%d refusal=%+v, want 401 BROKER_TOKEN_PROOF_INVALID", response.Code, refusal)
		}
	})
	t.Run("the broker unreachable", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-nobody-can-be-asked-about"
		rig.broker.Server.Close()

		response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusServiceUnavailable || refusal.Code != "BROKER_UNAVAILABLE" {
			t.Fatalf("status=%d refusal=%+v, want 503 BROKER_UNAVAILABLE", response.Code, refusal)
		}
	})
}

// TestAMachineLoginsOwnTokenIsRefusedAndNeverFilledInFromTheBody: the broker sets a session and
// a person only from an exchange's claims, so a machine login's own access token — live, bound
// to the caller's key, issued to the agent-secrets helper — answers with neither. Dispatch
// records every write under the session its credential proved, so such a token is refused by
// name: whoami will not say who it is, and a write will not take the session its body names.
func TestAMachineLoginsOwnTokenIsRefusedAndNeverFilledInFromTheBody(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-machine-logins-own-access-token"
	rig.broker.Says(token, machineLoginToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	who := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	refusal := decodeBody[refusalAnswer](t, who)
	if who.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_A_SESSION" {
		t.Fatalf("whoami: status=%d refusal=%+v, want 401 BROKER_TOKEN_NOT_A_SESSION", who.Code, refusal)
	}
	if strings.Contains(refusal.Error, token) {
		t.Fatalf("the refusal repeats the token: %q", refusal.Error)
	}

	const chosen = "ses-chosen-by-the-body"
	written := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject, "title": "Filed by a machine login", "spec": "# Spec",
		"actor": map[string]any{"kind": "session", "id": chosen},
	}))
	refused := decodeBody[refusalAnswer](t, written)
	if written.Code != http.StatusUnauthorized || refused.Code != "BROKER_TOKEN_NOT_A_SESSION" {
		t.Fatalf("a write: status=%d body=%s, want 401 BROKER_TOKEN_NOT_A_SESSION", written.Code, written.Body.String())
	}
	if strings.Contains(written.Body.String(), chosen) {
		t.Fatalf("the refusal carries the session the body named: %s", written.Body.String())
	}
	issues := rig.asPerson(t, http.MethodGet, "/api/v1/issues?project="+brokerTestProject, nil)
	if strings.Contains(issues.Body.String(), "Filed by a machine login") {
		t.Fatalf("the refused write created an issue: %s", issues.Body.String())
	}
}

// TestABrokerTokenUnderTheBearerSchemeIsRefusedAsItAlwaysWas: RFC 9449 Section 7.2 refuses a
// DPoP-bound token presented under any other scheme, and Dispatch's bearer branches are untouched
// — such a presentation is the unknown personal token it has always been, and the broker is never
// asked about it.
func TestABrokerTokenUnderTheBearerSchemeIsRefusedAsItAlwaysWas(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-presented-the-wrong-way"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	response := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+token, ""))
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusUnauthorized || refusal.Code != "UNAUTHORIZED" {
		t.Fatalf("status=%d refusal=%+v, want 401 UNAUTHORIZED", response.Code, refusal)
	}
	if rig.broker.Count() != 0 {
		t.Fatalf("the broker was asked %d times about a bearer presentation, want none", rig.broker.Count())
	}
}

// TestADPoPPresentationWithoutABrokerIsRefusedAsItAlwaysWas: a Dispatch with no broker configured
// has no such credential kind, and the answer is what any Authorization header that is not a
// bearer has always been here.
func TestADPoPPresentationWithoutABrokerIsRefusedAsItAlwaysWas(t *testing.T) {
	handler := newTestHandler(t)
	caller := newBrokerCaller(t)
	presented := caller.presenting(t, http.MethodGet, "/api/v1/whoami", "a-token-nobody-here-decides", nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, presented)
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusUnauthorized || refusal.Code != "UNAUTHORIZED" || refusal.Error != "invalid bearer token" {
		t.Fatalf("status=%d refusal=%+v, want 401 UNAUTHORIZED invalid bearer token", response.Code, refusal)
	}
}

// TestTheOtherCredentialsAreUnaffectedByTheBrokerBranch: a cookie caller and a personal-token
// caller resolve exactly as they did, on a server that decides broker tokens too — the personal
// token's write still has to name its own session, since that credential proves none.
func TestTheOtherCredentialsAreUnaffectedByTheBrokerBranch(t *testing.T) {
	rig := newBrokerTokenRig(t)

	person := rig.serve(withPerson(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "", ""), brokerTestPerson))
	if person.Code != http.StatusOK {
		t.Fatalf("whoami as a person: status=%d body=%s", person.Code, person.Body.String())
	}
	if answer := decodeBody[whoamiAnswer](t, person); answer.Kind != "user" || answer.Login != brokerTestPerson {
		t.Fatalf("whoami as a person = %+v, want the signed-in person", answer)
	}

	minted := rig.serve(withPerson(brokerRequest(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "laptop"}, "", ""), brokerTestPerson))
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a personal token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[struct {
		Token string `json:"token"`
	}](t, minted).Token
	if personal == "" {
		t.Fatalf("the minted token is empty: %s", minted.Body.String())
	}

	agent := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+personal, ""))
	if agent.Code != http.StatusOK {
		t.Fatalf("whoami under a personal token: status=%d body=%s", agent.Code, agent.Body.String())
	}
	answer := decodeBody[whoamiAnswer](t, agent)
	if answer.Kind != "agent" || answer.Owner == nil || *answer.Owner != brokerTestPerson {
		t.Fatalf("whoami under a personal token = %+v, want the token's owner", answer)
	}
	if answer.Session != nil || answer.Project != nil {
		t.Fatalf("whoami under a personal token = %+v, want no session and no project: it proves neither", answer)
	}

	unnamed := rig.serve(brokerRequest(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "PRS", "name": "Personal"}, "Bearer "+personal, ""))
	refusal := decodeBody[refusalAnswer](t, unnamed)
	if unnamed.Code != http.StatusBadRequest || refusal.Code != "ACTOR_KIND" {
		t.Fatalf("a personal token writing with no actor: status=%d refusal=%+v, want 400 ACTOR_KIND", unnamed.Code, refusal)
	}
}

// TestABrokerTokensReresolutionIsNotProofCheckedAgain: a connection that outlives its request
// resolves its caller again from a detached copy of that request, whose proof is already spent. A
// second request carrying that proof is refused as the replay it is, while the re-resolution of
// the connection it authenticated asks only whether the token is still live — which is what
// whileCallerResolves calls on every heartbeat (callerStillResolves), driven here directly.
func TestABrokerTokensReresolutionIsNotProofCheckedAgain(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-holding-a-socket-open"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	served := directServer(rig.deps)

	admitted := caller.presenting(t, http.MethodGet, "/ws/live", token, nil)
	actor, human, err := served.optionalActor(admitted)
	if err != nil || human || actor.ID != brokerTestSession {
		t.Fatalf("the upgrade's caller = %+v, human=%v, err=%v; want the token's session", actor, human, err)
	}

	replayed := brokerRequest(t, http.MethodGet, "/ws/live", nil, admitted.Header.Get("Authorization"), admitted.Header.Get("DPoP"))
	if _, _, err := served.optionalActor(replayed); err == nil {
		t.Fatal("a fresh request carrying the spent proof resolved; want it refused as a replay")
	}

	// Two heartbeats of the socket that proof opened. Both carry it, and neither is refused:
	// the re-resolution never looks at it.
	for beat := range 2 {
		if err := served.callerStillResolves(admitted); err != nil {
			t.Fatalf("heartbeat %d: %v, want the caller to keep resolving", beat, err)
		}
	}

	// Once the broker stops holding the token live, the re-resolution refuses and the connection
	// closes. The affirmative answer is remembered for a few seconds, so this reads past it.
	rig.broker.Says(token, brokertoken.Introspection{Active: false})
	time.Sleep(brokertoken.AffirmativeCacheTTL + 50*time.Millisecond)
	if err := served.callerStillResolves(admitted); err == nil {
		t.Fatal("a revoked token kept resolving on an open connection")
	}
}

// TestTheRateRefusalCarriesTheWaitInItsHeader: the introspection bucket's refusal tells the
// caller when to come back in Retry-After as well as in its prose, as the broker's own 429s do,
// so a client reads the wait without parsing a sentence. The bucket here holds one request, so
// the second presentation of a token the broker has not been asked about meets it.
func TestTheRateRefusalCarriesTheWaitInItsHeader(t *testing.T) {
	rig := newBrokerTokenRig(t, func(o *brokertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Minute, Burst: 1}
	})
	caller := newBrokerCaller(t)

	spent := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", "the-token-that-spends-the-bucket", nil))
	if refusal := decodeBody[refusalAnswer](t, spent); spent.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_ACTIVE" {
		t.Fatalf("the request that spends the bucket: status=%d refusal=%+v, want 401 BROKER_TOKEN_NOT_ACTIVE", spent.Code, refusal)
	}

	limited := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", "a-second-token", nil))
	refusal := decodeBody[refusalAnswer](t, limited)
	if limited.Code != http.StatusTooManyRequests || refusal.Code != "BROKER_TOKEN_RATE_LIMITED" {
		t.Fatalf("status=%d refusal=%+v, want 429 BROKER_TOKEN_RATE_LIMITED", limited.Code, refusal)
	}
	if got := limited.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want the bucket's minute as whole seconds", got)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the refused request reached the broker: %d introspections, want 1", rig.broker.Count())
	}
}
