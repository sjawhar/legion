package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// INFRA-626: a session the Envoy listener knows and that is not running is a recipient, not a
// refusal. The listener stores the message for it and hands it over when it runs again, and
// Dispatch records that attempt `queued`. A session the listener knows nothing about is refused
// exactly as before, by the same text.

// stoppedSessionState and connectedSessionState are what the fake listener below puts in `state`,
// the two the real one uses (cmd/listener/presence.go).
const (
	connectedSessionState = "connected"
	stoppedSessionState   = "stopped"
)

// fakeSession is one session the queueing listener knows. registry is what
// GET /v1/registry/<id>?include_stopped=1 answers for it; a stopped session is a 404 without the
// parameter, as the real route answers. send is what POST /v1/messages/send answers for it, which
// differs from registry for the one case the acceptance names: a session whose registry entry is
// still in place but whose process is gone reads connected and sends stopped.
type fakeSession struct {
	title        string
	capabilities []string
	registry     string
	send         string
}

// queueingListener is a fake Envoy listener that knows some sessions as running and some as
// stopped, answers include_stopped as the real one does, and records every registry read and
// every send so a test can say what Dispatch asked for.
type queueingListener struct {
	url      string
	sessions map[string]fakeSession
	mu       sync.Mutex
	sends    []map[string]any
	// stoppedReads counts the registry reads that asked for stopped sessions too.
	stoppedReads int
	// liveOnlyReads counts the registry reads that did not.
	liveOnlyReads int
}

const stoppedLastSeen = 1762000000000

