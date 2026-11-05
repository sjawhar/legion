package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"authelia.com/provider/oauth2/handler/rfc9449"
	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The origin this file's listener is reached at: a loopback name, so the Host fence an open
// listener applies serves every request below, and the origin a proof's 'htu' is checked
// against.
const listenerBrokerOrigin = "http://127.0.0.1:9020"

// listenerBrokerAccount stands in for the projected token or the machine identity a deployed
// listener presents to the broker: Account is an interface, and what this file is about is what
// the broker answers rather than how the bearer was read.
type listenerBrokerAccount string

func (a listenerBrokerAccount) Read() (string, error) { return string(a), nil }

// spentProofsInMemory is the replay store of this file's verifier. The store the listener
// deployed runs is the KV bucket every replica shares (spentProofs), which has its own test over
// a real bus (TestSpentProofsRefuseAProofTheBucketAlreadyHolds); what the branch here is about is
// what apiAuth does with each outcome, and a proof spent twice is one of them.
type spentProofsInMemory struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (s *spentProofsInMemory) CheckAndSetDPoPProofUsed(_ context.Context, jti, _, _, _, htu string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	key := jti + "\x00" + htu
	used := s.seen[key]
	s.seen[key] = true
	return used, nil
}

// spentProofsUnanswered is the spent-proof store a NATS fault leaves behind: it decides nothing,
// so the listener cannot judge the presentation at all. It is the listener's own failure, not
// the token's, which is why apiAuth answers it the 503 every unjudged presentation gets.
type spentProofsUnanswered struct{}

func (spentProofsUnanswered) CheckAndSetDPoPProofUsed(context.Context, string, string, string, string, string, time.Time) (bool, error) {
	return false, errors.New("nats: no responders available for request")
}

// listenerBrokerTokens is the credential apiAuth takes, built over broker and ready to serve, as
// a listener whose spent-proof bucket is open holds it.
func listenerBrokerTokens(t *testing.T, broker *brokertokentest.StubBroker) *brokerTokens {
	t.Helper()
	return listenerBrokerTokensOver(t, broker, &spentProofsInMemory{})
}

// listenerBrokerTokensOver is listenerBrokerTokens over the spent-proof store named, which is
// how the outcomes of that store reach apiAuth.
func listenerBrokerTokensOver(t *testing.T, broker *brokertokentest.StubBroker, replay rfc9449.DPoPReplayStorage) *brokerTokens {
	t.Helper()
	tokens := newBrokerTokens(listenerBrokerConfig{
		BrokerURL:        broker.Server.URL,
		AccountTokenFile: "/var/run/secrets/broker/token",
		Origin:           listenerBrokerOrigin,
	})
	verifier, err := brokertoken.New(brokertoken.Options{
		BrokerURL: broker.Server.URL,
		Account:   listenerBrokerAccount("listener-account-token"),
		Origin:    listenerBrokerOrigin,
		Replay:    replay,
		Address:   listenerSourceAddress,
	})
	if err != nil {
		t.Fatalf("build the broker-token verifier: %v", err)
	}
	tokens.verifier.Store(verifier)
	return tokens
}

// sessionAnswer is the broker's answer about a token issued to a session acting for a person:
// what an agent's own access token is.
func sessionAnswer(session, person, jkt string) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active:       true,
		Session:      session,
		Person:       person,
		ProjectScope: brokertoken.ProjectScopePerson,
		ClientID:     brokertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// serviceAnswer is the broker's answer about a token naming a service and no person: a machine
// nobody has signed in on.
func serviceAnswer(session, service, jkt string) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active:       true,
		Session:      session,
		Service:      service,
		Project:      "EXAMPLE",
		ProjectScope: brokertoken.ProjectScopeNamed,
		ClientID:     brokertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// brokerTokenRequest is the request a caller holding token sends to path, signed as its real
// client signs one: the proof the caller's own DPoP transport produced, on a request carrying
// this listener's JSON body.
func brokerTokenRequest(t *testing.T, caller *brokertokentest.Caller, method, path, token, body string) *http.Request {
	t.Helper()
	signed := caller.Signed(t, method, listenerBrokerOrigin+path, token)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Host = "127.0.0.1:9020"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	return request
}

