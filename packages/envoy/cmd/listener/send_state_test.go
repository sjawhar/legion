package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// sendStateSender is the session every send below names as its source, so no answer here turns on
// the sender's own record.
const sendStateSender = "ses_state_sender"

// sendStateRig is the send route and GET /v1/registry/<session_id> over one NATS server, with the
// known-session store both answers read, as main wires them with every store open. clock is what
// that store stamps each record with and reads its 30-day retention against; a test moves it.
type sendStateRig struct {
	harness listenerDeliveryHarness
	deps    *listenerDeps
	known   *session.KnownSessions
	mux     *http.ServeMux
	url     string
	clock   time.Time
}

func newSendStateRig(t *testing.T) *sendStateRig {
	t.Helper()
	return newSendStateRigOn(t, newListenerDeliveryHarness(t, nil), testnats.URL(t))
}

// newSendStateRigOn is newSendStateRig over a listener connected to a server the test chose, one
// whose grant it set among them.
func newSendStateRigOn(t *testing.T, harness listenerDeliveryHarness, url string) *sendStateRig {
	t.Helper()
	waitForRegistry(t, harness.registry)
	rig := &sendStateRig{harness: harness, url: url, clock: time.Now()}
	known, err := session.OpenKnownSessions(harness.client.Conn,
		session.WithKnownSessionReplicas(1),
		session.WithKnownSessionClock(func() time.Time { return rig.clock }))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	deps := harness.publishDeps(t)
	deps.known = known
	rig.deps, rig.known = deps, known
	rig.mux = http.NewServeMux()
	registerV1Routes(rig.mux, deps, "test-machine", harness.config.logger)
	return rig
}

// registerThrough runs one registration of sessionID through a listener's own /v1 routes, as a
// plugin holding its own NATS connection sends it: that writes the interest row, the session
// entry and the known-session record. fields overrides or adds to the registration's defaults,
// "presence": true, "capabilities" and "delivery" among them.
func registerThrough(t *testing.T, mux *http.ServeMux, sessionID string, fields map[string]any) {
	t.Helper()
	body := map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"title":           "a checked session",
		"topics":          []string{},
	}
	maps.Copy(body, fields)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, postJSON("/v1/interests/subscribe", string(encoded)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("register %s: status %d, body %s", sessionID, recorder.Code, recorder.Body.String())
	}
}

// stopThrough ends sessionID the way a session that shuts down cleanly does, through the real
// route: its session entry goes and its known-session record stays, moved to the store's clock.
func stopThrough(t *testing.T, mux *http.ServeMux, sessionID string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stop %s: status %d, body %s", sessionID, recorder.Code, recorder.Body.String())
	}
}

func (rig *sendStateRig) register(t *testing.T, sessionID string, fields map[string]any) {
	t.Helper()
	registerThrough(t, rig.mux, sessionID, fields)
}

func (rig *sendStateRig) stop(t *testing.T, sessionID string) {
	t.Helper()
	stopThrough(t, rig.mux, sessionID)
	if _, err := rig.deps.sessions.Get(sessionID); err == nil {
		t.Fatalf("%s still holds a session entry, so it did not stop", sessionID)
	}
}

// entry is sessionID's session entry, which must be there.
func (rig *sendStateRig) entry(t *testing.T, sessionID string) session.SessionEntry {
	t.Helper()
	entry, err := rig.deps.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("read the session entry of %s: %v", sessionID, err)
	}
	return entry
}

// record is sessionID's known-session record, which must be there.
func (rig *sendStateRig) record(t *testing.T, sessionID string) session.KnownSession {
	t.Helper()
	stored, known := rig.known.Get(sessionID)
	if !known {
		t.Fatalf("the known-session store holds no record of %s", sessionID)
	}
	return stored
}

