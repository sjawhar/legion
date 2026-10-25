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

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// agentSessions is the registry the keyed-send tests' fake listener answers: two sessions that
// take every mode.
const agentSessions = `[
	{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"],"last_seen":200},
	{"session_id":"s2","title":"reviewer","capabilities":["aside","btw","steer"],"last_seen":100}
]`

// agentMessagePattern is the single-session send's route.
const agentMessagePattern = "/api/v1/agents/{session_id}/messages"

// postAgentMessage sends one message to sessionID as login under a key of its own, as the dashboard
// sends every write; most tests have no interest in which.
func postAgentMessage(t *testing.T, handler http.Handler, sessionID string, input map[string]any, login string) *httptest.ResponseRecorder {
	t.Helper()
	return postAgentMessageKeyed(t, handler, sessionID, input, login, uuid.NewString())
}

// postAgentMessageKeyed sends one message to sessionID as login under key.
func postAgentMessageKeyed(t *testing.T, handler http.Handler, sessionID string, input map[string]any, login, key string) *httptest.ResponseRecorder {
	t.Helper()
	return keyedRequest(t, handler, http.MethodPost, "/api/v1/agents/"+sessionID+"/messages", input, login, key)
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

// commitKeyedMessage writes what alice's keyed send to s1 commits before its delivery starts - the
// message, and its key holding the message as its recovery point - and nothing else, as a process
// that died between the commit and the attempt leaves them: the key in flight, leased leasedAgo,
// or finished with status when status is set.
func commitKeyedMessage(t *testing.T, database *store.Store, key, body, delivery string, leasedAgo time.Duration, status *int) string {
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice"}
	author, err := json.Marshal(alice)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, target) values (null, $1, $2, 'session:s1')
		returning id::text
	`, author, body).Scan(&id); err != nil {
		t.Fatalf("commit the message: %v", err)
	}
	holdRequestKey(t, database, heldKey{
		login: "alice", key: key, pattern: agentMessagePattern,
		request: jsonRequest(t, http.MethodPost, "/api/v1/agents/s1/messages",
			map[string]any{"body": body, "delivery": delivery}, map[string]string{"session_id": "s1"}),
		leasedAgo: leasedAgo, status: status,
		recovery: agentMessageRecovery{MessageID: id, Delivery: delivery},
	})
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

// INFRA-258. A single-session send without an Idempotency-Key is sent, as any write without one
// is: it is only not replayed. The key is a header, so a body still naming one - a tab opened
// before the cutover, which sends no header - is a member the route does not take, and writes and
// sends nothing.
func TestASingleSessionSendWithoutAKeyIsSent(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	before := databaseFingerprint(t, database)

	bodyKey := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Status?", "delivery": "btw", "idempotency_key": "press-1",
	}, "alice")
	if code, text := refusalOf(t, bodyKey); bodyKey.Code != http.StatusBadRequest || code != "INVALID_JSON" ||
		!strings.Contains(text, "idempotency_key") {
		t.Fatalf("a body key = %d %s %q, want 400 INVALID_JSON naming idempotency_key", bodyKey.Code, code, text)
	}
	assertUnchanged(t, "a send with a body key", before, databaseFingerprint(t, database))
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("listener sends = %v, want none", got)
	}

	keyless := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Status?", "delivery": "btw",
	}, "alice")
	if keyless.Code != http.StatusCreated || replayed(keyless) {
		t.Fatalf("a keyless send: status=%d body=%s, want 201", keyless.Code, keyless.Body.String())
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want the keyless send's one", got)
	}
}

// A repeat of one send - a double press, a retry after a 5xx or a timeout - is answered the
// message the first request made, with its deliveries, and the session is handed it once.
func TestARepeatedKeyAnswersTheMessageItMadeAndDeliversOnce(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	send := func() *httptest.ResponseRecorder {
		t.Helper()
		return postAgentMessageKeyed(t, handler, "s1", map[string]any{
			"body": "Where is the dashboard?", "delivery": "aside",
		}, "alice", "press-1")
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
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("repeat: status=%d replayed=%v body=%s, want the original message replayed", repeat.Code, replayed(repeat), repeat.Body.String())
	}
	again := decodeBody[model.Message](t, repeat)
	if again.ID != original.ID || again.Body != original.Body {
		t.Fatalf("the repeat answered %#v, want the original %s", again, original.ID)
	}
	if len(again.Deliveries) != 1 || again.Deliveries[0].Attempt != 1 || again.Deliveries[0].State != "sent" {
		t.Fatalf("the repeat's deliveries = %#v, want the original's one sent attempt", again.Deliveries)
	}
	if messages := countMessages(t, database); messages != 1 {
		t.Fatalf("the database holds %d messages, want the one send", messages)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}
}

// A deploy's signal cancels Dispatch's Lifetime before http.Server.Shutdown drains the requests in
// flight, and Shutdown waits for each of them. A send the listener is still answering at that
// signal therefore finishes inside the drain: its attempt settles sent, and the page's retry under
// its key is answered with that message and hands the session nothing more.
func TestASendInFlightAtShutdownStillSettles(t *testing.T) {
	held := make(chan struct{})
	release := make(chan struct{})
	cut := make(chan struct{})
	var holding atomic.Bool
	listener, sends := newBroadcastListener(t, agentSessions, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/send" || !holding.CompareAndSwap(false, true) {
			return false
		}
		close(held)
		select {
		case <-release:
			return false
		case <-r.Context().Done():
			// Dispatch gave the send up: the listener never answers it.
			close(cut)
			return true
		}
	})
	lifetime, shutDown := context.WithCancel(t.Context())
	defer shutDown()
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, lifetime: lifetime})
	const body = "Ship it once the checks are green."
	send := func() *httptest.ResponseRecorder {
		return postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": body, "delivery": "steer"}, "alice", "press-1")
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- send() }()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the send never reached the listener")
	}
	// The deploy's signal, while the listener holds the send. A send cut off by it ends its
	// listener request at once; given that long, the listener then answers the send the drain
	// waits for.
	shutDown()
	select {
	case <-cut:
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	var created *httptest.ResponseRecorder
	select {
	case created = <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the send in flight at shutdown never answered")
	}

	var messageID string
	if err := database.Pool.QueryRow(context.Background(),
		`select id::text from messages where body = $1`, body,
	).Scan(&messageID); err != nil {
		t.Fatalf("read the send's message: %v", err)
	}
	if attempts := messageAttempts(t, database, messageID); len(attempts) != 1 || attempts[0].State != "sent" {
		t.Fatalf("the message's attempts = %#v, want its one attempt settled sent inside the drain", attempts)
	}
	if created.Code != http.StatusCreated {
		t.Fatalf("the send in flight at shutdown: status=%d body=%s, want 201", created.Code, created.Body.String())
	}
	if message := decodeBody[model.Message](t, created); len(message.Deliveries) != 1 || message.Deliveries[0].State != "sent" {
		t.Fatalf("the send's deliveries = %#v, want its one sent attempt", message.Deliveries)
	}

	retry := send()
	if retry.Code != http.StatusCreated || !replayed(retry) {
		t.Fatalf("the retry under the key: status=%d body=%s, want the message replayed", retry.Code, retry.Body.String())
	}
	again := decodeBody[model.Message](t, retry)
	if again.ID != messageID || len(again.Deliveries) != 1 || again.Deliveries[0].Attempt != 1 ||
		again.Deliveries[0].State != "sent" {
		t.Fatalf("the retry answered %#v, want message %s with its one sent attempt", again, messageID)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want the one send and no second", got)
	}
	if attempts := messageAttempts(t, database, messageID); len(attempts) != 1 {
		t.Fatalf("the message's attempts = %#v, want the one", attempts)
	}
}

// A key names one request. Reused for a different one - another message, mode, session or reply -
// it is refused 422 naming the send that used it, and nothing is written or sent: that message
// says something else, so answering with it would tell the caller a message it never sent had
// gone out.
func TestAKeyReusedForAnotherMessageIsRefused(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": "Status?", "delivery": "btw"}, "alice", "reused")
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
			response := postAgentMessageKeyed(t, handler, changed.session, changed.input, "alice", "reused")
			code, text := refusalOf(t, response)
			if response.Code != http.StatusUnprocessableEntity || code != contracts.IdempotencyKeyReused ||
				!strings.Contains(text, "POST /api/v1/agents/s1/messages") || !strings.Contains(text, "this request was not run") {
				t.Fatalf("status=%d code=%s error=%q, want 422 %s naming the first send", response.Code, code, text, contracts.IdempotencyKeyReused)
			}
		})
	}
	assertUnchanged(t, "a reused key", before, databaseFingerprint(t, database))
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want the original's one and no other", got)
	}
}

// A retry racing its original, sixteen times over, all held at the key's insert until the last of
// them has arrived there: one makes the message and every other is told the first is in flight or,
// once it has answered, given its answer. The session is handed the message once.
func TestConcurrentSendsUnderOneKeyMakeOneMessage(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table request_keys in share mode`); err != nil {
		t.Fatalf("lock the key table: %v", err)
	}

	const senders = 16
	responses := make(chan *httptest.ResponseRecorder, senders)
	for range senders {
		go func() {
			responses <- postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": "Status?", "delivery": "btw"}, "alice", "race")
		}()
	}
	// Sixteen connections, less the holder's: every request that got one waits at the insert, and
	// the last waits for a connection.
	waitForDatabaseLocks(t, holder, senders-1)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the key table: %v", err)
	}

	fresh := 0
	ids := map[string]struct{}{}
	for range senders {
		select {
		case response := <-responses:
			switch {
			case response.Code == http.StatusCreated:
				if !replayed(response) {
					fresh++
				}
				ids[decodeBody[model.Message](t, response).ID] = struct{}{}
			case response.Code == http.StatusConflict:
				if code, _ := refusalOf(t, response); code != contracts.IdempotencyKeyInFlight {
					t.Errorf("409 %s, want %s", code, contracts.IdempotencyKeyInFlight)
				}
			default:
				t.Errorf("status=%d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a concurrent send did not answer")
		}
	}
	if fresh != 1 || len(ids) != 1 {
		t.Fatalf("%d fresh answers naming %d messages, want one of each", fresh, len(ids))
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
		return postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": "Status?", "delivery": "btw"}, login, "shared")
	}

	alices := send("alice")
	if alices.Code != http.StatusCreated {
		t.Fatalf("alice's send: status=%d body=%s", alices.Code, alices.Body.String())
	}
	original := decodeBody[model.Message](t, alices).ID
	bobs := send("bob")
	if bobs.Code != http.StatusCreated || replayed(bobs) {
		t.Fatalf("bob's send under alice's key: status=%d body=%s, want a message of his own", bobs.Code, bobs.Body.String())
	}
	if got := decodeBody[model.Message](t, bobs).ID; got == original {
		t.Fatalf("bob's send answered alice's message %s", original)
	}
	again := send("Alice")
	if !replayed(again) {
		t.Fatalf("Alice's repeat: status=%d body=%s, want alice's message replayed", again.Code, again.Body.String())
	}
	if got := decodeBody[model.Message](t, again).ID; got != original {
		t.Fatalf("Alice's repeat answered %s, want alice's message %s", got, original)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1", "s1"}) {
		t.Fatalf("listener sends = %v, want alice's and bob's", got)
	}
}