// assertBrokerRejectionLog holds the listener to one rejection line per refused presentation,
// naming the class and repeating neither the token nor the proof.
func assertBrokerRejectionLog(t *testing.T, logged, wantReason, token, proof string) {
	t.Helper()
	const message = "listener: broker token rejected"
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if strings.Contains(line, message) {
			lines++
			if !strings.Contains(line, `"reason":"`+wantReason+`"`) {
				t.Fatalf("rejection line does not name reason %q: %s", wantReason, line)
			}
		}
	}
	want := 0
	if wantReason != "" {
		want = 1
	}
	if lines != want {
		t.Fatalf("logged %d rejection lines, want %d; log: %s", lines, want, logged)
	}
	if token != "" && strings.Contains(logged, token) {
		t.Fatalf("the log repeats the access token it refused: %s", logged)
	}
	if proof != "" && strings.Contains(logged, proof) {
		t.Fatalf("the log repeats the proof it refused: %s", logged)
	}
}

// The DPoP branch of /v1's authentication, outcome by outcome: who is admitted, who is refused
// and with what, and the one line each refusal writes. The branch is driven through apiAuth
// itself, as every other case in apiauth_test.go is, so the order it sits in — before the two
// bearers and before the open listener's pass-through — is part of what is proved.
func TestAPIAuthBrokerToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, ok := brokerCaller(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"session": caller.Session, "service": caller.Service, "person": caller.Person, "bound": ok})
	})

	broker := brokertokentest.NewStubBroker(t)
	caller := brokertokentest.NewCaller(t)
	stranger := brokertokentest.NewCaller(t)
	const (
		personToken   = "access-token-of-a-person"
		serviceToken  = "access-token-of-a-service"
		revokedToken  = "access-token-that-was-revoked"
		unknownToken  = "access-token-the-broker-never-issued"
		strangerToken = "access-token-bound-to-another-key"
		machineToken  = "access-token-of-a-machine-login"
	)
	broker.Says(personToken, sessionAnswer("ses_person", "ada@example.com", caller.JKT()))
	broker.Says(serviceToken, serviceAnswer("ses_service", "example-fleet", caller.JKT()))
	broker.Says(revokedToken, brokertoken.Introspection{Active: false})
	broker.Says(strangerToken, sessionAnswer("ses_person", "ada@example.com", caller.JKT()))
	// A machine login's own access token: live, bound to this key, and no session's credential.
	broker.Says(machineToken, brokertoken.Introspection{
		Active: true, ClientID: "agent-secrets-helper", Thumbprint: caller.JKT(),
	})

	cases := []struct {
		name string
		// configured is a listener that takes broker tokens, started is one whose spent-proof
		// bucket is open; the pair apart is the window between the listening socket and NATS.
		configured bool
		started    bool
		// signer is the caller whose key signs the proof, the token's own by default.
		signer     *brokertokentest.Caller
		token      string
		wantStatus int
		wantReason string
		// wantSession is the session the admitted caller is carried to the handler as.
		wantSession string
		wantService string
	}{
		{
			name:        "admits a token naming a session and a person",
			configured:  true,
			started:     true,
			token:       personToken,
			wantStatus:  http.StatusOK,
			wantSession: "ses_person",
		},
		{
			name:        "admits a token naming a service and no person",
			configured:  true,
			started:     true,
			token:       serviceToken,
			wantStatus:  http.StatusOK,
			wantSession: "ses_service",
			wantService: "example-fleet",
		},
		{
			name:       "refuses a revoked token",
			configured: true,
			started:    true,
			token:      revokedToken,
			wantStatus: http.StatusUnauthorized,
			wantReason: "not_active",
		},
		{
			name:       "refuses a token the broker never issued",
			configured: true,
			started:    true,
			token:      unknownToken,
			wantStatus: http.StatusUnauthorized,
			wantReason: "not_active",
		},
		{
			name:       "refuses a proof signed by a key the token is not bound to",
			configured: true,
			started:    true,
			signer:     stranger,
			token:      strangerToken,
			wantStatus: http.StatusUnauthorized,
			wantReason: "key_mismatch",
		},
		{
			name:       "refuses a live token that is no session's credential",
			configured: true,
			started:    true,
			token:      machineToken,
			wantStatus: http.StatusUnauthorized,
			wantReason: "not_a_session",
		},
		{
			// R9: with none of the broker variables set the listener takes no such credential,
			// so the header is one it does not know and the open listener serves the request
			// as it served every unauthenticated one before this credential existed. Refusing
			// it would lock out every client that holds a broker route of its own — the
			// loopback, no-bearer listener a GCP VM and the clustered hub run is called by
			// exactly those clients.
			name:       "serves a DPoP presentation to a listener that takes none, as it always did",
			token:      personToken,
			wantStatus: http.StatusOK,
		},
		{
			name:       "answers a DPoP presentation while the spent-proof bucket is still opening with the starting 503",
			configured: true,
			token:      personToken,
			wantStatus: http.StatusServiceUnavailable,
			wantReason: "starting",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tokens *brokerTokens
			switch {
			case tc.configured && tc.started:
				tokens = listenerBrokerTokens(t, broker)
			case tc.configured:
				tokens = newBrokerTokens(listenerBrokerConfig{
					BrokerURL:        broker.Server.URL,
					AccountTokenFile: "/var/run/secrets/broker/token",
					Origin:           listenerBrokerOrigin,
				})
			}
			signer := caller
			if tc.signer != nil {
				signer = tc.signer
			}
			request := brokerTokenRequest(t, signer, http.MethodPost, "/v1/messages/publish", tc.token, `{}`)
			recorder := httptest.NewRecorder()
			var logged bytes.Buffer

			apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", &logged), next).ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				var admitted struct {
					Session string `json:"session"`
					Service string `json:"service"`
					Bound   bool   `json:"bound"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &admitted); err != nil {
					t.Fatalf("decode what the handler was handed: %v", err)
				}
				// An admitted broker caller always names a session (a live token naming none
				// is refused not_a_session), so the request the listener bound one for is
				// exactly the one that wants a session back. The listener that takes no broker
				// token binds none: it passed the request through untouched.
				if admitted.Bound != (tc.wantSession != "") || admitted.Session != tc.wantSession || admitted.Service != tc.wantService {
					t.Fatalf("the handler was handed %+v, want session %q and service %q", admitted, tc.wantSession, tc.wantService)
				}
			} else if body := recorder.Body.String(); tc.wantStatus == http.StatusUnauthorized && strings.Contains(body, tc.wantReason) {
				// Every refusal of a credential shares one 401 body: the class belongs in the
				// operator's log and nowhere else, as the service-account branch's does. The
				// two answers that refuse nobody — the starting 503 and the broker's own — say
				// what they are, since each asks the caller to come back.
				t.Fatalf("the refusal body names the failure class: %q", body)
			}
			assertBrokerRejectionLog(t, logged.String(), tc.wantReason, tc.token, request.Header.Get("DPoP"))
		})
	}
}

// A proof is single use: the same request sent twice is refused the second time, under the class
// every other RFC 9449 Section 4.3 failure shares, and the body says no more about it than any
// other refusal does.
func TestAPIAuthRefusesAReplayedProof(t *testing.T) {
	broker := brokertokentest.NewStubBroker(t)
	caller := brokertokentest.NewCaller(t)
	const token = "access-token-replayed"
	broker.Says(token, sessionAnswer("ses_replay", "ada@example.com", caller.JKT()))
	tokens := listenerBrokerTokens(t, broker)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	signed := brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", token, `{}`)
	replay := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{}`))
	replay.Host = signed.Host
	replay.Header = signed.Header.Clone()

	first := httptest.NewRecorder()
	apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", io.Discard), next).ServeHTTP(first, signed)
	if first.Code != http.StatusNoContent {
		t.Fatalf("the first presentation: status = %d, want 204; body = %s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	var logged bytes.Buffer
	apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", &logged), next).ServeHTTP(second, replay)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("the replayed proof: status = %d, want 401; body = %s", second.Code, second.Body.String())
	}
	assertBrokerRejectionLog(t, logged.String(), "proof_invalid", token, replay.Header.Get("DPoP"))
}