// sendAnswer is one send's answer as these tests read it: the status, the raw body (what an older
// client's keys are asserted on), the fields this change adds, and how long the route took, which
// is what the presence check's bound is asserted on.
type sendAnswer struct {
	status  int
	raw     string
	elapsed time.Duration

	EventID   string `json:"event_id"`
	Recipient string `json:"recipient"`
	State     string `json:"state"`
	LastSeen  int64  `json:"last_seen"`
	Error     string `json:"error"`
}

// sendThroughRoutes sends one message to target through a listener's own /v1 routes. fields
// overrides or adds to the request's defaults, "connected_only" and "payload" among them.
func sendThroughRoutes(t *testing.T, mux *http.ServeMux, target string, fields map[string]any) sendAnswer {
	t.Helper()
	body := map[string]any{
		"target_session": target,
		"source":         "agent",
		"source_session": sendStateSender,
		"message":        "are you there?",
	}
	maps.Copy(body, fields)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	started := time.Now()
	mux.ServeHTTP(recorder, postJSON("/v1/messages/send", string(encoded)))
	answer := sendAnswer{status: recorder.Code, raw: recorder.Body.String(), elapsed: time.Since(started)}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the send answer %s: %v", answer.raw, err)
	}
	return answer
}

func (rig *sendStateRig) send(t *testing.T, target string, fields map[string]any) sendAnswer {
	t.Helper()
	return sendThroughRoutes(t, rig.mux, target, fields)
}

// registryRoute reads GET /v1/registry/<session_id>, with query appended as it stands ("" for no
// parameter at all).
func (rig *sendStateRig) registryRoute(t *testing.T, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/registry/"+sessionID+query, nil))
	return recorder
}

// storedOn is how many messages the stream holds on subject.
func (rig *sendStateRig) storedOn(t *testing.T, subject string) uint64 {
	t.Helper()
	info, err := rig.harness.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read what the stream holds on %s: %v", subject, err)
	}
	return info.State.Subjects[subject]
}

// presenceStandIn is a session's plugin as far as the presence check is concerned: a NATS
// connection of its own, subscribed to the session's presence subject, answering each request
// with that session's id and its client instance id (presenceReply). close closes the connection
// and deregisters nothing, which is what a killed session process leaves behind - the entry
// stands and nothing answers for it.
type presenceStandIn struct {
	conn  *natsgo.Conn
	asked atomic.Int32
}

func startPresenceStandIn(t *testing.T, url, sessionID, instanceID string) *presenceStandIn {
	t.Helper()
	return startPresenceStandInOn(t, testnats.Connect(t, url), sessionID, instanceID)
}

