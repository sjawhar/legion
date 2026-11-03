package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// brokerRig is a listener's /v1 as a deployment runs it, with every piece of the broker-token
// credential real but the broker itself: the routes over a real bus (internal/testnats), the
// spent-proof bucket that bus holds, the verifier brokerTokens.open builds over it, and a caller
// whose proofs come from the DPoP transport its real client uses. The broker is the stub, which
// answers what each test says each token is.
type brokerRig struct {
	deps    *listenerDeps
	handler http.Handler
	// open is the same routes behind an apiAuth given no broker setting at all, which is the
	// listener R9 is about: the loopback, no-bearer one a GCP VM and the clustered hub run.
	open   http.Handler
	broker *brokertokentest.StubBroker
	caller *brokertokentest.Caller
	logs   *bytes.Buffer
}

// newBrokerRig opens the stores on one bus, opens the spent-proof bucket through the very call
// main makes, and mounts /v1 behind apiAuth with no bearer configured: an open listener, which
// is what a dev box and the dev stack run, so the session binding below is proved where it has
// to hold rather than behind a bearer that already keeps strangers out.
func newBrokerRig(t *testing.T) *brokerRig {
	t.Helper()
	client := setupPublishTestClient(t)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest and role registry: %v", err)
	}
	waitForRegistry(t, registry)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	ready, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.WaitForCacheReady(ready); err != nil {
		t.Fatalf("warm the session cache: %v", err)
	}
	deps := &listenerDeps{client: client, registry: registry, sessions: sessions}

	broker := brokertokentest.NewStubBroker(t)
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatalf("write the listener's own account token: %v", err)
	}
	tokens := newBrokerTokens(listenerBrokerConfig{
		BrokerURL:        broker.Server.URL,
		AccountTokenFile: accountFile,
		Origin:           listenerBrokerOrigin,
	})
	if err := tokens.open(client.Conn, 1); err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}

	logs := &bytes.Buffer{}
	logger := logging.NewWithWriter("test-machine", logs)
	routes := http.NewServeMux()
	registerV1Routes(routes, deps, "test-machine", logger)
	return &brokerRig{
		deps:    deps,
		handler: apiAuth("", nil, tokens, nil, logger, routes),
		open:    apiAuth("", nil, nil, nil, logger, routes),
		broker:  broker,
		caller:  brokertokentest.NewCaller(t),
		logs:    logs,
	}
}

// register puts a live session in the registry, as a plugin's own registration does, so the
// routes that read one find it.
func (r *brokerRig) register(t *testing.T, sessionID string) {
	t.Helper()
	if err := r.deps.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
}

// send drives one POST through the whole stack: the credential, then the route.
func (r *brokerRig) send(t *testing.T, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return r.call(t, r.handler, http.MethodPost, token, path, body)
}

// call is send for the routes that are not POSTs and for the listener that takes no broker
// token: one request of any method, through the handler named.
func (r *brokerRig) call(t *testing.T, handler http.Handler, method, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, brokerTokenRequest(t, r.caller, method, path, token, body))
	return recorder
}

