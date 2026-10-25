package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// agentSessions is the registry the keyed-send tests' fake listener answers: two sessions that
// take every mode.
const agentSessions = `[
	{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"],"last_seen":200},
	{"session_id":"s2","title":"reviewer","capabilities":["aside","btw","steer"],"last_seen":100}
]`

// postAgentMessage sends one message to sessionID as login, minting an idempotency key when the
// input names none: every send carries one, and most tests have no interest in which.
func postAgentMessage(t *testing.T, handler http.Handler, sessionID string, input map[string]any, login string) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := input["idempotency_key"]; !ok {
		input["idempotency_key"] = uuid.NewString()
	}
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+sessionID+"/messages", input, login)
}

// messageAttempts reads every delivery attempt of messageID, oldest first, straight from its rows.
func messageAttempts(t *testing.T, database *store.Store, messageID string) []model.MessageDelivery {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select `+messageDeliveryColumns+` from message_deliveries where message_id = $1 order by attempt
	`, messageID)
	if err != nil {
		t.Fatalf("read the attempts of %s: %v", messageID, err)
	}
	defer rows.Close()
	attempts := []model.MessageDelivery{}
	for rows.Next() {
		attempt, err := scanMessageDelivery(rows)
		if err != nil {
			t.Fatalf("scan an attempt of %s: %v", messageID, err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the attempts of %s: %v", messageID, err)
	}
	return attempts
}

// commitKeyedMessage writes what alice's keyed send to s1 commits before its delivery starts: the
// message and its key row, and nothing else, as a process that died between the commit and the
// attempt leaves them.
func commitKeyedMessage(t *testing.T, database *store.Store, key, body, delivery string) string {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	author, err := json.Marshal(alice)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, target) values (null, $1, $2, 'session:s1')
		returning id::text
	`, author, body).Scan(&id); err != nil {
		t.Fatalf("commit the message: %v", err)
	}
	held := newAgentMessageKey(alice, key, "s1", body, delivery, nil)
	if _, err := database.Pool.Exec(ctx, `
		insert into agent_message_idempotency_keys (login, idempotency_key, message_id, request_digest)
		values ($1, $2, $3, $4)
	`, held.login, held.key, id, held.digest); err != nil {
		t.Fatalf("commit the message's key: %v", err)
	}
	return id
}