// startPresenceStandInOn is startPresenceStandIn on a connection the test opened itself, which a
// server whose users are granted needs: the plugin authenticates as that server's one user.
func startPresenceStandInOn(t *testing.T, conn *natsgo.Conn, sessionID, instanceID string) *presenceStandIn {
	t.Helper()
	standIn := &presenceStandIn{conn: conn}
	t.Cleanup(standIn.conn.Close)
	reply, err := json.Marshal(presenceReply{SessionID: sessionID, ClientInstanceID: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := standIn.conn.Subscribe(bus.PresenceSubject(sessionID), func(msg *natsgo.Msg) {
		standIn.asked.Add(1)
		if err := msg.Respond(reply); err != nil {
			t.Errorf("answer the presence check of %s: %v", sessionID, err)
		}
	})
	if err != nil {
		t.Fatalf("subscribe the plugin of %s to its presence subject: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := standIn.conn.Flush(); err != nil {
		t.Fatalf("flush the presence subscription of %s: %v", sessionID, err)
	}
	return standIn
}

func (s *presenceStandIn) close() { s.conn.Close() }

// startLiveTopicsLane opens a second listener's live topics lane on the same server, subscribed
// to every notification topic in another machine's live queue group, as a deployment running
// ENVOY_SUBSCRIBE_ALL_TOPICS=1 does (openLiveTopics). It is what makes the presence check's
// one-second wait load-bearing: with a core subscriber on notifications.>, NATS never answers a
// presence request with its no-responders status, so the wait is the only thing that can report a
// session gone.
func startLiveTopicsLane(t *testing.T, url string, cfg listenerDeliveryHandlerConfig) {
	t.Helper()
	live, err := openLiveTopics([]string{url}, "other-machine", liveDeliveryHandler(cfg))
	if err != nil {
		t.Fatalf("open the second listener's live topics lane: %v", err)
	}
	t.Cleanup(live.Close)
}

// Acceptance A4. A session whose NATS connection closed with its entry still in place reads
// stopped, and reads it inside a second and a half of the request; a session whose registration
// declares no check reads connected while it has an entry, with nothing asked of it.
//
// A second listener's live topics lane holds a core subscription on notifications.> throughout,
// so no presence request here is answered no-responders and the one-second wait
// (presenceProbeTimeout) is what reports the gone session. The first send is the negative
// control: the same route, the same session, the same entry, with the plugin still answering, and
// it reads connected - so the stopped answer below can only come from the closed connection.
func TestASendReadsASessionWhoseConnectionClosedAsStopped(t *testing.T) {
	rig := newSendStateRig(t)
	startLiveTopicsLane(t, rig.url, rig.harness.config)

	const checked = "ses_cut_connection"
	plugin := startPresenceStandIn(t, rig.url, checked, "ci-cut")
	rig.register(t, checked, map[string]any{"presence": true, "client_instance_id": "ci-cut"})

	connected := rig.send(t, checked, nil)
	if connected.status != http.StatusOK || connected.State != sessionStateConnected {
		t.Fatalf("a send to a session answering its presence subject: status %d, body %s; want 200 connected",
			connected.status, connected.raw)
	}
	if connected.LastSeen != 0 {
		t.Fatalf("a connected send carries last_seen %d: %s; want the key left out", connected.LastSeen, connected.raw)
	}
	if asked := plugin.asked.Load(); asked != 1 {
		t.Fatalf("the plugin was asked %d times, want once", asked)
	}
	entry := rig.entry(t, checked)

	plugin.close()
	stopped := rig.send(t, checked, nil)

	if stopped.status != http.StatusOK || stopped.State != sessionStateStopped {
		t.Fatalf("a send to a session whose connection closed: status %d, body %s; want 200 stopped",
			stopped.status, stopped.raw)
	}
	if stopped.LastSeen != entry.UpdatedAt {
		t.Fatalf("the stopped answer's last_seen = %d, want the entry's updated_at %d: %s",
			stopped.LastSeen, entry.UpdatedAt, stopped.raw)
	}
	if stopped.elapsed > 1500*time.Millisecond {
		t.Fatalf("the send answered stopped after %s, want within 1.5s", stopped.elapsed)
	}
	t.Logf("the send to a session whose connection closed answered stopped after %s", stopped.elapsed)

	// A session whose registration declares no check: nothing of it answers a presence request
	// either, so an unconditional check would read it stopped. It has an entry, so it reads
	// connected, which is the answer every release gave before the check existed.
	const unchecked = "ses_no_declaration"
	rig.register(t, unchecked, nil)
	answer := rig.send(t, unchecked, nil)
	if answer.status != http.StatusOK || answer.State != sessionStateConnected {
		t.Fatalf("a send to a session that declares no presence check: status %d, body %s; want 200 connected",
			answer.status, answer.raw)
	}
	if answer.elapsed > 1500*time.Millisecond {
		t.Fatalf("the send to a session that declares no check answered after %s, so it was asked", answer.elapsed)
	}
}

// listenerGrantWithoutPresence is the listener's grant with the presence prefix left out of it: it
// may publish every notification subject this rig uses - the agent subjects a send stores on, the
// relay subjects and the exception lanes - and not a session's presence subject
// (bus.PresenceSubjectPrefix). A NATS publish grant is an allow list, so the prefix is left out by
// naming the rest rather than by denying it. The known-session bucket's subjects are added
// because this rig opens that store, which the grant that predates session consumers does not
// carry.
func listenerGrantWithoutPresence() []string {
	allow := make([]string, 0, 32)
	for _, subject := range listenerGrantBeforeSessionConsumers() {
		if subject == bus.NotificationTopicPrefix+">" {
			allow = append(allow,
				contracts.AgentTopicPrefix+">",
				bus.DeliverSubjectPrefix+">",
				bus.ExceptionTopicPrefix+">")
			continue
		}
		allow = append(allow, subject)
	}
	stream := "KV_" + bus.KnownSessionBucket
	return append(allow,
		"$JS.API.STREAM.INFO."+stream,
		"$JS.API.STREAM.CREATE."+stream,
		"$JS.API.DIRECT.GET."+stream+".>",
		"$JS.API.STREAM.MSG.GET."+stream,
		"$KV."+bus.KnownSessionBucket+".>",
		"$JS.API.CONSUMER.CREATE."+stream+".>",
		"$JS.API.CONSUMER.DELETE."+stream+".>",
		"$JS.FC."+stream+".>",
	)
}

// Acceptance A4, its third part: a listener whose NATS user may not publish the presence request
// answers connected, at once. The denial is the server's own - the user's publish grant names
// every subject this listener uses except the presence prefix, so the request comes back as
// bus.ErrPublishDenied from internal/bus's own detection and not from an error a seam injected.
// A listener that cannot ask must not call every session of its deployment stopped.
//
// The negative control is the plugin stand-in subscribed to the presence subject throughout: it
// is never asked, because the publish never leaves this listener, so connected here is the
// denial's answer and not a reply's.
func TestASendAnswersConnectedWhenTheListenerMayNotCheckPresence(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, listenerGrantWithoutPresence()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSendStateRigOn(t, newListenerDeliveryHarnessOn(t, client, nil), grant.URL)

	const denied = "ses_denied_check"
	pluginConn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect the plugin stand-in: %v", err)
	}
	plugin := startPresenceStandInOn(t, pluginConn, denied, "ci-denied")
	rig.register(t, denied, map[string]any{"presence": true, "client_instance_id": "ci-denied"})

	answer := rig.send(t, denied, nil)

	if answer.status != http.StatusOK || answer.State != sessionStateConnected {
		t.Fatalf("a send whose presence request the server denies: status %d, body %s; want 200 connected",
			answer.status, answer.raw)
	}
	if answer.elapsed > 1500*time.Millisecond {
		t.Fatalf("the denied check answered after %s, want within 1.5s", answer.elapsed)
	}
	if asked := plugin.asked.Load(); asked != 0 {
		t.Fatalf("the plugin was asked %d times, so the publish was not denied", asked)
	}
	if logs := rig.harness.logs.String(); !strings.Contains(logs, "listener presence check denied under its own grant") {
		t.Fatalf("the denial was not logged:\n%s", logs)
	}
	t.Logf("the send whose presence request was denied answered connected after %s", answer.elapsed)
}

// Acceptance A6. A send to a session neither store knows is the 404 this route has always
// answered, naming the id: Dispatch classifies a delivery failure on that prefix. A
// known-session record older than the 30-day retention is no record at all, so a session the
// store has aged out is that same 404 and not a stopped answer with a month-old last_seen.
func TestASendToASessionNeitherStoreKnowsIsTheNotFound(t *testing.T) {
	rig := newSendStateRig(t)

	unknown := rig.send(t, "ses_never_registered", nil)
	if unknown.status != http.StatusNotFound {
		t.Fatalf("a send to a session neither store knows: status %d, body %s; want 404", unknown.status, unknown.raw)
	}
	if unknown.Error != "no live session ses_never_registered" {
		t.Fatalf("the 404 reads %q, want it to name the session", unknown.Error)
	}

	// The negative control for the aged-out case: while the record is young the same send is the
	// 200 that proves the rig can answer stopped at all.
	const aged = "ses_aged_out"
	rig.register(t, aged, nil)
	rig.stop(t, aged)
	young := rig.send(t, aged, nil)
	if young.status != http.StatusOK || young.State != sessionStateStopped {
		t.Fatalf("a send to a session with a fresh record: status %d, body %s; want 200 stopped", young.status, young.raw)
	}

	rig.clock = rig.clock.Add(session.KnownSessionRetention + time.Hour)
	if _, known := rig.known.Get(aged); known {
		t.Fatalf("the store still answers a record of %s past its retention, so the route's answer shows nothing", aged)
	}
	old := rig.send(t, aged, nil)
	if old.status != http.StatusNotFound {
		t.Fatalf("a send to a session whose record aged out: status %d, body %s; want 404", old.status, old.raw)
	}
	if old.Error != "no live session "+aged {
		t.Fatalf("the 404 reads %q, want it to name the session", old.Error)
	}
}

// A send to a session with no entry is judged by the capabilities its known-session record
// carries, which are the ones that session last registered with: a mode the record advertises is
// stored, one it does not is the 403 this route answers a live session that will not take the
// mode. A record carrying no capabilities at all - one written before the field, or by a plugin
// that declared none - is not a session that advertises nothing, so its send is accepted and the
// mode is left to the session to judge when it resumes.
func TestASendToAStoppedSessionIsJudgedByItsRecordsCapabilities(t *testing.T) {
	rig := newSendStateRig(t)
	const steerFrame = `{"event":{"id":0,"issue_key":"CORE-1"},"delivery":{"attempt":1,"mode":"steer"}}`

	for _, each := range []struct {
		name         string
		sessionID    string
		capabilities []string
		wantStatus   int
		wantError    string
	}{
		{"the record advertises the mode", "ses_record_steers", []string{"aside", "steer"}, http.StatusOK, ""},
		{"the record advertises another mode", "ses_record_btw", []string{"btw"}, http.StatusForbidden,
			"session ses_record_btw (a checked session) does not advertise steer"},
		{"the record carries no capabilities", "ses_record_silent", nil, http.StatusOK, ""},
	} {
		t.Run(each.name, func(t *testing.T) {
			fields := map[string]any{}
			if each.capabilities != nil {
				fields["capabilities"] = each.capabilities
			}
			rig.register(t, each.sessionID, fields)
			rig.stop(t, each.sessionID)

			answer := rig.send(t, each.sessionID, map[string]any{"payload": steerFrame})

			if answer.status != each.wantStatus {
				t.Fatalf("a steer send to %s: status %d, body %s; want %d",
					each.sessionID, answer.status, answer.raw, each.wantStatus)
			}
			if each.wantError != "" && answer.Error != each.wantError {
				t.Fatalf("the refusal reads %q, want %q", answer.Error, each.wantError)
			}
			if each.wantStatus == http.StatusOK && answer.State != sessionStateStopped {
				t.Fatalf("the accepted send answers state %q, want stopped: %s", answer.State, answer.raw)
			}
		})
	}
}

// connected_only is the flag Dispatch's model and effort switch sets: applied at an unknown later
// time such a switch is worse than none, so a stopped target is answered 409 and nothing is
// stored for it. A connected target is the ordinary 200, and a send with the flag absent stores
// for a stopped target as it always has.
func TestConnectedOnlyRefusesAStoppedTargetAndStoresNothing(t *testing.T) {
	rig := newSendStateRig(t)

	const gone = "ses_switch_stopped"
	rig.register(t, gone, nil)
	rig.stop(t, gone)
	subject := contracts.AgentSubject(gone)
	if held := rig.storedOn(t, subject); held != 0 {
		t.Fatalf("the stream already holds %d messages on %s", held, subject)
	}

	refused := rig.send(t, gone, map[string]any{"connected_only": true})

	if refused.status != http.StatusConflict {
		t.Fatalf("a connected_only send to a stopped session: status %d, body %s; want 409", refused.status, refused.raw)
	}
	if refused.Error != "the session is stopped" {
		t.Fatalf("the 409 reads %q, want \"the session is stopped\"", refused.Error)
	}
	if held := rig.storedOn(t, subject); held != 0 {
		t.Fatalf("the refused send stored %d messages on %s, want none", held, subject)
	}
	// The negative control: the same session, the same route, the flag left off. It stores, so
	// the 409 above is the flag's doing and not the rig's.
	if stored := rig.send(t, gone, nil); stored.status != http.StatusOK || stored.State != sessionStateStopped {
		t.Fatalf("the same send without the flag: status %d, body %s; want 200 stopped", stored.status, stored.raw)
	}
	if held := rig.storedOn(t, subject); held != 1 {
		t.Fatalf("the stream holds %d messages on %s after the send without the flag, want 1", held, subject)
	}

	const live = "ses_switch_connected"
	plugin := startPresenceStandIn(t, rig.url, live, "ci-switch")
	rig.register(t, live, map[string]any{"presence": true, "client_instance_id": "ci-switch"})
	answer := rig.send(t, live, map[string]any{"connected_only": true})
	if answer.status != http.StatusOK || answer.State != sessionStateConnected {
		t.Fatalf("a connected_only send to a connected session: status %d, body %s; want 200 connected",
			answer.status, answer.raw)
	}
	if asked := plugin.asked.Load(); asked != 1 {
		t.Fatalf("the plugin was asked %d times, want once", asked)
	}
}

// Acceptance A8, the send route's half: a send to a session that stopped cleanly answers from the
// caches it already holds. Over warm caches 100 of them send no JetStream API request on the
// consumer or stream-info subjects, and the probe shows it is watching by seeing the one call the
// test makes itself (watchJetStreamAPI, which the publish route's half of A8 uses too).
func TestAHundredSendsToAStoppedSessionSendNoJetStreamAPIRequest(t *testing.T) {
	rig := newSendStateRig(t)
	const gone = "ses_a8_send_half"
	rig.register(t, gone, nil)
	rig.stop(t, gone)
	// One send before the probe starts, so anything a first send would set up is already set up
	// and the window below holds the steady state.
	if warm := rig.send(t, gone, nil); warm.status != http.StatusOK || warm.State != sessionStateStopped {
		t.Fatalf("the warming send: status %d, body %s; want 200 stopped", warm.status, warm.raw)
	}

	probe := watchJetStreamAPI(t, rig.url, rig.harness.client.Conn, "$JS.API.CONSUMER.>", "$JS.API.STREAM.INFO.>")
	for i := range 100 {
		answer := rig.send(t, gone, map[string]any{"idempotency_key": "a8-send-" + strconv.Itoa(i)})
		if answer.status != http.StatusOK || answer.State != sessionStateStopped {
			t.Fatalf("send %d: status %d, body %s; want 200 stopped", i, answer.status, answer.raw)
		}
	}

	control := "$JS.API.STREAM.INFO." + bus.Stream
	if _, err := rig.harness.client.JS().StreamInfo(bus.Stream); err != nil {
		t.Fatalf("the probe's control call: %v", err)
	}
	if calls := probe.awaitCall(t, control); len(calls) > 0 {
		t.Fatalf("100 sends sent %d JetStream API requests: %v", len(calls), calls)
	}
}

// GET /v1/registry/<session_id> answers a stopped session only when the request asks for one.
// Without include_stopped the route answers exactly what it always has - the 404 for a session
// that stopped cleanly, and a live session's body with no state key - which is what the Agents
// page, issue_claim.go's holder liveness and every other reader of it still read. With it, a
// stopped session is 200 with the record's own fields and no port and no host at all, a live
// session is its ordinary body with state "connected", and a session neither store knows is still
// the 404.
func TestTheRegistryRouteAnswersAStoppedSessionOnlyWhenAsked(t *testing.T) {
	rig := newSendStateRig(t)
	const gone = "ses_registry_stopped"
	rig.register(t, gone, map[string]any{
		"title":        "the stopped one",
		"capabilities": []string{"aside", "btw"},
		"topics":       []string{"notifications.team.registry-state"},
	})
	stored := rig.record(t, gone)
	rig.stop(t, gone)

	plain := rig.registryRoute(t, gone, "")
	if plain.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/registry/%s with no parameter: status %d, body %s; want 404", gone, plain.Code, plain.Body.String())
	}

	for _, spelling := range []string{"?include_stopped=1", "?include_stopped=true"} {
		t.Run(spelling, func(t *testing.T) {
			answer := rig.registryRoute(t, gone, spelling)
			if answer.Code != http.StatusOK {
				t.Fatalf("GET /v1/registry/%s%s: status %d, body %s; want 200", gone, spelling, answer.Code, answer.Body.String())
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(answer.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode %s: %v", answer.Body.String(), err)
			}
			for _, key := range []string{"port", "host"} {
				if value, carried := raw[key]; carried {
					t.Fatalf("the stopped answer carries %s: %s; want the key left out entirely: %s",
						key, value, answer.Body.String())
				}
			}
			var info stoppedSessionInfo
			if err := json.Unmarshal(answer.Body.Bytes(), &info); err != nil {
				t.Fatalf("decode %s: %v", answer.Body.String(), err)
			}
			want := stoppedSessionInfo{
				SessionID:    gone,
				State:        sessionStateStopped,
				LastSeen:     stored.LastSeen,
				Title:        "the stopped one",
				Capabilities: []string{"aside", "btw"},
				Topics:       stored.Topics,
			}
			if info.SessionID != want.SessionID || info.State != want.State || info.LastSeen != want.LastSeen ||
				info.Title != want.Title || strings.Join(info.Capabilities, ",") != strings.Join(want.Capabilities, ",") ||
				strings.Join(info.Topics, ",") != strings.Join(want.Topics, ",") {
				t.Fatalf("the stopped answer is %+v, want %+v: %s", info, want, answer.Body.String())
			}
		})
	}

	const live = "ses_registry_live"
	rig.register(t, live, nil)
	plainLive := rig.registryRoute(t, live, "")
	if plainLive.Code != http.StatusOK {
		t.Fatalf("GET /v1/registry/%s: status %d, body %s; want 200", live, plainLive.Code, plainLive.Body.String())
	}
	var plainRaw map[string]json.RawMessage
	if err := json.Unmarshal(plainLive.Body.Bytes(), &plainRaw); err != nil {
		t.Fatalf("decode %s: %v", plainLive.Body.String(), err)
	}
	if value, carried := plainRaw["state"]; carried {
		t.Fatalf("a live session answered with no parameter carries state: %s; want the key left out: %s",
			value, plainLive.Body.String())
	}
	askedLive := rig.registryRoute(t, live, "?include_stopped=1")
	if askedLive.Code != http.StatusOK {
		t.Fatalf("GET /v1/registry/%s?include_stopped=1: status %d, body %s; want 200", live, askedLive.Code, askedLive.Body.String())
	}
	// The ordinary body, plus state: every field the plain answer carried is still there, with
	// the one key this request adds.
	var plainInfo, liveInfo sessionInfo
	if err := json.Unmarshal(plainLive.Body.Bytes(), &plainInfo); err != nil {
		t.Fatalf("decode %s: %v", plainLive.Body.String(), err)
	}
	if err := json.Unmarshal(askedLive.Body.Bytes(), &liveInfo); err != nil {
		t.Fatalf("decode %s: %v", askedLive.Body.String(), err)
	}
	if liveInfo.State != sessionStateConnected {
		t.Fatalf("a live session asked with the parameter answers state %q, want connected: %s",
			liveInfo.State, askedLive.Body.String())
	}
	plainInfo.State = sessionStateConnected
	if !reflect.DeepEqual(plainInfo, liveInfo) {
		t.Fatalf("a live session asked with the parameter answers %+v, want its ordinary body %+v with state connected",
			liveInfo, plainInfo)
	}

	unknown := rig.registryRoute(t, "ses_registry_unknown", "?include_stopped=1")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/registry/ses_registry_unknown?include_stopped=1: status %d, body %s; want 404",
			unknown.Code, unknown.Body.String())
	}
}
