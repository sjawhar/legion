package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// endableIdentity names one person until their session ends.
type endableIdentity struct{ ended atomic.Bool }

func (i *endableIdentity) Login(*http.Request) (string, error) {
	if i.ended.Load() {
		return "", identity.ErrNoIdentity
	}
	return "alice@d.example", nil
}

// A person whose session ends (logout, a refresh the sign-in pool refuses, a lost group) stops
// receiving the event stream they opened while signed in: each heartbeat resolves them again.
func TestEventStreamClosesOnceItsPersonNoLongerResolves(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	person := &endableIdentity{}
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: person, AgentToken: "agent-token", Docs: documents, Events: broker,
		StreamHeartbeat: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("event stream status %d, want 200", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("first heartbeat: %q %v", line, err)
	}

	person.ended.Store(true)
	closed := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		closed <- err
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("stream ended with %v, want a clean close", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the event stream stayed open after its person stopped resolving")
	}
}

// The same holds for a human watching an agent's live conversation: the relay resolves the viewer
// on each heartbeat and stops relaying once their session has ended.
func TestAgentStreamClosesOnceItsViewerNoLongerResolves(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	person := &endableIdentity{}
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: person, AgentToken: "agent-token", Docs: documents, Events: broker,
		AgentStream: agentstream.NewMemory(), StreamHeartbeat: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/api/v1/agents/" + plannerSessionID + "/stream")
	if err != nil {
		t.Fatalf("open agent stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("agent stream status %d, want 200", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("first line of the stream: %q %v", line, err)
	}

	person.ended.Store(true)
	closed := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		closed <- err
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("stream ended with %v, want a clean close", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent stream stayed open after its viewer stopped resolving")
	}
}

// A person whose session ends while their editor holds a document open loses the live document
// too: the document socket resolves them again on every heartbeat, as the event streams do, and
// closes once they no longer resolve, so the editor neither receives the document's edits nor
// sends its own. Each case ends the session the way it ends in production: the hourly membership
// check finding the person out of the group, and a sign-out.
func TestDocumentSocketClosesOnceItsPersonNoLongerResolves(t *testing.T) {
	const heartbeat = 250 * time.Millisecond
	for name, end := range map[string]func(*testing.T, *documentSocketRig){
		"the sign-in pool drops them from the group": func(t *testing.T, rig *documentSocketRig) {
			// The pool drops the group before the confirmation lapses, so the first refresh after
			// the hour is the one that finds the person outside it.
			rig.pool.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"other-group"} })
			membership, found, err := rig.people.Membership(context.Background(), rig.email)
			if err != nil || !found {
				t.Fatalf("the member's membership: found %t, %v", found, err)
			}
			if err := rig.people.Confirm(context.Background(), rig.email, membership.RefreshToken, time.Now().Add(-2*time.Hour)); err != nil {
				t.Fatalf("date the confirmation two hours back: %v", err)
			}
		},
		"they sign out": func(t *testing.T, rig *documentSocketRig) {
			if err := rig.sessions.RevokeSessions(context.Background(), rig.email); err != nil {
				t.Fatalf("sign out: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDocumentSocketRig(t, heartbeat)
			peer := &syncedPeer{
				wsURL: "ws" + strings.TrimPrefix(rig.server.URL, "http") + "/ws/doc/" + rig.artifactID, sockets: rig.sockets,
				headers: http.Header{"Cookie": []string{rig.cookie.Name + "=" + rig.cookie.Value}}, artifactID: rig.artifactID, doc: crdt.New(),
			}
			peer.connect(t)
			t.Cleanup(peer.close)

			// While they are a member the socket outlives several heartbeats, each resolving them
			// again, and stays writable: their edit reaches the room.
			time.Sleep(4 * heartbeat)
			peer.appendParagraph(t, "typed while a member")
			peer.barrier(t)
			if refreshes := rig.pool.Refreshes(); refreshes != 0 {
				t.Fatalf("the sign-in pool was asked %d times within the hour, want 0", refreshes)
			}

			end(t, rig)
			ended := time.Now()
			select {
			case <-peer.connection().Ended:
				t.Logf("the document socket closed %s after the session ended (heartbeat %s)", time.Since(ended).Round(time.Millisecond), heartbeat)
			case <-time.After(heartbeat + time.Second):
				t.Fatalf("the document socket stayed open %s after its person's session ended", time.Since(ended).Round(time.Millisecond))
			}
			if generation, _, err := rig.sessions.CurrentSessionGeneration(context.Background(), rig.email); err != nil || generation != rig.generation+1 {
				t.Fatalf("session generation after the session ended = %d (%v), want %d", generation, err, rig.generation+1)
			}

			// The document goes on without them: an edit after the close reaches the room and not
			// their editor, and their editor cannot reconnect.
			if _, err := rig.documents.ReplaceText(context.Background(), rig.artifactID, "written after the session ended", model.Actor{Kind: "user", ID: "bob@d.example"}); err != nil {
				t.Fatalf("edit the document after the close: %v", err)
			}
			if text, err := rig.documents.Text(context.Background(), rig.artifactID); err != nil || !strings.Contains(text, "written after the session ended") {
				t.Fatalf("room text after the edit = %q (%v)", text, err)
			}
			peer.assertTextLacks(t, "written after the session ended")
			connection, response, err := gws.DefaultDialer.Dial(peer.wsURL, peer.headers)
			if connection != nil {
				_ = connection.Close()
			}
			if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("reconnect after the session ended: response=%#v err=%v, want a 401 handshake", response, err)
			}
		})
	}
}