// A broker that answers nothing is not a refusal of the caller's token: the token was never
// judged, so the caller is asked to come back rather than told to discard a token that may be
// perfectly live.
func TestAPIAuthAnswers503WhenTheBrokerCannotBeReached(t *testing.T) {
	broker := brokertokentest.NewStubBroker(t)
	caller := brokertokentest.NewCaller(t)
	const token = "access-token-nobody-answers-about"
	broker.Says(token, sessionAnswer("ses_unreachable", "ada@example.com", caller.JKT()))
	tokens := listenerBrokerTokens(t, broker)
	broker.Server.Close()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	recorder := httptest.NewRecorder()
	var logged bytes.Buffer
	apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", &logged), next).
		ServeHTTP(recorder, brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", token, `{}`))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	if retry := recorder.Header().Get("Retry-After"); retry == "" {
		t.Fatal("the 503 carries no Retry-After")
	}
	assertBrokerRejectionLog(t, logged.String(), "unavailable", token, "")
}

// A spent-proof store that did not answer is not a refusal of the caller's token either: the
// listener could not run its own half of the check, so nothing was decided about the token. It
// is the same 503 with the same Retry-After the broker's silence gets, and for the same reason
// Dispatch answers that failure 503 BROKER_UNAVAILABLE: a client does not retry a 401, so a
// NATS fault of a few seconds answered as one would fail every send a live token makes.
func TestAPIAuthAnswers503WhenTheSpentProofStoreCannotAnswer(t *testing.T) {
	broker := brokertokentest.NewStubBroker(t)
	caller := brokertokentest.NewCaller(t)
	const token = "access-token-whose-proof-could-not-be-recorded"
	broker.Says(token, sessionAnswer("ses_unrecorded", "ada@example.com", caller.JKT()))
	tokens := listenerBrokerTokensOver(t, broker, spentProofsUnanswered{})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	recorder := httptest.NewRecorder()
	var logged bytes.Buffer
	request := brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", token, `{}`)
	apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", &logged), next).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	if retryAfter := recorder.Header().Get("Retry-After"); retryAfter != "1" {
		t.Fatalf("Retry-After = %q, want 1: the caller is asked to come back", retryAfter)
	}
	assertBrokerRejectionLog(t, logged.String(), brokerTokenCheckFailed, token, request.Header.Get("DPoP"))
}