// strandAttempt opens messageID's first attempt as a sender that claimed it claimedAgo and never
// came back leaves it: pending, naming sessionID (empty for one stranded before anything was
// resolved).
func strandAttempt(t *testing.T, database *store.Store, messageID, delivery, sessionID string, claimedAgo time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at, requested_by)
		values ($1, 1, $2, $3, 'pending', now() - make_interval(secs => $4), '{"kind":"user","id":"alice"}')
	`, messageID, delivery, sessionID, claimedAgo.Seconds()); err != nil {
		t.Fatalf("strand the attempt of %s: %v", messageID, err)
	}
}

// keysOf is every idempotency key the listener was sent, sorted.
func keysOf(l *broadcastListener) []string {
	l.Lock()
	defer l.Unlock()
	keys := make([]string, 0, len(l.sends))
	for _, send := range l.sends {
		keys = append(keys, send.key)
	}
	sort.Strings(keys)
	return keys
}

// INFRA-258. A single-session send names itself with an idempotency key, so its retry can be told
// from a second send. A send without one - a page loaded before the key was required, or any other
// keyless caller - is refused, saying what that page's person does, and writes and sends nothing.
func TestASingleSessionSendWithoutAKeyIsRefused(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	before := databaseFingerprint(t, database)

	keyless := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Status?", "delivery": "btw",
	}, "alice")
	refusal := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, keyless)
	if keyless.Code != http.StatusBadRequest || refusal.Code != "MESSAGE_INPUT" ||
		refusal.Error != "idempotency_key is required, one per send. On a page loaded before it was required: copy the message, reload the page and send it again" {
		t.Fatalf("a keyless send = %d %#v, want 400 MESSAGE_INPUT saying the key is required", keyless.Code, refusal)
	}

	for name, test := range map[string]struct {
		key, code, says string
	}{
		"a key with a space":        {"press 1", "MESSAGE_INPUT", "idempotency_key may use only letters, digits, '.', '_', ':' and '-'"},
		"a key with a slash":        {"press/1", "MESSAGE_INPUT", "idempotency_key may use only letters, digits, '.', '_', ':' and '-'"},
		"a key past 128 characters": {strings.Repeat("k", 129), "CAP_EXCEEDED", "idempotency_key is 1 characters over the 128-character limit (129/128)"},
	} {
		t.Run(name, func(t *testing.T) {
			response := postAgentMessage(t, handler, "s1", map[string]any{
				"body": "Status?", "delivery": "btw", "idempotency_key": test.key,
			}, "alice")
			refusal := decodeBody[struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}](t, response)
			if response.Code != http.StatusBadRequest || refusal.Code != test.code || refusal.Error != test.says {
				t.Fatalf("status=%d refusal=%#v, want 400 %s %q", response.Code, refusal, test.code, test.says)
			}
		})
	}
	assertUnchanged(t, "a refused send", before, databaseFingerprint(t, database))
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("listener sends = %v, want none", got)
	}

	if longest := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Status?", "delivery": "btw", "idempotency_key": strings.Repeat("k", 128),
	}, "alice"); longest.Code != http.StatusCreated {
		t.Fatalf("a 128-character key: status=%d body=%s, want 201", longest.Code, longest.Body.String())
	}
}

// A repeat of one send - a double press, a retry after a 5xx or a timeout - is answered the
// message the first request made, with its deliveries, and the session is handed it once.
func TestARepeatedKeyAnswersTheMessageItMadeAndDeliversOnce(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	send := func() *httptest.ResponseRecorder {
		t.Helper()
		return postAgentMessage(t, handler, "s1", map[string]any{
			"body": "Where is the dashboard?", "delivery": "aside", "idempotency_key": "press-1",
		}, "alice")
	}

	first := send()
	if first.Code != http.StatusCreated {
		t.Fatalf("first send: status=%d body=%s", first.Code, first.Body.String())
	}
	original := decodeBody[model.Message](t, first)
	if len(original.Deliveries) != 1 || original.Deliveries[0].State != "sent" {
		t.Fatalf("first send's deliveries = %#v, want one sent attempt", original.Deliveries)
	}

	repeat := send()
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat: status=%d body=%s, want 200 with the original message", repeat.Code, repeat.Body.String())
	}
	replayed := decodeBody[model.Message](t, repeat)
	if replayed.ID != original.ID || replayed.Body != original.Body {
		t.Fatalf("the repeat answered %#v, want the original %s", replayed, original.ID)
	}
	if len(replayed.Deliveries) != 1 || replayed.Deliveries[0].Attempt != 1 || replayed.Deliveries[0].State != "sent" {
		t.Fatalf("the repeat's deliveries = %#v, want the original's one sent attempt", replayed.Deliveries)
	}
	if messages := countMessages(t, database); messages != 1 {
		t.Fatalf("the database holds %d messages, want the one send", messages)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}
}

// A key names one request. Reused for a different one - another message, mode, session or reply -
// it is refused naming the message that used it, and nothing is written or sent: that message says
// something else, so answering with it would tell the caller a message it never sent had gone out.
func TestAKeyReusedForAnotherMessageIsRefused(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Status?", "delivery": "btw", "idempotency_key": "reused",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", created.Code, created.Body.String())
	}
	original := decodeBody[model.Message](t, created)
	before := databaseFingerprint(t, database)

	for name, changed := range map[string]struct {
		session string
		input   map[string]any
	}{
		"another message": {"s1", map[string]any{"body": "Stand down.", "delivery": "btw"}},
		"another mode":    {"s1", map[string]any{"body": "Status?", "delivery": "steer"}},
		"another session": {"s2", map[string]any{"body": "Status?", "delivery": "btw"}},
		"another reply":   {"s1", map[string]any{"body": "Status?", "delivery": "btw", "in_reply_to": original.ID}},
	} {
		t.Run(name, func(t *testing.T) {
			changed.input["idempotency_key"] = "reused"
			response := postAgentMessage(t, handler, changed.session, changed.input, "alice")
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", response.Code, response.Body.String())
			}
			refusal := decodeBody[struct {
				Code      string `json:"code"`
				Error     string `json:"error"`
				MessageID string `json:"message_id"`
			}](t, response)
			if refusal.Code != "MESSAGE_KEY_REUSED" || refusal.MessageID != original.ID ||
				!strings.Contains(refusal.Error, "this request was not sent") {
				t.Fatalf("refusal = %#v, want MESSAGE_KEY_REUSED naming %s and saying nothing was sent", refusal, original.ID)
			}
		})
	}
	assertUnchanged(t, "a reused key", before, databaseFingerprint(t, database))
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want the original's one and no other", got)
	}
}

// A retry racing its original, sixteen times over: every request carrying one key is past the
// replay check before any writes, so all of them reach the key's insert together. The key's
// primary key makes each but one wait for the first and roll back, and the session is handed the
// message once, whichever request opens its attempt.
func TestTwoConcurrentSendsUnderOneKeyMakeOneMessage(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	ctx := context.Background()
	// The table lock holds every request at its key's insert, past the replay check, until the
	// last of them has arrived there.
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table agent_message_idempotency_keys in share mode`); err != nil {
		t.Fatalf("lock the key table: %v", err)
	}

	const senders = 16
	responses := make(chan *httptest.ResponseRecorder, senders)
	for range senders {
		go func() {
			responses <- postAgentMessage(t, handler, "s1", map[string]any{
				"body": "Status?", "delivery": "btw", "idempotency_key": "race",
			}, "alice")
		}()
	}
	// Sixteen connections, less the holder's: every request that got one waits at the insert, and
	// the last waits for a connection.
	waitForDatabaseLocks(t, holder, senders-1)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the key table: %v", err)
	}

	codes := map[int]int{}
	ids := map[string]struct{}{}
	for range senders {
		select {
		case response := <-responses:
			codes[response.Code]++
			if response.Code == http.StatusOK || response.Code == http.StatusCreated {
				ids[decodeBody[model.Message](t, response).ID] = struct{}{}
			} else {
				t.Errorf("status=%d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a concurrent send did not answer")
		}
	}
	if codes[http.StatusCreated] != 1 || codes[http.StatusOK] != senders-1 {
		t.Fatalf("status codes = %v, want one 201 and %d 200s", codes, senders-1)
	}
	if len(ids) != 1 {
		t.Fatalf("the answers name %d messages, want one", len(ids))
	}
	if messages := countMessages(t, database); messages != 1 {
		t.Fatalf("the database holds %d messages, want one", messages)
	}
	for id := range ids {
		if attempts := messageAttempts(t, database, id); len(attempts) != 1 || attempts[0].State != "sent" {
			t.Fatalf("the message's attempts = %#v, want one sent attempt", attempts)
		}
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}
}