// The whole of R6 on a real bus: a request presenting a broker access token acts as the session
// that token names and as no other, on each of the eight routes that act as a session; a revoked
// token reaches no route at all; a token naming a service and no person is admitted as any other
// session's is; and a request presenting no token is served exactly as it was before this
// credential existed. R9 is here too: the same routes behind a listener given no broker setting
// serve that very presentation and read no credential out of it (rig.open).
func TestBrokerTokenBindsEveryWriteRouteToTheSessionItNames(t *testing.T) {
	rig := newBrokerRig(t)
	const (
		bound    = "ses_rig_bound"
		other    = "ses_rig_other"
		target   = "ses_rig_target"
		service  = "ses_rig_service"
		token    = "access-token-of-the-bound-session"
		revoked  = "access-token-revoked"
		machines = "access-token-of-a-machine"
	)
	rig.broker.Says(token, sessionAnswer(bound, "ada@example.com", rig.caller.JKT()))
	rig.broker.Says(revoked, brokertoken.Introspection{Active: false})
	rig.broker.Says(machines, serviceAnswer(service, "example-fleet", rig.caller.JKT()))
	rig.register(t, bound)
	rig.register(t, service)
	rig.register(t, target)
	// The session this token does not name is registered too: the read routes below answer about
	// it, which is the point of not narrowing them.
	rig.register(t, other)

	// Each route as the session it names: the body's session is the field that route reads, and
	// the request's own credential names the same session.
	routes := []struct {
		name string
		path string
		// body takes the session the request acts as, so one line drives the bound case and the
		// impersonating one.
		body func(session string) string
	}{
		{name: "subscribe", path: "/v1/interests/subscribe", body: func(s string) string {
			return `{"session_id":"` + s + `","topics":["notifications.github.rig"]}`
		}},
		{name: "unsubscribe", path: "/v1/interests/unsubscribe", body: func(s string) string {
			return `{"session_id":"` + s + `","topics":["notifications.github.rig"]}`
		}},
		{name: "roles/set", path: "/v1/roles/set", body: func(s string) string {
			return `{"session_id":"` + s + `","role":"rig-role"}`
		}},
		{name: "messages/send", path: "/v1/messages/send", body: func(s string) string {
			return `{"target_session":"` + target + `","source_session":"` + s + `","message":"a message of the rig"}`
		}},
		{name: "messages/publish", path: "/v1/messages/publish", body: func(s string) string {
			return `{"topic":"notifications.github.rig","source_session":"` + s + `","message":"a message of the rig"}`
		}},
	}

	for _, route := range routes {
		t.Run(route.name+" acts as the session the token names", func(t *testing.T) {
			answer := rig.send(t, token, route.path, route.body(bound))
			if answer.Code != http.StatusOK {
				t.Fatalf("POST %s as %s: status = %d, want 200; body = %s", route.path, bound, answer.Code, answer.Body.String())
			}
		})
		t.Run(route.name+" refuses the session the token does not name", func(t *testing.T) {
			before := rig.logs.Len()
			answer := rig.send(t, token, route.path, route.body(other))
			if answer.Code != http.StatusForbidden {
				t.Fatalf("POST %s as %s: status = %d, want 403; body = %s", route.path, other, answer.Code, answer.Body.String())
			}
			if body := answer.Body.String(); !strings.Contains(body, bound) || !strings.Contains(body, other) {
				t.Fatalf("POST %s as %s: body = %s, want it to name both sessions", route.path, other, body)
			}
			logged := rig.logs.String()[before:]
			if !strings.Contains(logged, "listener: broker token acts as another session") {
				t.Fatalf("POST %s as %s logged no refusal line: %s", route.path, other, logged)
			}
			if strings.Contains(logged, token) {
				t.Fatalf("the refusal line repeats the access token: %s", logged)
			}
		})
	}

	t.Run("a token naming a service and no person acts as its own session", func(t *testing.T) {
		answer := rig.send(t, machines, "/v1/interests/subscribe", `{"session_id":"`+service+`","topics":[]}`)
		if answer.Code != http.StatusOK {
			t.Fatalf("a service-only token subscribing as %s: status = %d, want 200; body = %s", service, answer.Code, answer.Body.String())
		}
		refused := rig.send(t, machines, "/v1/interests/subscribe", `{"session_id":"`+other+`","topics":[]}`)
		if refused.Code != http.StatusForbidden {
			t.Fatalf("a service-only token subscribing as %s: status = %d, want 403; body = %s", other, refused.Code, refused.Body.String())
		}
	})

	t.Run("a revoked token reaches no route", func(t *testing.T) {
		answer := rig.send(t, revoked, "/v1/interests/subscribe", `{"session_id":"`+bound+`","topics":[]}`)
		if answer.Code != http.StatusUnauthorized {
			t.Fatalf("a revoked token: status = %d, want 401; body = %s", answer.Code, answer.Body.String())
		}
		if _, err := rig.deps.registry.Get(context.Background(), bound); err != nil {
			t.Fatalf("read %s's interests after the refusal: %v", bound, err)
		}
	})

	t.Run("a request presenting no token keeps the open listener's rule", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
			strings.NewReader(`{"session_id":"`+other+`","topics":[]}`))
		request.Host = "127.0.0.1:9020"
		request.Header.Set("Content-Type", "application/json")
		answer := httptest.NewRecorder()
		rig.handler.ServeHTTP(answer, request)
		if answer.Code != http.StatusOK {
			t.Fatalf("an unauthenticated subscribe naming any session: status = %d, want 200; body = %s", answer.Code, answer.Body.String())
		}
	})

	t.Run("the read routes that name another session by design are not narrowed", func(t *testing.T) {
		for _, path := range []string{"/v1/registry/" + other, "/v1/interests/" + other, "/v1/sessions"} {
			signed := rig.caller.Signed(t, http.MethodGet, listenerBrokerOrigin+path, token)
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Host = "127.0.0.1:9020"
			request.Header.Set("Authorization", signed.Header.Get("Authorization"))
			request.Header.Set("DPoP", signed.Header.Get("DPoP"))
			answer := httptest.NewRecorder()
			rig.handler.ServeHTTP(answer, request)
			if answer.Code != http.StatusOK {
				t.Fatalf("GET %s under a token naming %s: status = %d, want 200; body = %s", path, bound, answer.Code, answer.Body.String())
			}
		}
	})

	// The three routes that act on one session without naming it in a message body: the replay of
	// a session's inbox, and the two deletes, whose session is the path. Each is driven as the
	// session the token names and then as a session it does not.
	t.Run("replay reads only the session the token names", func(t *testing.T) {
		own := rig.send(t, token, "/v1/replay",
			`{"session_id":"`+bound+`","subjects":[{"subject":"notifications.agent.`+bound+`"}]}`)
		if own.Code != http.StatusOK {
			t.Fatalf("replaying %s's own inbox: status = %d, want 200; body = %s", bound, own.Code, own.Body.String())
		}
		refused := rig.send(t, token, "/v1/replay",
			`{"session_id":"`+other+`","subjects":[{"subject":"notifications.agent.`+other+`"}]}`)
		if refused.Code != http.StatusForbidden {
			t.Fatalf("replaying %s's inbox under a token naming %s: status = %d, want 403; body = %s",
				other, bound, refused.Code, refused.Body.String())
		}
	})

	t.Run("a session's interests are removed only under the token that names it", func(t *testing.T) {
		refused := rig.call(t, rig.handler, http.MethodDelete, token, "/v1/interests/"+other, "")
		if refused.Code != http.StatusForbidden {
			t.Fatalf("DELETE /v1/interests/%s under a token naming %s: status = %d, want 403; body = %s",
				other, bound, refused.Code, refused.Body.String())
		}
		if _, err := rig.deps.registry.Get(context.Background(), other); err != nil {
			t.Fatalf("read %s's interests after the refusal: %v", other, err)
		}
		own := rig.call(t, rig.handler, http.MethodDelete, token, "/v1/interests/"+bound, "")
		if own.Code != http.StatusNoContent {
			t.Fatalf("DELETE /v1/interests/%s under its own token: status = %d, want 204; body = %s",
				bound, own.Code, own.Body.String())
		}
	})

	t.Run("a session registration is removed only under the token that names it", func(t *testing.T) {
		refused := rig.call(t, rig.handler, http.MethodDelete, token, "/v1/sessions/"+other, "")
		if refused.Code != http.StatusForbidden {
			t.Fatalf("DELETE /v1/sessions/%s under a token naming %s: status = %d, want 403; body = %s",
				other, bound, refused.Code, refused.Body.String())
		}
		if _, err := rig.deps.sessions.Refresh(context.Background(), other); err != nil {
			t.Fatalf("%s's registration after the refusal: %v", other, err)
		}
		// The Claude Code /clear handoff's own shape: the client instance id of the process
		// leaving, beside the session its token still names, since it deregisters the session it
		// hands off from before it adopts the new id.
		own := rig.call(t, rig.handler, http.MethodDelete, token,
			"/v1/sessions/"+bound+"?client_instance_id=4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a", "")
		if own.Code != http.StatusOK {
			t.Fatalf("DELETE /v1/sessions/%s under its own token: status = %d, want 200; body = %s",
				bound, own.Code, own.Body.String())
		}
		if !strings.Contains(own.Body.String(), `"deleted":true`) {
			t.Fatalf("DELETE /v1/sessions/%s answered %s, want the registration gone", bound, own.Body.String())
		}
	})

	t.Run("a listener with no broker setting reads no credential out of the same request", func(t *testing.T) {
		// R9: with none of the broker variables set, the request the listener above holds to one
		// session is served by this one as an unauthenticated request was before this credential
		// existed — it names any session it likes — and nothing of it reaches the broker. A
		// client presents a broker token to whatever listener it is pointed at, so refusing it
		// here would lock every such client out of the listeners that take no broker token.
		asked := rig.broker.Count()
		answer := rig.call(t, rig.open, http.MethodPost, token, "/v1/interests/subscribe",
			`{"session_id":"`+other+`","topics":[]}`)
		if answer.Code != http.StatusOK {
			t.Fatalf("subscribing as %s under a DPoP presentation to a listener that takes no broker token: status = %d, want 200; body = %s",
				other, answer.Code, answer.Body.String())
		}
		if asked != rig.broker.Count() {
			t.Fatalf("the listener that takes no broker token made %d introspections, want none", rig.broker.Count()-asked)
		}
	})
}

