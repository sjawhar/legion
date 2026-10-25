package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dedupe"
	dispatchapi "github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	dispatchstore "github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testpg"
)

// This file is the one proof that spans both services. Every other test of INFRA-249 exercises
// one of them: the listener's route against its own stores, or Dispatch against a stand-in
// listener. Here the real listener mux and the real Dispatch mux run together over one real NATS
// and one real Postgres, so "a mention reaches a session the listener's cache has not refilled"
// is observed end to end — at the agent's own endpoint — rather than inferred from two halves.
//
// It lives in package main because the listener's HTTP mux is built here (registerV1Routes,
// api.go) and no package outside cmd/listener can build it. So this file imports Dispatch, not
// the other way round.

const (
	// rigMachineID is the machine both listeners and every session in this file run on, as the
	// rest of the package's tests spell it. The deliverer pushes to a local session through its
	// host bridge, so a session entry must carry this id to be pushed to at 127.0.0.1.
	rigMachineID = "test-machine"
	// rigHuman is the person who writes the comments and sends the broadcasts here: a Dispatch
	// write carries a human's login, and this one is recorded as having signed in.
	rigHuman = "alice"
	// rigUserHeader is the header the test identity reads that login from, and rigAgentToken the
	// shared bearer a session's own write carries (DISPATCH_AGENT_TOKEN).
	rigUserHeader = "X-Dispatch-User"
	rigAgentToken = "agent-token"
	// rigDeliveryMode is the targeted delivery mode every mention and broadcast here asks for,
	// and one every session in this file advertises: resolution, not capability, is under test.
	rigDeliveryMode = "steer"
)

// dispatchRig is the pair of services one test here drives.
type dispatchRig struct {
	// warm is the listener the sessions register through. Put is write-through, so its cache
	// holds every session this rig registers, and its delivery loop is the one that pushes
	// their frames — the listener process the agents are talking to.
	warm *listenerDeps
	// cold is the listener Dispatch asks. Its session cache stopped following the bucket before
	// any of these sessions registered (trailingListeners), which is the state a listener is in
	// while its cache refills after the session bucket was recreated, and the state it is in for
	// the moment after another listener registered a session. Asking it is therefore asking with
	// a cold cache while the record is in the bucket.
	cold *listenerDeps
	// dispatch is the real Dispatch /api/v1 mux, its EnvoyURL the cold listener's.
	dispatch http.Handler
	database *dispatchstore.Store
}

// newDispatchRig builds both services: one NATS, two listeners over it, the cold one served over
// HTTP, one migrated Postgres with the person who writes, and Dispatch pointed at the cold
// listener.
func newDispatchRig(t *testing.T) *dispatchRig {
	t.Helper()
	warm, cold := trailingListeners(t)
	startListenerDelivery(t, warm)

	mux := http.NewServeMux()
	registerV1Routes(mux, cold, rigMachineID, listenerRigLogger(t, "cold listener"))
	listener := httptest.NewServer(mux)
	t.Cleanup(listener.Close)

	rig := &dispatchRig{warm: warm, cold: cold, database: migratedDispatchDatabase(t)}
	rig.dispatch = dispatchMux(t, rig.database, listener.URL)
	return rig
}

// listenerRigLogger writes the listener's own log lines into the test's output, but only when the
// test fails: a passing cross-service test would otherwise print a delivery log nobody reads.
func listenerRigLogger(t *testing.T, name string) *logging.Logger {
	t.Helper()
	lines := &cmdtest.LockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s log:\n%s", name, lines.String())
		}
	})
	return logging.NewWithWriter("test", lines)
}