func newQueueingListener(t *testing.T, sessions map[string]fakeSession) *queueingListener {
	t.Helper()
	listener := &queueingListener{sessions: sessions}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			listener.writeLiveSessions(w)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			listener.writeRegistry(t, w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			listener.writeSend(t, w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	listener.url = server.URL
	return listener
}

func (l *queueingListener) writeLiveSessions(w http.ResponseWriter) {
	rows := []map[string]any{}
	for id, session := range l.sessions {
		if session.registry != connectedSessionState {
			continue
		}
		rows = append(rows, map[string]any{
			"session_id": id, "title": session.title, "machine_id": "m1", "host": "m1",
			"capabilities": session.capabilities, "topics": []string{}, "roles": []string{},
			"last_seen": stoppedLastSeen,
		})
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func (l *queueingListener) writeRegistry(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
	withStopped := r.URL.Query().Get("include_stopped") == "1"
	l.mu.Lock()
	if withStopped {
		l.stoppedReads++
	} else {
		l.liveOnlyReads++
	}
	l.mu.Unlock()
	session, known := l.sessions[sessionID]
	if !known || (session.registry == stoppedSessionState && !withStopped) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no live session ` + sessionID + `"}`))
		return
	}
	body := map[string]any{
		"session_id": sessionID, "title": session.title,
		"capabilities": session.capabilities, "topics": []string{},
	}
	if session.registry == stoppedSessionState {
		// The stopped shape carries no port, host, machine or dir, as the record holds none.
		body["state"] = stoppedSessionState
		body["last_seen"] = stoppedLastSeen
	} else {
		body["machine_id"], body["host"], body["roles"] = "m1", "m1", []string{}
		body["last_seen"] = stoppedLastSeen
		if withStopped {
			body["state"] = connectedSessionState
		}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (l *queueingListener) writeSend(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode a send: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	target, _ := request["target_session"].(string)
	session, known := l.sessions[target]
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no live session ` + target + `"}`))
		return
	}
	state := session.send
	if state == "" {
		state = session.registry
	}
	if state == stoppedSessionState && request["connected_only"] == true {
		// The listener stores nothing for a connected_only send to a stopped target, so this
		// send is not recorded either.
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"the session is stopped"}`))
		return
	}
	l.mu.Lock()
	l.sends = append(l.sends, request)
	l.mu.Unlock()
	answer := map[string]any{"event_id": "envelope-" + target, "recipient": target, "state": state}
	if state == stoppedSessionState {
		answer["last_seen"] = stoppedLastSeen
	}
	_ = json.NewEncoder(w).Encode(answer)
}

// sentTo is every send the listener took for target, in order.
func (l *queueingListener) sentTo(target string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	took := []map[string]any{}
	for _, send := range l.sends {
		if send["target_session"] == target {
			took = append(took, send)
		}
	}
	return took
}

// stoppedAndLive is the listener every test below but the unknown-session ones runs against: s1
// stopped but known, s2 running, and crashed, whose registry entry is still in place while its
// process has gone.
func stoppedAndLive(t *testing.T) *queueingListener {
	t.Helper()
	modes := []string{"aside", "btw", "steer"}
	return newQueueingListener(t, map[string]fakeSession{
		"s1":      {title: "planner", capabilities: modes, registry: stoppedSessionState},
		"s2":      {title: "tester", capabilities: modes, registry: connectedSessionState},
		"crashed": {title: "crashed", capabilities: modes, registry: connectedSessionState, send: stoppedSessionState},
	})
}

// A person's direct message to a stopped but known session reaches the listener, which stores it
// for that session, and the attempt is recorded queued - never failed "no live session", which is
// what Dispatch recorded before this change and what told the person their message was thrown
// away. The receipt the conversation renders carries the same word.
func TestDirectMessageToAStoppedSessionIsQueued(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Status when you are back?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to a stopped session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "queued" ||
		message.Deliveries[0].Error != nil || message.Deliveries[0].SessionID != "s1" {
		t.Fatalf("the answer's deliveries = %#v, want one queued attempt pinned to s1", message.Deliveries)
	}
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 ||
		attempts[0].State != "queued" || attempts[0].EnvelopeID == nil {
		t.Fatalf("the stored attempts = %#v, want one queued attempt carrying its envelope", attempts)
	}
	if sends := listener.sentTo("s1"); len(sends) != 1 {
		t.Fatalf("the listener took %d sends for s1, want exactly one", len(sends))
	} else if _, asked := sends[0]["connected_only"]; asked {
		t.Fatalf("an ordinary direct message asked for a connected session: %#v", sends[0])
	}
	if receipts := messageDeliveryReceiptRows(t, database, message.ID); len(receipts) != 1 ||
		receipts[0].State != "queued" {
		t.Fatalf("the delivery receipts = %#v, want one carrying queued", receipts)
	}
	// The live-only read is what the Agents page and the claim holder's liveness ask for; a
	// delivery asks for the stopped sessions too, or it would never find this session at all.
	if listener.stoppedReads == 0 {
		t.Fatalf("the delivery made no registry read that asks for stopped sessions")
	}
}

// A mention of a stopped but known session is queued for it too, on one send: the mention's own.
func TestMentionOfAStoppedSessionIsQueuedOnOneSend(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, _ := newTargetedMessageHandler(t, listener.url)
	issue := createInteractionIssue(t, handler, "QUE", "A mention of a stopped session", "before")

	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "@s1 pick this up when you are back",
		"mentions": []map[string]string{{"target": "session:s1"}},
	}))
	if len(comment.Deliveries) != 1 || comment.Deliveries[0].State != "queued" ||
		comment.Deliveries[0].Error != nil ||
		comment.Deliveries[0].SessionID == nil || *comment.Deliveries[0].SessionID != "s1" {
		t.Fatalf("the mention's deliveries = %#v, want one queued attempt pinned to s1", comment.Deliveries)
	}
	if sends := listener.sentTo("s1"); len(sends) != 1 {
		t.Fatalf("the listener took %d sends for s1, want exactly one", len(sends))
	}
}

// A broadcast that names a stopped but known session beside a running one reaches both: the
// stopped one is a recipient with a message and a queued attempt of its own, and it is in no
// exclusion list.
func TestBroadcastQueuesForAStoppedRecipientAndExcludesItFromNothing(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, _ := newTargetedMessageHandler(t, listener.url)

	response := postBroadcast(t, handler, map[string]any{
		"body": "Stand down and report.", "delivery": "steer", "session_ids": []string{"s1", "s2"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	if len(created.Excluded) != 0 {
		t.Fatalf("excluded = %#v, want nothing excluded: both sessions are known", created.Excluded)
	}
	// A broadcast answers once it is committed, each recipient's delivery behind the answer.
	settled := awaitBroadcastDeliveries(t, handler, created.ID)
	states := map[string]string{}
	for _, recipient := range settled.Recipients {
		if len(recipient.Message.Deliveries) != 1 {
			t.Fatalf("%s's attempts = %#v, want one", recipient.SessionID, recipient.Message.Deliveries)
		}
		states[recipient.SessionID] = recipient.Message.Deliveries[0].State
	}
	if len(states) != 2 || states["s1"] != "queued" || states["s2"] != "sent" {
		t.Fatalf("recipient states = %#v, want s1 queued and s2 sent", states)
	}
	if sends := listener.sentTo("s1"); len(sends) != 1 {
		t.Fatalf("the listener took %d sends for the stopped recipient, want one", len(sends))
	}
}

// The send's own answer decides, not the registry read before it: a session whose entry was still
// in place when Dispatch resolved it, and whose process had gone by the send, is queued too.
func TestASendAnsweredStoppedIsQueuedThoughTheRegistryReadFoundItLive(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "crashed", map[string]any{
		"body": "Are you still there?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to a crashed session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 || attempts[0].State != "queued" {
		t.Fatalf("the stored attempts = %#v, want one queued attempt", attempts)
	}
}

// A session Envoy knows nothing about is refused exactly as it always was, by the text
// retryableDeliveryFailure and the dashboard both key on, and is still excluded from a broadcast
// with the reason they key on there.
func TestASessionTheListenerDoesNotKnowIsStillRefusedByTheSameText(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "ghost", map[string]any{
		"body": "Anyone there?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to an unknown session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	attempts := messageAttempts(t, database, message.ID)
	if len(attempts) != 1 || attempts[0].State != "failed" || attempts[0].Error == nil ||
		*attempts[0].Error != "no live session ghost" {
		t.Fatalf("the stored attempts = %#v, want one failed attempt reading \"no live session ghost\"", attempts)
	}
	if sends := listener.sentTo("ghost"); len(sends) != 0 {
		t.Fatalf("the listener took %d sends for a session it does not know, want none", len(sends))
	}

	response := postBroadcast(t, handler, map[string]any{
		"body": "Stand down.", "delivery": "steer", "session_ids": []string{"s1", "ghost"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	broadcast := decodeBody[broadcastResponse](t, response)
	if len(broadcast.Excluded) != 1 || broadcast.Excluded[0].SessionID != "ghost" ||
		broadcast.Excluded[0].Reason != "no live session" {
		t.Fatalf("excluded = %#v, want ghost alone, for no live session", broadcast.Excluded)
	}
	if len(broadcast.Recipients) != 1 || broadcast.Recipients[0].SessionID != "s1" {
		t.Fatalf("recipients = %#v, want the stopped but known session alone", broadcast.Recipients)
	}
}

// A queued attempt is the session's turn to take: the listener handed it the message, so the
// accept route must not answer ACCEPT_FAILED, which it answers for an attempt the person was told
// had failed.
func TestASessionAcceptsAQueuedAttemptAsItsTurn(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Take this as your turn.", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to a stopped session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 || attempts[0].State != "queued" {
		t.Fatalf("the stored attempts = %#v, want one queued attempt to accept", attempts)
	}

	accepted := acceptDelivery(t, handler, message.ID, 1, "s1")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept a queued attempt: status=%d code=%s body=%s",
			accepted.Code, responseCode(t, accepted), accepted.Body.String())
	}
	took := decodeBody[acceptedAttempt](t, accepted)
	if took.State != "queued" || took.AcceptedAs == nil || *took.AcceptedAs != acceptedAsUserTurn ||
		took.Body != "Take this as your turn." {
		t.Fatalf("the accepted attempt = %#v, want the queued attempt taken as a user turn", took)
	}
}

// The delivery sweep resumes and settles the attempts a dead sender left pending. A queued
// attempt is not one of them - the listener took its message - so a pass leaves it as it is, even
// on a message whose issue has closed, which is what the sweep settles failed.
func TestTheSweepLeavesAQueuedAttemptAlone(t *testing.T) {
	listener := stoppedAndLive(t)
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.url})
	ctx := context.Background()
	var messageID string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (author, body, target)
		values ('{"kind":"user","id":"alice"}', 'Queued and left alone.', 'session:s1')
		returning id::text
	`).Scan(&messageID); err != nil {
		t.Fatalf("seed the message: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, envelope_id,
		                                claimed_at, created_at, requested_by)
		values ($1::uuid, 1, 'steer', 's1', 'queued', 'envelope-s1',
		        now() - interval '10 minutes', now() - interval '10 minutes', '{"kind":"user","id":"alice"}')
	`, messageID); err != nil {
		t.Fatalf("seed the queued attempt: %v", err)
	}

	if err := served.resumeOwedDeliveries(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	attempts := messageAttempts(t, database, messageID)
	if len(attempts) != 1 || attempts[0].State != "queued" || attempts[0].Error != nil {
		t.Fatalf("after the sweep the attempts = %#v, want the one queued attempt untouched", attempts)
	}
	if sends := listener.sentTo("s1"); len(sends) != 0 {
		t.Fatalf("the sweep sent %d frames for a queued attempt, want none", len(sends))
	}
}

// An attempt whose sender died between the commit and the settle is resumed by the delivery
// sweep, which asks for a connected session only if the send that opened the attempt did. So the
// switch's own attempt, resumed against a stopped target, is settled failed and nothing is
// stored for the session, while the same attempt without the flag is queued. The flag lives on
// the row (message_deliveries.connected_only), because the sweep never saw the request.
func TestTheSweepResumesAStrandedSwitchAsConnectedOnly(t *testing.T) {
	for _, test := range []struct {
		name          string
		connectedOnly bool
		wantState     string
		wantSends     int
	}{
		{name: "a stranded switch", connectedOnly: true, wantState: "failed", wantSends: 0},
		{name: "a stranded ordinary send", connectedOnly: false, wantState: "queued", wantSends: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := stoppedAndLive(t)
			_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.url})
			ctx := context.Background()
			var messageID string
			if err := database.Pool.QueryRow(ctx, `
				insert into messages (author, body, target)
				values ('{"kind":"user","id":"alice"}', '/model claude-opus', 'session:s1')
				returning id::text
			`).Scan(&messageID); err != nil {
				t.Fatalf("seed the message: %v", err)
			}
			// The attempt its own request committed and never settled: pending, claimed ten
			// minutes ago, which is what the sweep takes.
			if _, err := database.Pool.Exec(ctx, `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state,
				                                claimed_at, created_at, requested_by, connected_only)
				values ($1::uuid, 1, 'steer', 's1', 'pending',
				        now() - interval '10 minutes', now() - interval '10 minutes',
				        '{"kind":"user","id":"alice"}', $2)
			`, messageID, test.connectedOnly); err != nil {
				t.Fatalf("strand the attempt: %v", err)
			}

			if err := served.resumeOwedDeliveries(ctx); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			attempts := messageAttempts(t, database, messageID)
			if len(attempts) != 1 || attempts[0].State != test.wantState ||
				attempts[0].ConnectedOnly != test.connectedOnly {
				t.Fatalf("the resumed attempt = %#v, want one %s attempt carrying connected_only=%v",
					attempts, test.wantState, test.connectedOnly)
			}
			if test.wantState == "failed" && (attempts[0].Error == nil || *attempts[0].Error != "the session is stopped") {
				t.Fatalf("the refused switch's error = %v, want \"the session is stopped\"", attempts[0].Error)
			}
			sends := listener.sentTo("s1")
			if len(sends) != test.wantSends {
				t.Fatalf("the listener stored %d sends, want %d", len(sends), test.wantSends)
			}
			if test.wantSends == 1 && sends[0]["connected_only"] == true {
				t.Fatalf("an ordinary resumed send asked for a connected session: %#v", sends[0])
			}
		})
	}
}

// The Agents page's model and effort switch keeps its refusal: it asks the listener for a session
// that is running now (connected_only), and the 409 it meets settles the attempt failed with the
// listener's own reason. Nothing is stored for the session, so nothing applies at an unknown
// later time.
func TestTheModelSwitchToAStoppedSessionIsRefusedAndQueuesNothing(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "/model claude-opus", "delivery": "steer", "connected_only": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("the switch: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	attempts := messageAttempts(t, database, message.ID)
	if len(attempts) != 1 || attempts[0].State != "failed" || attempts[0].Error == nil ||
		*attempts[0].Error != "the session is stopped" {
		t.Fatalf("the switch's attempts = %#v, want one failed attempt reading \"the session is stopped\"", attempts)
	}
	if sends := listener.sentTo("s1"); len(sends) != 0 {
		t.Fatalf("the listener stored %d sends for the refused switch, want none", len(sends))
	}

	// The same switch to a running session is sent, under the same flag, and recorded sent.
	live := postAgentMessage(t, handler, "s2", map[string]any{
		"body": "/model claude-opus", "delivery": "steer", "connected_only": true,
	}, "alice")
	if live.Code != http.StatusCreated {
		t.Fatalf("the switch to a running session: status=%d body=%s", live.Code, live.Body.String())
	}
	running := decodeBody[model.Message](t, live)
	if attempts := messageAttempts(t, database, running.ID); len(attempts) != 1 || attempts[0].State != "sent" {
		t.Fatalf("the running session's attempts = %#v, want one sent attempt", attempts)
	}
	sends := listener.sentTo("s2")
	if len(sends) != 1 || sends[0]["connected_only"] != true {
		t.Fatalf("the switch's send = %#v, want one carrying connected_only", sends)
	}
}

// The claim holder's liveness read stays live-only: a stopped session is not a live holder, so
// it must read as no holder at all and the read must not ask for stopped sessions. A delivery's
// read of the same session, beside it, finds it.
func TestTheClaimHoldersLivenessReadStaysLiveOnly(t *testing.T) {
	listener := stoppedAndLive(t)
	_, _, deps := newTestServer(t, testServerOptions{envoyURL: listener.url})
	direct := directServer(deps)
	ctx := context.Background()

	liveness, err := direct.fetchHolderLiveness(ctx, model.IssueClaim{Actor: model.Actor{Kind: "session", ID: "s1"}})
	if err != nil {
		t.Fatalf("read the holder's liveness: %v", err)
	}
	if !liveness.fetched || liveness.holder != nil {
		t.Fatalf("liveness of a stopped holder = %#v, want fetched with no live holder", liveness)
	}
	if listener.stoppedReads != 0 {
		t.Fatalf("the holder's liveness made %d registry reads asking for stopped sessions, want none",
			listener.stoppedReads)
	}
	if listener.liveOnlyReads != 1 {
		t.Fatalf("the holder's liveness made %d live-only registry reads, want one", listener.liveOnlyReads)
	}

	// The same session, resolved for a delivery, is found: the two reads differ only in what
	// they ask for.
	sessionID, _, resolveError := direct.resolveDeliveryTarget(ctx, model.Route{Kind: "session", ID: "s1"}, "steer")
	if sessionID != "s1" || resolveError != "" {
		t.Fatalf("a delivery resolved s1 as (%q, %q), want it resolved with no error", sessionID, resolveError)
	}
}