// The spent-proof bucket is the record two listeners of one deployment share: the first create of
// a key succeeds and every later one reports the proof spent, which is what refuses a captured
// proof presented to the replica that never saw it. Two handles on one bucket stand in for the
// two listeners.
func TestSpentProofsRefuseAProofTheBucketAlreadyHolds(t *testing.T) {
	client := setupPublishTestClient(t)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	open := func() spentProofs {
		kv, err := bus.EnsureKeyValue(js, &natsgo.KeyValueConfig{
			Bucket: bus.DPoPProofBucket, TTL: spentProofRetention, Storage: natsgo.FileStorage, Replicas: 1,
		})
		if err != nil {
			t.Fatalf("open %s: %v", bus.DPoPProofBucket, err)
		}
		return spentProofs{kv: kv}
	}
	first, second := open(), open()
	ctx := context.Background()
	expiry := time.Now().Add(time.Minute)

	used, err := first.CheckAndSetDPoPProofUsed(ctx, "jti-rig-1", "jkt", "", "POST", "http://127.0.0.1:9020/v1/messages/send", expiry)
	if err != nil || used {
		t.Fatalf("the first sight of a proof = (%t, %v), want (false, nil)", used, err)
	}
	used, err = second.CheckAndSetDPoPProofUsed(ctx, "jti-rig-1", "jkt", "", "POST", "http://127.0.0.1:9020/v1/messages/send", expiry)
	if err != nil || !used {
		t.Fatalf("the same proof at the other listener = (%t, %v), want (true, nil)", used, err)
	}
	// A different target URI is a different proof, as it is in Dispatch's own store: the key is
	// the 'jti' and the URI together.
	used, err = second.CheckAndSetDPoPProofUsed(ctx, "jti-rig-1", "jkt", "", "POST", "http://127.0.0.1:9020/v1/messages/publish", expiry)
	if err != nil || used {
		t.Fatalf("the same jti on another target URI = (%t, %v), want (false, nil)", used, err)
	}
	// The bucket keeps a spent proof for its whole window and no longer, so a listener that has
	// been up for days holds only the last few minutes of them.
	status, err := first.kv.Status()
	if err != nil {
		t.Fatalf("read the bucket's status: %v", err)
	}
	if status.TTL() != spentProofRetention {
		t.Fatalf("the bucket's TTL = %s, want %s", status.TTL(), spentProofRetention)
	}
}