// RFC 9449 Section 7.2: a DPoP-bound token presented under any other scheme is refused, and a
// token presented under the DPoP scheme is never retried as a bearer. So the shared token's own
// value, sent as a DPoP presentation to a listener that holds it, is refused, and a listener
// that takes broker tokens still serves its shared bearer.
func TestAPIAuthDoesNotRetryADPoPPresentationAsABearer(t *testing.T) {
	const shared = "expected-token"
	broker := brokertokentest.NewStubBroker(t)
	tokens := listenerBrokerTokens(t, broker)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := apiAuth(shared, nil, tokens, nil, logging.NewWithWriter("test-machine", io.Discard), next)

	asDPoP := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	asDPoP.Host = "127.0.0.1:9020"
	asDPoP.Header.Set("Authorization", "DPoP "+shared)
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, asDPoP)
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("the shared token presented under the DPoP scheme: status = %d, want 401", refused.Code)
	}

	asBearer := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	asBearer.Host = "127.0.0.1:9020"
	asBearer.Header.Set("Authorization", "Bearer "+shared)
	served := httptest.NewRecorder()
	handler.ServeHTTP(served, asBearer)
	if served.Code != http.StatusNoContent {
		t.Fatalf("the shared bearer on a listener that also takes broker tokens: status = %d, want 204", served.Code)
	}
}