// A key belongs to the person who sent it. Another person's identical send under the same key is
// another send, since a message carries one author; the same person under another casing of the
// identity header is the same person, since every identity names a person by lowercase email.
func TestAKeyIsHeldPerPerson(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	send := func(login string) *httptest.ResponseRecorder {
		t.Helper()
		return postAgentMessage(t, handler, "s1", map[string]any{
			"body": "Status?", "delivery": "btw", "idempotency_key": "shared",
		}, login)
	}

	alices := send("alice")
	if alices.Code != http.StatusCreated {
		t.Fatalf("alice's send: status=%d body=%s", alices.Code, alices.Body.String())
	}
	original := decodeBody[model.Message](t, alices).ID
	bobs := send("bob")
	if bobs.Code != http.StatusCreated {
		t.Fatalf("bob's send under alice's key: status=%d body=%s, want a message of his own", bobs.Code, bobs.Body.String())
	}
	if got := decodeBody[model.Message](t, bobs).ID; got == original {
		t.Fatalf("bob's send answered alice's message %s", original)
	}
	again := send("Alice")
	if again.Code != http.StatusOK {
		t.Fatalf("Alice's repeat: status=%d body=%s, want 200", again.Code, again.Body.String())
	}
	if got := decodeBody[model.Message](t, again).ID; got != original {
		t.Fatalf("Alice's repeat answered %s, want alice's message %s", got, original)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1", "s1"}) {
		t.Fatalf("listener sends = %v, want alice's and bob's", got)
	}
}