// startListenerDelivery runs the listener's own delivery loop over d: the durable JetStream
// subscription main binds, with a real HTTP deliverer. A frame Dispatch publishes through
// /v1/messages/send is then pushed to the session's endpoint exactly as the listener pushes it,
// so a delivery is observed where the agent would receive it rather than in a log line.
func startListenerDelivery(t *testing.T, d *listenerDeps) {
	t.Helper()
	dedupeCache := dedupe.New(10 * time.Minute)
	attemptCache := dedupe.New(5 * time.Minute)
	t.Cleanup(dedupeCache.Stop)
	t.Cleanup(attemptCache.Stop)
	met := metrics.New()
	config := listenerDeliveryHandlerConfig{
		client:      d.client,
		forwardRole: d.client.RequestCoreTo,
		registry:    d.registry,
		sessions:    d.sessions,
		machineID:   rigMachineID,
		deliverer: &session.Deliverer{
			MachineID:    rigMachineID,
			HostBridge:   "127.0.0.1",
			RequestLimit: 5 * time.Second,
			// No Sessions lookup: the delivery config above resolves the entry from its own
			// sessions field and hands it to DeliverWithResultForEntry, so the deliverer holds
			// no registry of its own (INFRA-141).
		},
		dedupeCache:       dedupeCache,
		attemptCache:      attemptCache,
		logger:            listenerRigLogger(t, "delivery listener"),
		messagesReceived:  met.NewCounter("rig_messages_received", "test"),
		messagesDelivered: met.NewCounter("rig_messages_delivered", "test"),
		messagesNAKed:     met.NewCounter("rig_messages_naked", "test"),
		deliveryDuration:  met.NewHistogram("rig_delivery_duration", "test", metrics.DefaultBuckets),
		// One deferral set per handler, as main.go builds it: an agent delivery this rig cannot
		// place is held here and retried rather than acknowledged away (INFRA-141).
		deferredUnplaceable: newUnplaceableDeferrals(),
	}
	subscription, err := startListenerSubscription(
		d.client, "listener-dispatch-"+id.New(), jetStreamDeliveryHandler(config))
	if err != nil {
		t.Fatalf("bind the listener's durable consumer: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
}

// migratedDispatchDatabase opens an empty Postgres database of this test's own, carries every
// Dispatch migration into it, and records the person whose writes the tests make.
func migratedDispatchDatabase(t *testing.T) *dispatchstore.Store {
	t.Helper()
	ctx := context.Background()
	database, err := dispatchstore.Open(ctx, testpg.URL(t))
	if err != nil {
		t.Fatalf("open the Dispatch database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate the Dispatch database: %v", err)
	}
	people := dispatchstore.NewPgPeopleStore(database.Pool, "signing-key", nil)
	if err := people.Record(ctx, rigHuman); err != nil {
		t.Fatalf("record %s as having signed in: %v", rigHuman, err)
	}
	return database
}

// dispatchMux is the real Dispatch API, built the way cmd/dispatch builds it, with the listener
// under test as its ENVOY_URL.
func dispatchMux(t *testing.T, database *dispatchstore.Store, envoyURL string) http.Handler {
	t.Helper()
	tokens, err := auth.ParseSharedAgentTokens(rigAgentToken)
	if err != nil {
		t.Fatalf("parse the shared agent token: %v", err)
	}
	people := dispatchstore.NewPgPeopleStore(database.Pool, "signing-key", nil)
	broker := events.NewBroker()
	caller := identity.HeaderIdentity{Header: rigUserHeader, People: people}
	// The document service is built here rather than left to NewDeps so this test can shut it
	// down: an issue's spec schedules a settlement, and a service nobody ended would still be
	// warming that document after the database pool had closed.
	documents := docs.New(docs.Deps{
		Store:     database,
		Events:    broker,
		Identity:  caller,
		ServerURL: "https://dispatch.example",
		Settle:    20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	deps, err := dispatchapi.NewDeps(dispatchapi.DepsInput{
		Store:       database,
		Identity:    caller,
		AgentTokens: tokens,
		ServerURL:   "https://dispatch.example",
		EnvoyURL:    envoyURL,
		Docs:        documents,
		Events:      broker,
		Lifetime:    t.Context(),
	})
	if err != nil {
		t.Fatalf("new Dispatch API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	// The wait returns once Lifetime is cancelled, which t.Context() is just before the
	// cleanups run, and it must finish before the database pool closes — the cleanup registered
	// first, so the one registered here runs first.
	t.Cleanup(dispatchapi.Register(mux, deps))
	return mux
}

// rigSession is a registered session with an endpoint of its own: the httptest server standing in
// for the agent's serve process, which records every frame the listener pushes to it.
type rigSession struct {
	id     string
	title  string
	frames chan string
}

// registerSession registers sessionID against the warm listener, with a push endpoint of its own,
// and checks that the listener Dispatch asks has not seen it: a rig whose cold cache already held
// the session would prove nothing.
func (rig *dispatchRig) registerSession(t *testing.T, sessionID, title string) *rigSession {
	t.Helper()
	live := &rigSession{id: sessionID, title: title, frames: make(chan string, 8)}
	path := "/session/" + sessionID + "/prompt_async"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			t.Errorf("%s was pushed %s %s, want POST %s", sessionID, r.Method, r.URL.Path, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode the frame pushed to %s: %v", sessionID, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var frame strings.Builder
		for _, part := range body.Parts {
			frame.WriteString(part.Text)
		}
		live.frames <- frame.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(endpoint.Close)

	if err := rig.warm.sessions.Put(sessionID, session.SessionEntry{
		Port:         endpointPort(t, endpoint.URL),
		MachineID:    rigMachineID,
		Host:         "127.0.0.1",
		Dir:          "/test/" + sessionID,
		Title:        title,
		Capabilities: []string{"aside", "btw", "steer"},
	}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	if _, err := rig.cold.sessions.Get(sessionID); err == nil {
		t.Fatalf("the listener Dispatch asks already holds %s, so its read-through is not under test",
			sessionID)
	}
	return live
}

// endSession removes a session from the bucket, as its plugin does when the agent stops. Nothing
// is left for any listener to read.
func (rig *dispatchRig) endSession(t *testing.T, sessionID string) {
	t.Helper()
	if err := rig.warm.sessions.Delete(sessionID); err != nil {
		t.Fatalf("end %s: %v", sessionID, err)
	}
}

func endpointPort(t *testing.T, raw string) int {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse the session endpoint URL %q: %v", raw, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("read the session endpoint's port from %q: %v", raw, err)
	}
	return port
}

// awaitFrame waits for the next frame the listener pushed to this session's endpoint.
func (s *rigSession) awaitFrame(t *testing.T) string {
	t.Helper()
	select {
	case frame := <-s.frames:
		return frame
	case <-time.After(15 * time.Second):
		t.Fatalf("no frame reached %s's own endpoint", s.id)
		return ""
	}
}

// noFrame reports that nothing reached this session's endpoint in the window a frame would have.
func (s *rigSession) noFrame(t *testing.T) {
	t.Helper()
	select {
	case frame := <-s.frames:
		t.Fatalf("%s received %q, want nothing", s.id, frame)
	case <-time.After(500 * time.Millisecond):
	}
}

// asHuman makes one Dispatch request as the person who signed in: a comment and a broadcast carry
// a human's identity.
func (rig *dispatchRig) asHuman(
	t *testing.T, method, target string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	return rig.request(t, method, target, body, rigUserHeader, rigHuman)
}

// asSession makes one Dispatch request as an agent bearer, the identity `dispatch claim` carries;
// the session it acts as is named in the body's actor.
func (rig *dispatchRig) asSession(
	t *testing.T, method, target string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	return rig.request(t, method, target, body, "Authorization", "Bearer "+rigAgentToken)
}

func (rig *dispatchRig) request(
	t *testing.T, method, target string, body any, header, value string,
) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the %s %s body: %v", method, target, err)
		}
		data = encoded
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(header, value)
	recorder := httptest.NewRecorder()
	rig.dispatch.ServeHTTP(recorder, request)
	return recorder
}

// issueKey creates the project, once, and an issue to hang a comment or a claim on.
func (rig *dispatchRig) issueKey(t *testing.T, title string) string {
	t.Helper()
	project := rig.asHuman(t, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	})
	if project.Code != http.StatusCreated && project.Code != http.StatusConflict {
		t.Fatalf("create the project: status=%d body=%s", project.Code, project.Body.String())
	}
	created := rig.asHuman(t, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": title,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeRigBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

// broadcastRows counts the broadcasts the database holds, so a test can say that a refused send
// wrote nothing.
func (rig *dispatchRig) broadcastRows(t *testing.T) int {
	t.Helper()
	var rows int
	count := `select count(*) from broadcasts`
	if err := rig.database.Pool.QueryRow(context.Background(), count).Scan(&rows); err != nil {
		t.Fatalf("count the broadcasts: %v", err)
	}
	return rows
}

func decodeRigBody[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return value
}

// rigDelivery is one mention's attempt row, as the comment create answers it.
type rigDelivery struct {
	Target    string  `json:"target"`
	Delivery  string  `json:"delivery"`
	SessionID *string `json:"session_id"`
	State     string  `json:"state"`
	Error     *string `json:"error"`
}

type rigComment struct {
	Deliveries []rigDelivery `json:"deliveries"`
}

type rigBroadcast struct {
	ID         string `json:"id"`
	Recipients []struct {
		SessionID string `json:"session_id"`
		Message   struct {
			Deliveries []rigDelivery `json:"deliveries"`
		} `json:"message"`
	} `json:"recipients"`
	Excluded []struct {
		SessionID string `json:"session_id"`
		Reason    string `json:"reason"`
	} `json:"excluded"`
}

// awaitBroadcastDeliveries reads the broadcast back until every recipient's attempt has settled.
// A send answers as soon as its messages are committed and delivers behind the request, on the
// server's lifetime, so a test that returned with a delivery still in flight would have that
// work cancelled by its own t.Context() rather than finished.
func (rig *dispatchRig) awaitBroadcastDeliveries(t *testing.T, broadcastID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		read := rig.asHuman(t, http.MethodGet, "/api/v1/broadcasts/"+broadcastID, nil)
		if read.Code != http.StatusOK {
			t.Fatalf("read the broadcast: status=%d body=%s", read.Code, read.Body.String())
		}
		stored := decodeRigBody[rigBroadcast](t, read)
		settled := len(stored.Recipients) > 0
		for _, recipient := range stored.Recipients {
			if len(recipient.Message.Deliveries) == 0 ||
				recipient.Message.Deliveries[0].State == "pending" {
				settled = false
			}
		}
		if settled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recipients still without a settled attempt: %#v", stored.Recipients)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type rigFailure struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// mention posts one comment mentioning sessionID and returns what the create answered.
func (rig *dispatchRig) mention(t *testing.T, issueKey, sessionID, body string) rigComment {
	t.Helper()
	created := rig.asHuman(t, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
		"body":     body,
		"delivery": rigDeliveryMode,
		"mentions": []map[string]any{{"target": "session:" + sessionID}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("comment mentioning %s: status=%d body=%s",
			sessionID, created.Code, created.Body.String())
	}
	comment := decodeRigBody[rigComment](t, created)
	if len(comment.Deliveries) != 1 {
		t.Fatalf("the comment's deliveries = %#v, want the one mention's attempt", comment.Deliveries)
	}
	return comment
}

// A person mentions a session the listener Dispatch asks has not refilled into its cache. Before
// this change Dispatch answered "is this session live?" by fetching every live session (GET
// /v1/sessions), which is that one listener process's in-memory cache: the working session was
// simply absent from the list, so the mention was refused "no live session ses_planner", the
// frame was never published, and the attempt row recorded that failure. Now the question is asked
// about the one session by id, against a route that reads the session store, and the frame
// arrives at the agent's own endpoint.
func TestDispatchMentionResolvesASessionTheCacheHasNotRefilled(t *testing.T) {
	rig := newDispatchRig(t)
	planner := rig.registerSession(t, "ses_planner", "Planner")
	key := rig.issueKey(t, "A mention while the cache refills")

	comment := rig.mention(t, key, planner.id, "Please take this one.")
	attempt := comment.Deliveries[0]
	if attempt.State != "sent" || attempt.Error != nil ||
		attempt.SessionID == nil || *attempt.SessionID != planner.id {
		t.Fatalf("the mention's attempt = %#v, want it sent to %s with no failure", attempt, planner.id)
	}
	if frame := planner.awaitFrame(t); !strings.Contains(frame, "Please take this one.") {
		t.Fatalf("the frame %s received = %q, want the comment's body", planner.id, frame)
	}
}

// A human broadcasts to two sessions the listener Dispatch asks has not refilled into its cache.
// Before this change the send judged the whole selection against that listener's live list, so
// both were excluded "no live session" and the message the human composed for them was dropped.
// Now each selected session is read by id against the session store, so both are recipients and
// both frames arrive.
func TestDispatchBroadcastResolvesSessionsTheCacheHasNotRefilled(t *testing.T) {
	rig := newDispatchRig(t)
	planner := rig.registerSession(t, "ses_planner", "Planner")
	tester := rig.registerSession(t, "ses_tester", "Tester")

	response := rig.asHuman(t, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body":            "Stand down and report status.",
		"delivery":        rigDeliveryMode,
		"session_ids":     []string{planner.id, tester.id},
		"idempotency_key": "cross-service-broadcast",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("broadcast to two sessions the cache has not refilled: status=%d body=%s",
			response.Code, response.Body.String())
	}
	created := decodeRigBody[rigBroadcast](t, response)
	if len(created.Recipients) != 2 || len(created.Excluded) != 0 {
		t.Fatalf("the broadcast = %#v, want both sessions as recipients and nothing excluded", created)
	}
	recipients := map[string]bool{}
	for _, recipient := range created.Recipients {
		recipients[recipient.SessionID] = true
	}
	if !recipients[planner.id] || !recipients[tester.id] {
		t.Fatalf("recipients = %#v, want %s and %s", created.Recipients, planner.id, tester.id)
	}
	for _, live := range []*rigSession{planner, tester} {
		if frame := live.awaitFrame(t); !strings.Contains(frame, "Stand down and report status.") {
			t.Fatalf("the frame %s received = %q, want the broadcast's body", live.id, frame)
		}
	}
	rig.awaitBroadcastDeliveries(t, created.ID)
}

// A read of a session's record that never answers says nothing about whether that session is
// working, and the two routes that could act on it must say so rather than invent an absence.
// Before this change both asked for the live list: a listener that could not answer it at all
// left Dispatch with no rows, which is indistinguishable from every session having ended, so the
// broadcast would have been written with both recipients excluded "no live session" — frozen
// under its idempotency key, where no retry of the human's could ever reach them — and the
// mention's attempt would have recorded that same wording.
func TestDispatchRefusesABroadcastWhenASessionReadDoesNotAnswer(t *testing.T) {
	rig := newDispatchRig(t)
	planner := rig.registerSession(t, "ses_planner", "Planner")
	tester := rig.registerSession(t, "ses_tester", "Tester")
	key := rig.issueKey(t, "A mention while the bucket cannot be read")
	stallSessionReads(t, rig.cold.sessions)

	response := rig.asHuman(t, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body":            "Stand down and report status.",
		"delivery":        rigDeliveryMode,
		"session_ids":     []string{planner.id, tester.id},
		"idempotency_key": "cross-service-unanswered",
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("broadcast while the session bucket cannot be read: status=%d body=%s, want 503",
			response.Code, response.Body.String())
	}
	if failure := decodeRigBody[rigFailure](t, response); failure.Code != "ENVOY_UNAVAILABLE" {
		t.Fatalf("the refusal = %#v, want ENVOY_UNAVAILABLE", failure)
	}
	if rows := rig.broadcastRows(t); rows != 0 {
		t.Fatalf("%d broadcasts were written, want none: a send whose reads did not answer commits nothing",
			rows)
	}
	planner.noFrame(t)
	tester.noFrame(t)

	attempt := rig.mention(t, key, planner.id, "Please take this one.").Deliveries[0]
	if attempt.State != "failed" || attempt.Error == nil {
		t.Fatalf("the mention's attempt = %#v, want it failed with a recorded cause", attempt)
	}
	// "no live session <id>" and anything starting with it alike: that prefix is the contract
	// comment_delivery.go classifies a gone session on, and the dashboard prints verbatim.
	if strings.HasPrefix(*attempt.Error, "no live session ") {
		t.Fatalf("the mention recorded %q, which Dispatch and the dashboard read as a session that has ended",
			*attempt.Error)
	}
}

// An issue's claim is the one thing that says two sessions never work the same issue. Before this
// change Dispatch judged its holder against the listener's live list, so a working session whose
// cache entry was still being refilled read as gone and its issue was handed to the next agent
// that asked — with no force flag, and with nothing to undo it once both had started. The holder
// is now read by id against the session store, so only a holder the store has no record of is
// taken over. Both sides are here: the cache that has not refilled keeps its holder's claim, and
// the session that genuinely ended loses it.
func TestDispatchRefusesAClaimTakeoverWhileTheCacheRefills(t *testing.T) {
	rig := newDispatchRig(t)
	holder := rig.registerSession(t, "ses_holder", "Implementer")
	contender := rig.registerSession(t, "ses_contender", "Second agent")
	working := rig.issueKey(t, "Work a refilling cache must not hand over")

	// Claiming a free issue judges nobody, so the holder takes it without any read: the cold
	// cache is still cold when the contender's claim asks about it.
	if claimed := rig.claim(t, working, holder); claimed.Code != http.StatusOK {
		t.Fatalf("%s claims %s: status=%d body=%s",
			holder.id, working, claimed.Code, claimed.Body.String())
	}

	contested := rig.claim(t, working, contender)
	if contested.Code != http.StatusConflict {
		t.Fatalf("a takeover from a holder the cache has not refilled: status=%d body=%s, want 409",
			contested.Code, contested.Body.String())
	}
	refusal := decodeRigBody[rigFailure](t, contested)
	if refusal.Code != "ISSUE_CLAIMED" ||
		!strings.Contains(refusal.Error, holder.id) || !strings.Contains(refusal.Error, holder.title) {
		t.Fatalf("the refusal = %#v, want ISSUE_CLAIMED naming %s and the title its record carries",
			refusal, holder.id)
	}

	// The other side: a holder whose record the session bucket no longer has is gone, and its
	// issue is free for any agent to take.
	ended := rig.registerSession(t, "ses_ended", "Agent that stopped")
	abandoned := rig.issueKey(t, "Work a session that stopped leaves behind")
	if claimed := rig.claim(t, abandoned, ended); claimed.Code != http.StatusOK {
		t.Fatalf("%s claims %s: status=%d body=%s",
			ended.id, abandoned, claimed.Code, claimed.Body.String())
	}
	rig.endSession(t, ended.id)

	takeover := rig.claim(t, abandoned, contender)
	if takeover.Code != http.StatusOK {
		t.Fatalf("a takeover from a holder the bucket has lost: status=%d body=%s, want it granted",
			takeover.Code, takeover.Body.String())
	}
	took := decodeRigBody[struct {
		Claim *struct {
			Actor struct {
				ID string `json:"id"`
			} `json:"actor"`
		} `json:"claim"`
	}](t, takeover)
	if took.Claim == nil || took.Claim.Actor.ID != contender.id {
		t.Fatalf("%s now holds %s: %#v", contender.id, abandoned, took.Claim)
	}
}

// claim posts the claim `dispatch claim` posts: the session acting, named from the host's own
// runtime, with the title it is running under.
func (rig *dispatchRig) claim(
	t *testing.T, key string, live *rigSession,
) *httptest.ResponseRecorder {
	t.Helper()
	return rig.asSession(t, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": map[string]any{
			"kind":   "session",
			"id":     live.id,
			"origin": map[string]any{"session_title": live.title},
		},
	})
}