// Configuring broker tokens opens nothing and closes nothing: a request presenting no token at
// all is judged exactly as it was before the credential existed. The open listener serves it,
// and the Host fence it serves under still refuses a host it does not serve.
func TestAPIAuthLeavesARequestWithNoTokenToTheRuleItAlwaysHad(t *testing.T) {
	broker := brokertokentest.NewStubBroker(t)
	tokens := listenerBrokerTokens(t, broker)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", io.Discard), next)

	served := httptest.NewRecorder()
	loopback := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	loopback.Host = "127.0.0.1:9020"
	handler.ServeHTTP(served, loopback)
	if served.Code != http.StatusNoContent {
		t.Fatalf("an unauthenticated request to an open listener that takes broker tokens: status = %d, want 204; body = %s", served.Code, served.Body.String())
	}

	refused := httptest.NewRecorder()
	rebinding := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rebinding.Host = "rebind.attacker.example"
	handler.ServeHTTP(refused, rebinding)
	if refused.Code != http.StatusMisdirectedRequest {
		t.Fatalf("a rebinding host on a listener that takes broker tokens: status = %d, want 421", refused.Code)
	}

	// The fence that refuses a non-loopback listener with no bearer counts no broker token
	// either, for the same reason: this credential says who is calling, never whether /v1 may be
	// reached at all.
	if err := validateListenerAPIAuth(config.Service{ListenHost: "0.0.0.0"}, "", "", nil); err == nil {
		t.Fatal("a non-loopback listener with no bearer was allowed to start")
	}
}

// resolveListenerBrokerConfig's all-or-nothing rule, variable by variable.
func TestResolveListenerBrokerConfig(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		want        listenerBrokerConfig
		wantMissing []string
	}{
		{
			name: "no variable set takes no broker token",
			env:  map[string]string{},
		},
		{
			name: "a pod names the broker, its projected token and its own origin",
			env: map[string]string{
				EnvBrokerURL:              "https://broker.example",
				EnvBrokerAccountTokenFile: "/var/run/secrets/broker/token",
				EnvPublicURL:              "https://listener.example",
			},
			want: listenerBrokerConfig{
				BrokerURL:        "https://broker.example",
				AccountTokenFile: "/var/run/secrets/broker/token",
				Origin:           "https://listener.example",
			},
		},
		{
			name: "a VM names the audience its machine identity is minted for",
			env: map[string]string{
				EnvBrokerURL:            "https://broker.example",
				EnvBrokerGoogleAudience: "legion-broker-pod",
				EnvPublicURL:            "https://listener.example",
			},
			want: listenerBrokerConfig{
				BrokerURL:      "https://broker.example",
				GoogleAudience: "legion-broker-pod",
				Origin:         "https://listener.example",
			},
		},
		{
			name: "an account with no broker names the broker",
			env: map[string]string{
				EnvBrokerAccountTokenFile: "/var/run/secrets/broker/token",
				EnvPublicURL:              "https://listener.example",
			},
			wantMissing: []string{EnvBrokerURL},
		},
		{
			name: "an origin with no broker names the broker",
			env:  map[string]string{EnvPublicURL: "https://listener.example"},
			wantMissing: []string{
				EnvBrokerURL,
			},
		},
		{
			name: "two identities are one too many",
			env: map[string]string{
				EnvBrokerURL:              "https://broker.example",
				EnvBrokerAccountTokenFile: "/var/run/secrets/broker/token",
				EnvBrokerGoogleAudience:   "legion-broker-pod",
				EnvPublicURL:              "https://listener.example",
			},
			wantMissing: []string{EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience},
		},
		{
			name: "no identity at all names both it could be",
			env: map[string]string{
				EnvBrokerURL: "https://broker.example",
				EnvPublicURL: "https://listener.example",
			},
			wantMissing: []string{EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience},
		},
		{
			name: "a broker with no origin names the origin",
			env: map[string]string{
				EnvBrokerURL:              "https://broker.example",
				EnvBrokerAccountTokenFile: "/var/run/secrets/broker/token",
			},
			wantMissing: []string{EnvPublicURL},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, err := resolveListenerBrokerConfig(func(key string) string { return tc.env[key] })
			if len(tc.wantMissing) > 0 {
				if err == nil {
					t.Fatalf("resolveListenerBrokerConfig(%v) error = nil, want one naming %v", tc.env, tc.wantMissing)
				}
				for _, named := range tc.wantMissing {
					if !strings.Contains(err.Error(), named) {
						t.Fatalf("resolveListenerBrokerConfig(%v) error = %q, want it to name %s", tc.env, err, named)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveListenerBrokerConfig(%v) error = %v, want nil", tc.env, err)
			}
			if config != tc.want {
				t.Fatalf("resolveListenerBrokerConfig(%v) = %+v, want %+v", tc.env, config, tc.want)
			}
			if config.configured() != (tc.want != listenerBrokerConfig{}) {
				t.Fatalf("configured() = %t for %+v", config.configured(), config)
			}
		})
	}
}