// The page gives up on a send - a closed tab, its own deadline - after the message committed,
// while the listener still holds the frame. The delivery runs on past the request: the attempt
// settles sent, and the page's retry under the key is answered the message, sending nothing again.
func TestAClientAbortMidSendStillDeliversOnce(t *testing.T) {
	sending := make(chan struct{})
	release := make(chan struct{})
	var held atomic.Bool
	listener, sends := newBroadcastListener(t, agentSessions, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" && held.CompareAndSwap(false, true) {
			close(sending)
			<-release
		}
		return false
	})
	handler, database := newTargetedMessageHandler(t, listener.URL)

	input := map[string]any{"body": "Deploy now.", "delivery": "steer", "idempotency_key": "abort-1"}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/agents/s1/messages", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-sending:
	case <-time.After(10 * time.Second):
		t.Fatal("the send never reached the listener")
	}
	cancel()
	close(release)
	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the cut-off send never finished")
	}

	var id string
	if err := database.Pool.QueryRow(context.Background(),
		`select id::text from messages where body = 'Deploy now.'`).Scan(&id); err != nil {
		t.Fatalf("find the message: %v", err)
	}
	if attempts := messageAttempts(t, database, id); len(attempts) != 1 || attempts[0].State != "sent" {
		t.Fatalf("the cut-off send's attempts = %#v, want one attempt settled sent", attempts)
	}

	retry := postAgentMessage(t, handler, "s1", input, "alice")
	if retry.Code != http.StatusOK {
		t.Fatalf("the page's retry: status=%d body=%s, want 200", retry.Code, retry.Body.String())
	}
	if got := decodeBody[model.Message](t, retry); got.ID != id || len(got.Deliveries) != 1 || got.Deliveries[0].State != "sent" {
		t.Fatalf("the retry answered %#v, want message %s with its one sent attempt", got, id)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}
}