// documentSocketRig is the server main serves a person signed in through a sign-in pool: cookie
// identity confirming membership with the pool, and the API and document socket Register mounts.
type documentSocketRig struct {
	pool       *oidctest.Issuer
	people     *store.PgPeopleStore
	sessions   *store.PgSessionStore
	documents  *docs.Service
	server     *httptest.Server
	sockets    *servedSockets
	email      string
	generation int64
	cookie     *http.Cookie
	artifactID string
}

// newDocumentSocketRig signs alice@d.example in through a fake pool as a member of
// dispatch-members, as the sign-in callback does, and gives her an issue whose spec she can open.
func newDocumentSocketRig(t *testing.T, heartbeat time.Duration) *documentSocketRig {
	t.Helper()
	ctx := context.Background()
	rig := &documentSocketRig{pool: oidctest.New(t), email: "alice@d.example"}
	rig.pool.EnableCodeFlow(rig.pool.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret")
	flow, err := oidc.NewCodeFlow(ctx, rig.pool.URL(), "dispatch-client", "dispatch-secret")
	if err != nil {
		t.Fatalf("discover the sign-in pool: %v", err)
	}
	const redirect = "https://dispatch.example/auth/callback"
	rig.pool.SignInAs(map[string]any{
		"sub":              "person-subject",
		"cognito:username": "ExampleIdP_" + rig.email,
		"cognito:groups":   []string{"dispatch-members"},
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	})
	code, _ := rig.pool.Authorize(t, flow.AuthURL(redirect, "state", "nonce"))
	signIn, err := flow.Exchange(ctx, redirect, code, "nonce")
	if err != nil {
		t.Fatalf("exchange the sign-in code: %v", err)
	}

	database := storetest.Open(t)
	rig.people = store.NewPgPeopleStore(database.Pool, "signing-key", nil)
	rig.sessions = store.NewPgSessionStore(database.Pool)
	if err := rig.people.SignIn(ctx, rig.email, signIn.RefreshToken, time.Now()); err != nil {
		t.Fatalf("record the sign-in: %v", err)
	}
	if rig.generation, err = rig.sessions.EnsureSession(ctx, rig.email); err != nil {
		t.Fatalf("establish the session: %v", err)
	}
	if rig.cookie, err = http.ParseSetCookie(auth.IssueSessionCookie(rig.email, rig.generation, "signing-key", false)); err != nil {
		t.Fatalf("parse the session cookie: %v", err)
	}
	person := identity.CookieIdentity{
		SigningKey: "signing-key",
		Sessions:   rig.sessions,
		Membership: &identity.Membership{People: rig.people, Sessions: rig.sessions, SignIn: flow, Group: "dispatch-members"},
	}
	broker := events.NewBroker()
	rig.documents = docs.New(docs.Deps{Store: database, Events: broker, Identity: person, Settle: time.Hour})
	t.Cleanup(func() { _ = rig.documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: person, AgentToken: "agent-token", Docs: rig.documents, Events: broker,
		StreamHeartbeat: heartbeat,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	rig.sockets = &servedSockets{finished: make(map[string]chan struct{})}
	rig.server = httptest.NewServer(rig.sockets.serve(mux.ServeHTTP))
	t.Cleanup(rig.server.Close)

	signedIn := func(method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
		request.AddCookie(rig.cookie)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response
	}
	if response := signedIn(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := signedIn(http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Live document", "spec": "The spec."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	rig.artifactID = decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created).PrimaryArtifactID
	return rig
}