// A listener configured to take broker access tokens on a bus with no JetStream refuses to start,
// naming the setting that asked for it. The alternative — a spent-proof store in this process's
// memory — would let a proof captured from one replica be presented to another, so there is no
// fallback to refuse into.
func TestBrokerTokensRefuseABusWithNoJetStream(t *testing.T) {
	// A server of this test's own with no -js, connected to directly: testnats.Connect waits for
	// JetStream, which this one will never have.
	_, uri := testnats.StartConfig(t, "", nil, true)
	conn, err := natsgo.Connect(uri)
	if err != nil {
		t.Fatalf("connect to a bus with no JetStream: %v", err)
	}
	t.Cleanup(conn.Close)

	tokens := newBrokerTokens(listenerBrokerConfig{
		BrokerURL:        "https://broker.example",
		AccountTokenFile: "/var/run/secrets/broker/token",
		Origin:           listenerBrokerOrigin,
	})
	err = tokens.open(conn, 1)
	if err == nil {
		t.Fatal("a listener took broker tokens on a bus with no JetStream")
	}
	if !strings.Contains(err.Error(), EnvBrokerURL) || !strings.Contains(err.Error(), bus.DPoPProofBucket) {
		t.Fatalf("the refusal = %q, want it to name %s and %s", err, EnvBrokerURL, bus.DPoPProofBucket)
	}
	if tokens.verifier.Load() != nil {
		t.Fatal("the credential is live after its bucket could not be opened")
	}
	// Nothing of it serves either: a presentation reaching a listener in that state is the
	// starting 503 and never an admission.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	request.Host = "127.0.0.1:9020"
	request.Header.Set("Authorization", "DPoP some-access-token")
	apiAuth("", nil, tokens, nil, logging.NewWithWriter("test-machine", &bytes.Buffer{}),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).
		ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}