// A process that died between a send's commit and its first attempt leaves a message with no
// attempt at all. The page's retry under the key starts that delivery, in the message's own mode,
// and a later repeat finds it on its way and sends nothing more.
func TestARetryOfAMessageWithNoAttemptDeliversIt(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	id := commitKeyedMessage(t, database, "undelivered", "Still there?", "aside")
	retry := func() model.Message {
		t.Helper()
		response := postAgentMessage(t, handler, "s1", map[string]any{
			"body": "Still there?", "delivery": "aside", "idempotency_key": "undelivered",
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("retry: status=%d body=%s, want 200", response.Code, response.Body.String())
		}
		return decodeBody[model.Message](t, response)
	}

	got := retry()
	if got.ID != id || len(got.Deliveries) != 1 || got.Deliveries[0].Attempt != 1 ||
		got.Deliveries[0].State != "sent" || got.Deliveries[0].Delivery != "aside" || got.Deliveries[0].SessionID != "s1" {
		t.Fatalf("the retry answered %#v, want message %s with attempt 1 sent to s1 as an aside", got, id)
	}
	if keys := keysOf(sends); !equalStrings(keys, []string{id + ":aside"}) {
		t.Fatalf("listener sends = %v, want the message once, in its own mode", keys)
	}
	if again := retry(); len(again.Deliveries) != 1 {
		t.Fatalf("a second retry's deliveries = %#v, want the one attempt", again.Deliveries)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}
}

// A sender that claimed a send's attempt and never came back leaves it pending. While its claim
// is live it may still be sending, and a retry under the key leaves it to that sender; once the
// claim has lapsed the retry resumes the attempt under its own number, and so its own idempotency
// key, as a same-mode retry from the agent card does - an attempt stranded before anything was
// resolved included, which takes the message's session.
func TestARetryOfAStrandedSendResumesItsAttempt(t *testing.T) {
	for name, pinned := range map[string]string{
		"an attempt naming its session":        "s1",
		"an attempt stranded before resolving": "",
	} {
		t.Run(name, func(t *testing.T) {
			listener, sends := newBroadcastListener(t, agentSessions)
			handler, database := newTargetedMessageHandler(t, listener.URL)
			id := commitKeyedMessage(t, database, "stranded", "Status?", "steer")
			strandAttempt(t, database, id, "steer", pinned, 0)
			retry := func() model.Message {
				t.Helper()
				response := postAgentMessage(t, handler, "s1", map[string]any{
					"body": "Status?", "delivery": "steer", "idempotency_key": "stranded",
				}, "alice")
				if response.Code != http.StatusOK {
					t.Fatalf("retry: status=%d body=%s, want 200", response.Code, response.Body.String())
				}
				return decodeBody[model.Message](t, response)
			}

			inFlight := retry()
			if len(inFlight.Deliveries) != 1 || inFlight.Deliveries[0].State != "pending" {
				t.Fatalf("a retry beside a live claim answered %#v, want the one pending attempt", inFlight.Deliveries)
			}
			if got := sends.targets(); len(got) != 0 {
				t.Fatalf("a retry beside a live claim sent %v, want nothing: its sender may still be sending", got)
			}

			if _, err := database.Pool.Exec(context.Background(),
				`update message_deliveries set claimed_at = now() - interval '2 minutes' where message_id = $1`, id,
			); err != nil {
				t.Fatalf("lapse the claim: %v", err)
			}
			resumed := retry()
			if len(resumed.Deliveries) != 1 || resumed.Deliveries[0].Attempt != 1 ||
				resumed.Deliveries[0].State != "sent" || resumed.Deliveries[0].SessionID != "s1" {
				t.Fatalf("a retry beside a lapsed claim answered %#v, want attempt 1 resumed and sent to s1", resumed.Deliveries)
			}
			if keys := keysOf(sends); !equalStrings(keys, []string{id + ":steer"}) {
				t.Fatalf("listener sends = %v, want attempt 1 once, under its own key", keys)
			}
		})
	}
}

// A retry of a send that landed is answered from the database before the listener is read, so it
// is answered while the listener is down rather than failed as unreachable. A held send whose
// delivery never started is started even then, and the listener's outage is recorded on its
// attempt, never answered as a server error the page would retry.
func TestARetryIsAnsweredWhileTheListenerIsDown(t *testing.T) {
	var down atomic.Bool
	var asked atomic.Int32
	listener, sends := newBroadcastListener(t, agentSessions, func(w http.ResponseWriter, _ *http.Request) bool {
		if !down.Load() {
			return false
		}
		asked.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	handler, database := newTargetedMessageHandler(t, listener.URL)
	input := func() map[string]any {
		return map[string]any{"body": "Status?", "delivery": "btw", "idempotency_key": "before-the-outage"}
	}
	created := postAgentMessage(t, handler, "s1", input(), "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", created.Code, created.Body.String())
	}
	original := decodeBody[model.Message](t, created)

	down.Store(true)
	repeat := postAgentMessage(t, handler, "s1", input(), "alice")
	if repeat.Code != http.StatusOK {
		t.Fatalf("a repeat while the listener is down: status=%d body=%s, want 200", repeat.Code, repeat.Body.String())
	}
	if got := decodeBody[model.Message](t, repeat); got.ID != original.ID || len(got.Deliveries) != 1 || got.Deliveries[0].State != "sent" {
		t.Fatalf("the repeat answered %#v, want message %s with its one sent attempt", got, original.ID)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the repeat asked the listener %d times, want none", n)
	}

	id := commitKeyedMessage(t, database, "undelivered", "Still there?", "aside")
	retry := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Still there?", "delivery": "aside", "idempotency_key": "undelivered",
	}, "alice")
	if retry.Code != http.StatusOK {
		t.Fatalf("a retry of an undelivered send while the listener is down: status=%d body=%s, want 200", retry.Code, retry.Body.String())
	}
	got := decodeBody[model.Message](t, retry)
	if got.ID != id || len(got.Deliveries) != 1 || got.Deliveries[0].State != "failed" ||
		got.Deliveries[0].Error == nil || !strings.HasPrefix(*got.Deliveries[0].Error, "envoy listener: ") {
		t.Fatalf("the retry answered %#v, want message %s with one attempt failed on the listener", got, id)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want the original's one", got)
	}
}