// The page gives up on a send - a closed tab, its own deadline - after the message committed,
// while the listener still holds the frame. The delivery runs on past the request, the key keeps
// its answer, and the page's retry under the key is answered the message, sending nothing again.
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

	input := map[string]any{"body": "Deploy now.", "delivery": "steer"}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/agents/s1/messages", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, "abort-1")
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

	retry := postAgentMessageKeyed(t, handler, "s1", input, "alice", "abort-1")
	if retry.Code != http.StatusCreated || !replayed(retry) {
		t.Fatalf("the page's retry: status=%d body=%s, want the message replayed", retry.Code, retry.Body.String())
	}
	if got := decodeBody[model.Message](t, retry); got.ID != id || len(got.Deliveries) != 1 || got.Deliveries[0].State != "sent" {
		t.Fatalf("the retry answered %#v, want message %s with its one sent attempt", got, id)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}
}

// A process that died between a send's commit and its answer leaves the message with no attempt
// at all and its key in flight, holding the message as its recovery point. A retry inside the lease
// is told the first is in flight and sends nothing; the first past it starts the delivery, in the
// message's own mode, rather than making the message again, and a later repeat finds it on its way
// and sends nothing more.
func TestARetryOfAMessageCommittedWithoutAnAnswerDeliversIt(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	id := commitKeyedMessage(t, database, "undelivered", "Still there?", "aside", 0, nil)
	retry := func() *httptest.ResponseRecorder {
		t.Helper()
		return postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": "Still there?", "delivery": "aside"}, "alice", "undelivered")
	}

	if inFlight := retry(); inFlight.Code != http.StatusConflict {
		t.Fatalf("a retry inside the lease: status=%d body=%s, want 409 %s", inFlight.Code, inFlight.Body.String(), contracts.IdempotencyKeyInFlight)
	}
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("a retry inside the lease sent %v, want nothing", got)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update request_keys set leased_at = now() - interval '2 minutes'`); err != nil {
		t.Fatalf("lapse the lease: %v", err)
	}
	resumed := retry()
	if resumed.Code != http.StatusCreated || !replayed(resumed) {
		t.Fatalf("a retry past the lease: status=%d body=%s, want the message replayed", resumed.Code, resumed.Body.String())
	}
	got := decodeBody[model.Message](t, resumed)
	if got.ID != id || len(got.Deliveries) != 1 || got.Deliveries[0].Attempt != 1 ||
		got.Deliveries[0].State != "sent" || got.Deliveries[0].Delivery != "aside" || got.Deliveries[0].SessionID != "s1" {
		t.Fatalf("the retry answered %#v, want message %s with attempt 1 sent to s1 as an aside", got, id)
	}
	if keys := keysOf(sends); !equalStrings(keys, []string{id + ":aside"}) {
		t.Fatalf("listener sends = %v, want the message once, in its own mode", keys)
	}
	if again := decodeBody[model.Message](t, retry()); len(again.Deliveries) != 1 {
		t.Fatalf("a second retry's deliveries = %#v, want the one attempt", again.Deliveries)
	}
	if messages := countMessages(t, database); messages != 1 {
		t.Fatalf("the database holds %d messages, want the one", messages)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}
}

// A sender that claimed a send's attempt and never came back leaves it pending, though the send
// itself was answered. While its claim is live it may still be sending, and a retry under the key
// leaves it to that sender; once the claim has lapsed the retry resumes the attempt under its own
// number, and so its own delivery key, as a same-mode retry from the agent card does - an attempt
// stranded before anything was resolved included, which takes the message's session.
func TestARetryOfAStrandedSendResumesItsAttempt(t *testing.T) {
	created := http.StatusCreated
	for name, pinned := range map[string]string{
		"an attempt naming its session":        "s1",
		"an attempt stranded before resolving": "",
	} {
		t.Run(name, func(t *testing.T) {
			listener, sends := newBroadcastListener(t, agentSessions)
			handler, database := newTargetedMessageHandler(t, listener.URL)
			id := commitKeyedMessage(t, database, "stranded", "Status?", "steer", 0, &created)
			strandAttempt(t, database, id, "steer", pinned, 0)
			retry := func() model.Message {
				t.Helper()
				response := postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": "Status?", "delivery": "steer"}, "alice", "stranded")
				if response.Code != http.StatusCreated || !replayed(response) {
					t.Fatalf("retry: status=%d body=%s, want the message replayed", response.Code, response.Body.String())
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
	input := map[string]any{"body": "Status?", "delivery": "btw"}
	created := postAgentMessageKeyed(t, handler, "s1", input, "alice", "before-the-outage")
	if created.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", created.Code, created.Body.String())
	}
	original := decodeBody[model.Message](t, created)

	down.Store(true)
	repeat := postAgentMessageKeyed(t, handler, "s1", input, "alice", "before-the-outage")
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("a repeat while the listener is down: status=%d body=%s, want the message replayed", repeat.Code, repeat.Body.String())
	}
	if got := decodeBody[model.Message](t, repeat); got.ID != original.ID || len(got.Deliveries) != 1 || got.Deliveries[0].State != "sent" {
		t.Fatalf("the repeat answered %#v, want message %s with its one sent attempt", got, original.ID)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the repeat asked the listener %d times, want none", n)
	}

	id := commitKeyedMessage(t, database, "undelivered", "Still there?", "aside", 2*time.Minute, nil)
	retry := postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": "Still there?", "delivery": "aside"}, "alice", "undelivered")
	if retry.Code != http.StatusCreated || !replayed(retry) {
		t.Fatalf("a retry of an undelivered send while the listener is down: status=%d body=%s, want the message replayed", retry.Code, retry.Body.String())
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

// A send whose key a repeat took over while it was slow commits nothing: its message and its
// recovery point roll back together, and the repeat that holds the key makes the send instead.
func TestASendThatLostItsKeyCommitsNothing(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	_, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	holdRequestKey(t, database, heldKey{
		login: "alice", key: "slow", pattern: agentMessagePattern,
		request: jsonRequest(t, http.MethodPost, "/api/v1/agents/s1/messages",
			map[string]any{"body": "Status?", "delivery": "btw"}, map[string]string{"session_id": "s1"}),
	})
	// The repeat past the lease took the key over: the slow holder still holds attempt 1.
	if _, err := database.Pool.Exec(context.Background(), `update request_keys set attempt = 2`); err != nil {
		t.Fatalf("take the key over: %v", err)
	}
	slow := context.WithValue(store.WithTransactionTracking(context.Background()), requestKeyClaimKey{},
		requestKeyClaim{login: "alice", key: "slow", attempt: 1})
	target := "session:s1"
	delivery := "btw"
	_, _, err := directServer(deps).createStoredMessage(slow, nil,
		createMessageInput{Body: "Status?", Target: &target, Delivery: &delivery}, "btw",
		model.Actor{Kind: "user", ID: "alice"}, true)
	if err != errRequestKeyTakenOver {
		t.Fatalf("the slow holder's write = %v, want %v", err, errRequestKeyTakenOver)
	}
	if messages := countMessages(t, database); messages != 0 {
		t.Fatalf("the database holds %d messages, want none", messages)
	}
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("listener sends = %v, want none", got)
	}
	var recovery *string
	if err := database.Pool.QueryRow(context.Background(), `select recovery::text from request_keys`).Scan(&recovery); err != nil || recovery != nil {
		t.Fatalf("the key's recovery point = %v (%v), want none", recovery, err)
	}
}

// A send that committed its message and then failed - a 5xx, a panic - releases its key with its
// recovery point kept, so the next attempt is answered from the message at once, not refused as in
// flight and never made twice. A request that committed nothing has its key deleted instead.
func TestAFailureAfterTheSendCommittedIsAnsweredFromItsMessage(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	id := commitKeyedMessage(t, database, "failed-after-commit", "Status?", "btw", 0, nil)
	directServer(deps).releaseRequestKey(context.Background(), requestKeyClaim{login: "alice", key: "failed-after-commit", attempt: 1})
	if rows := countRequestKeys(t, database); rows != 1 {
		t.Fatalf("the release left %d key rows, want the one keeping its recovery point", rows)
	}
	retry := postAgentMessageKeyed(t, handler, "s1", map[string]any{"body": "Status?", "delivery": "btw"}, "alice", "failed-after-commit")
	if retry.Code != http.StatusCreated || !replayed(retry) {
		t.Fatalf("the next attempt: status=%d body=%s, want the message replayed", retry.Code, retry.Body.String())
	}
	if got := decodeBody[model.Message](t, retry); got.ID != id || len(got.Deliveries) != 1 || got.Deliveries[0].State != "sent" {
		t.Fatalf("the next attempt answered %#v, want message %s sent once", got, id)
	}
	if messages := countMessages(t, database); messages != 1 {
		t.Fatalf("the database holds %d messages, want the one", messages)
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}

	holdRequestKey(t, database, heldKey{
		login: "alice", key: "failed-before-commit", pattern: agentMessagePattern,
		request: jsonRequest(t, http.MethodPost, "/api/v1/agents/s1/messages",
			map[string]any{"body": "Status?", "delivery": "btw"}, map[string]string{"session_id": "s1"}),
	})
	directServer(deps).releaseRequestKey(context.Background(), requestKeyClaim{login: "alice", key: "failed-before-commit", attempt: 1})
	if rows := countRequestKeys(t, database); rows != 1 {
		t.Fatalf("after releasing a key that committed nothing there are %d key rows, want only the first", rows)
	}
}