// An unreadable Authorization header is no DPoP presentation: the scheme comparison is the
// library's, and a bearer, an empty scheme and a DPoP header with no token all reach the
// credential they always did.
func TestAPIAuthReadsOnlyTheDPoPSchemeAsABrokerToken(t *testing.T) {
	broker := brokertokentest.NewStubBroker(t)
	tokens := listenerBrokerTokens(t, broker)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", io.Discard), next)

	for _, authorization := range []string{"", "DPoP", "DPoP ", "Bearer something"} {
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		request.Host = "127.0.0.1:9020"
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("Authorization %q: status = %d, want 204 from the open listener; body = %s", authorization, recorder.Code, recorder.Body.String())
		}
	}
}

// R9, the whole of it for this credential: with none of the broker variables set the listener
// is the listener it was before this credential existed, whatever a client presents. A client
// sends `Authorization: DPoP` whenever it has a broker and an identity to trade and no Envoy
// bearer is set (packages/envoy-client/src/envoy-settings.ts), so it presents one to every
// listener it is pointed at — including the loopback, no-bearer listener a GCP VM and the
// clustered hub run, which takes no broker token. Such a listener reads that header as it read
// every Authorization header it did not know: the open one serves the request, the one that
// requires a bearer answers the 401 it always answered. It asks the broker nothing, binds no
// caller, and writes no refusal of a credential kind it does not take.
func TestAPIAuthWithNoBrokerSettingIsTheListenerItAlwaysWas(t *testing.T) {
	broker := brokertokentest.NewStubBroker(t)
	caller := brokertokentest.NewCaller(t)
	const token = "access-token-of-a-session-on-another-listener"
	broker.Says(token, sessionAnswer("ses_elsewhere", "ada@example.com", caller.JKT()))

	issuer := oidctest.New(t)
	verifier := discoveredAPIVerifier(t, issuer)

	bound := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, bound = brokerCaller(r)
		w.WriteHeader(http.StatusNoContent)
	})

	for _, tc := range []struct {
		name       string
		shared     string
		verifier   *apiVerifier
		wantStatus int
	}{
		{
			name:       "an open listener serves it",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "a listener that requires the shared bearer answers its own 401",
			shared:     "expected-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a listener that requires a service-account token answers its own 401",
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound = false
			var logged bytes.Buffer
			handler := apiAuth(tc.shared, tc.verifier, nil, nil, logging.NewWithWriter("test-machine", &logged), next)
			request := brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", token, `{}`)

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if body := recorder.Body.String(); body != "{\"error\":\"unauthorized\"}\n" {
					t.Fatalf("body = %q, want the one 401 body every refused credential shares", body)
				}
			} else if bound {
				t.Fatal("a listener that takes no broker token handed the route a broker caller")
			}
			if broker.Count() != 0 {
				t.Fatalf("the listener made %d introspections, want none: it takes no broker token", broker.Count())
			}
			assertBrokerRejectionLog(t, logged.String(), "", token, request.Header.Get("DPoP"))
		})
	}

	// And the boot log says what it always said: this listener names the bearers it takes and
	// no third credential.
	if got := describeListenerAPIAuth("expected-token", issuer.URL(), nil); got != "shared token and oidc "+issuer.URL() {
		t.Fatalf("describeListenerAPIAuth with no broker setting = %q, want the two bearers alone", got)
	}
}
