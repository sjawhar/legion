package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// countMessages counts every message the test's database holds: a repeat that wrote a second
// broadcast shows here whichever broadcast it wrote.
func countMessages(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from messages`).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return count
}

// broadcastCount reads the broadcast list as the Sends strip's owner would.
func broadcastCount(t *testing.T, handler http.Handler) int {
	t.Helper()
	return len(decodeBody[[]json.RawMessage](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts", nil, "alice")))
}

// LEGION-446, INFRA-258. A repeat of one send under its Idempotency-Key - a double press, a
// proxy's retry or the Sends strip's Retry - is answered the broadcast the first request made,
// with the first answer's exclusions, so every recipient is handed the message once rather than
// twice.
func TestBroadcastRepeatWithItsKeyAnswersTheOriginalAndDeliversOnce(t *testing.T) {
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	send := func() *httptest.ResponseRecorder {
		t.Helper()
		// The reviewer advertises aside alone, so a steer excludes it.
		return postBroadcastKeyed(t, handler, map[string]any{
			"body": "Status?", "delivery": "steer", "session_ids": []string{"planner", "tester", "reviewer"},
		}, "alice", "press-1")
	}

	first := send()
	if first.Code != http.StatusCreated {
		t.Fatalf("first send: status=%d body=%s", first.Code, first.Body.String())
	}
	original := decodeBody[broadcastResponse](t, first)
	if got := sendTargets(original); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("first send's recipients = %v", got)
	}
	if len(original.Excluded) != 1 || original.Excluded[0].SessionID != "reviewer" ||
		original.Excluded[0].Reason != "does not advertise steer" {
		t.Fatalf("first send's exclusions = %#v", original.Excluded)
	}

	repeat := send()
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("repeat: status=%d body=%s, want the original broadcast replayed", repeat.Code, repeat.Body.String())
	}
	again := decodeBody[broadcastResponse](t, repeat)
	if again.ID != original.ID {
		t.Fatalf("the repeat answered broadcast %s, want the original %s", again.ID, original.ID)
	}
	if got := sendTargets(again); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("the repeat's recipients = %v", got)
	}
	if len(again.Excluded) != 1 || again.Excluded[0] != original.Excluded[0] {
		t.Fatalf("the repeat's exclusions = %#v, want the original's %#v", again.Excluded, original.Excluded)
	}

	if listed := broadcastCount(t, handler); listed != 1 {
		t.Fatalf("the broadcast list holds %d broadcasts, want the one send", listed)
	}
	awaitBroadcastDeliveries(t, handler, original.ID)
	if got := sends.targets(); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("listener sends = %v, want one frame per recipient", got)
	}
	if messages := countMessages(t, database); messages != 2 {
		t.Fatalf("the database holds %d messages, want one per recipient", messages)
	}
}

// A retry of a send that landed is answered from the database before the registry is read, so
// it is answered even while the listener is down, rather than refused as unreachable.
func TestBroadcastRepeatIsAnsweredWhileTheListenerIsDown(t *testing.T) {
	var down atomic.Bool
	listener, _ := newBroadcastListener(t, broadcastSessions, func(w http.ResponseWriter, _ *http.Request) bool {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return down.Load()
	})
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	send := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		return postBroadcastKeyed(t, handler, map[string]any{
			"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"},
		}, "alice", key)
	}

	created := send("before-the-outage")
	if created.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", created.Code, created.Body.String())
	}
	original := decodeBody[broadcastResponse](t, created)
	awaitBroadcastDeliveries(t, handler, original.ID)

	down.Store(true)
	// A send nobody has made yet cannot be judged without the registry: the listener is down.
	if fresh := send("during-the-outage"); fresh.Code != http.StatusServiceUnavailable ||
		!strings.Contains(fresh.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
		t.Fatalf("a new send while the listener is down: status=%d body=%s, want 503 ENVOY_UNAVAILABLE",
			fresh.Code, fresh.Body.String())
	}
	repeat := send("before-the-outage")
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("a repeat while the listener is down: status=%d body=%s, want the broadcast replayed", repeat.Code, repeat.Body.String())
	}
	if got := decodeBody[broadcastResponse](t, repeat).ID; got != original.ID {
		t.Fatalf("the repeat answered broadcast %s, want the original %s", got, original.ID)
	}
}

// A key names one request. Reused for a different one, it is refused 422 and nothing is sent:
// the key's broadcast says something else, so answering with it would tell the caller a message
// it never sent had gone out.
func TestBroadcastKeyReusedForADifferentRequestIsRefused(t *testing.T) {
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := postBroadcastKeyed(t, handler, map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"},
	}, "alice", "reused")
	if created.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", created.Code, created.Body.String())
	}
	original := decodeBody[broadcastResponse](t, created)
	awaitBroadcastDeliveries(t, handler, original.ID)

	for name, changed := range map[string]map[string]any{
		"another message":    {"body": "Stand down.", "delivery": "btw", "session_ids": []string{"planner", "tester"}},
		"another mode":       {"body": "Status?", "delivery": "steer", "session_ids": []string{"planner", "tester"}},
		"sessions reordered": {"body": "Status?", "delivery": "btw", "session_ids": []string{"tester", "planner"}},
	} {
		t.Run(name, func(t *testing.T) {
			response := postBroadcastKeyed(t, handler, changed, "alice", "reused")
			code, text := refusalOf(t, response)
			if response.Code != http.StatusUnprocessableEntity || code != contracts.IdempotencyKeyReused ||
				!strings.Contains(text, "POST /api/v1/broadcasts") || !strings.Contains(text, "this request was not run") {
				t.Fatalf("status=%d code=%s error=%q, want 422 %s naming the first send", response.Code, code, text, contracts.IdempotencyKeyReused)
			}
		})
	}
	if listed := broadcastCount(t, handler); listed != 1 {
		t.Fatalf("the broadcast list holds %d broadcasts, want the one send", listed)
	}
	if messages := countMessages(t, database); messages != 2 {
		t.Fatalf("the database holds %d messages, want the original's two", messages)
	}
	if got := sends.targets(); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("listener sends = %v, want the original's two and no other", got)
	}
}

// A key belongs to the human who sent it. Another person's identical send under the same key is
// another send, since a broadcast carries one author; the same person under another casing of
// the identity header is the same human, since every identity names a person by lowercase email.
func TestBroadcastKeysAreScopedToTheSender(t *testing.T) {
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	send := func(login string) *httptest.ResponseRecorder {
		t.Helper()
		return postBroadcastKeyed(t, handler, map[string]any{
			"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"},
		}, login, "shared")
	}

	alices := send("alice")
	if alices.Code != http.StatusCreated {
		t.Fatalf("alice's send: status=%d body=%s", alices.Code, alices.Body.String())
	}
	original := decodeBody[broadcastResponse](t, alices).ID
	bobs := send("bob")
	if bobs.Code != http.StatusCreated || replayed(bobs) {
		t.Fatalf("bob's send under alice's key: status=%d body=%s, want a broadcast of his own", bobs.Code, bobs.Body.String())
	}
	if got := decodeBody[broadcastResponse](t, bobs).ID; got == original {
		t.Fatalf("bob's send answered alice's broadcast %s", original)
	}
	again := send("Alice")
	if !replayed(again) {
		t.Fatalf("Alice's repeat: status=%d body=%s, want alice's broadcast replayed", again.Code, again.Body.String())
	}
	if got := decodeBody[broadcastResponse](t, again).ID; got != original {
		t.Fatalf("Alice's repeat answered %s, want alice's broadcast %s", got, original)
	}
}

// A retry racing its original: the first holds the key while it reads the registry, so the
// retry is told it is in flight and writes nothing; once the first has answered, a retry is its
// answer. One broadcast is written and each recipient is handed one frame.
func TestConcurrentBroadcastsWithOneKeyMakeOneBroadcast(t *testing.T) {
	reading, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	var reads atomic.Int32
	listener, sends := newBroadcastListener(t, broadcastSessions, func(_ http.ResponseWriter, r *http.Request) bool {
		// The first create's first per-session registry read waits; the create's other read and
		// each recipient's delivery read after the 201 (envoy_resolve.go) go through.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/") && reads.Add(1) == 1 {
			close(reading)
			<-release
		}
		return false
	})
	// Registered after the listener's close, so it runs first and a failed test never leaves the
	// held read blocking that close.
	t.Cleanup(releaseOnce)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	send := func() *httptest.ResponseRecorder {
		return postBroadcastKeyed(t, handler, map[string]any{
			"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"},
		}, "alice", "race")
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- send() }()
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the first send never read the registry")
	}
	retry := send()
	if code, _ := refusalOf(t, retry); retry.Code != http.StatusConflict || code != contracts.IdempotencyKeyInFlight {
		t.Fatalf("a retry while the first runs: status=%d body=%s, want 409 %s", retry.Code, retry.Body.String(), contracts.IdempotencyKeyInFlight)
	}
	releaseOnce()
	first := awaitResponse(t, answered)
	if first.Code != http.StatusCreated || replayed(first) {
		t.Fatalf("the first send: status=%d body=%s", first.Code, first.Body.String())
	}
	id := decodeBody[broadcastResponse](t, first).ID
	if later := send(); !replayed(later) || decodeBody[broadcastResponse](t, later).ID != id {
		t.Fatalf("a retry once answered: status=%d body=%s, want broadcast %s replayed", later.Code, later.Body.String(), id)
	}
	if listed := broadcastCount(t, handler); listed != 1 {
		t.Fatalf("the broadcast list holds %d broadcasts, want one", listed)
	}
	if messages := countMessages(t, database); messages != 2 {
		t.Fatalf("the database holds %d messages, want one per recipient", messages)
	}
	awaitBroadcastDeliveries(t, handler, id)
	if got := sends.targets(); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("listener sends = %v, want one frame per recipient", got)
	}
}

// A deploy, or a crash, can stop the process that answered a send while recipients of it are
// still being delivered: their attempts stay pending under a claim nobody holds. Nothing else
// resumes them - the broadcast's page only ever repeats the send - so the repeat puts every
// recipient the broadcast still owes on its way before it answers, as a repeat of a single-session
// send does (sendHeldMessageOn). It hands no recipient a second frame: not the one already
// delivered, and not the one a live sender is delivering right now.
func TestBroadcastRepeatResumesEveryOwedRecipientAndDeliversEachOnce(t *testing.T) {
	const body = "Status before the deploy?"
	requested := []string{"planner", "tester", "reviewer", "archivist"}
	listener, sends := newBroadcastListener(t, broadcastSessionsJSON(t, requested))
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	direct := directServer(deps)
	actor := model.Actor{Kind: "user", ID: "alice"}
	ctx := store.WithTransactionTracking(context.Background())

	// The send as the stopped process left it: writeBroadcast commits the broadcast and one
	// pending, unclaimed attempt per recipient, the key holds the broadcast as its recovery point
	// and the 201 the send answered, and the delivery it answers runs behind the response - so
	// this is the state of a send whose process never got that far.
	targets := make([]broadcastTarget, 0, len(requested))
	for _, sessionID := range requested {
		targets = append(targets, broadcastTarget{sessionID: sessionID, title: sessionID})
	}
	written, _, err := direct.writeBroadcast(ctx, actor, body, "btw", targets, []broadcastExclusion{})
	if err != nil {
		t.Fatalf("write the broadcast whose process stopped mid-delivery: %v", err)
	}
	input := map[string]any{"body": body, "delivery": "btw", "session_ids": requested}
	created := http.StatusCreated
	holdRequestKey(t, database, heldKey{
		login: "alice", key: "press-1", pattern: "/api/v1/broadcasts",
		request: jsonRequest(t, http.MethodPost, "/api/v1/broadcasts", input, nil),
		status:  &created, recovery: broadcastRecovery{BroadcastID: written.ID, Excluded: []broadcastExclusion{}},
	})
	recipient := func(sessionID string) model.Message {
		t.Helper()
		for _, sent := range written.Recipients {
			if sent.SessionID == sessionID {
				return sent.Message
			}
		}
		t.Fatalf("the broadcast has no recipient %q", sessionID)
		return model.Message{}
	}
	claimed := func(sessionID, ago string) {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(),
			`update message_deliveries set claimed_at = now() - $2::interval where message_id = $1`,
			recipient(sessionID).ID, ago); err != nil {
			t.Fatalf("claim %s's attempt %s ago: %v", sessionID, ago, err)
		}
	}
	// The planner's frame landed before the process stopped.
	if _, err := direct.deliverMessageResuming(ctx, recipient("planner"), "btw", nil, actor, nil, 1); err != nil {
		t.Fatalf("deliver the one recipient the process reached: %v", err)
	}
	// The tester's sender claimed its attempt and died inside the send: the claim has lapsed.
	claimed("tester", "5 minutes")
	// The archivist's sender is alive and inside its claim's lease, so that attempt is its own.
	claimed("archivist", "0 seconds")
	// The reviewer's attempt was never claimed at all: the write opened it and nothing took it.

	repeat := postBroadcastKeyed(t, handler, input, "alice", "press-1")
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("the repeat: status=%d body=%s, want the broadcast replayed", repeat.Code, repeat.Body.String())
	}
	again := decodeBody[broadcastResponse](t, repeat)
	if again.ID != written.ID {
		t.Fatalf("the repeat answered broadcast %s, want the original %s", again.ID, written.ID)
	}

	settled := make([]string, 0, len(requested))
	for _, sent := range again.Recipients {
		for _, attempt := range sent.Message.Deliveries {
			settled = append(settled, fmt.Sprintf("%s %d %s", sent.SessionID, attempt.Attempt, attempt.State))
		}
	}
	sort.Strings(settled)
	// The archivist is left to the sender that holds it; every other recipient is settled by
	// the time the repeat answers, since the page reads a pending recipient as a send it must
	// ask about again.
	if want := []string{"archivist 1 pending", "planner 1 sent", "reviewer 1 sent", "tester 1 sent"}; !equalStrings(settled, want) {
		t.Fatalf("the repeat answered %v, want %v", settled, want)
	}
	if got := sends.targets(); !equalStrings(got, []string{"planner", "reviewer", "tester"}) {
		t.Fatalf("listener sends = %v, want the planner's first frame and one each for the two owed", got)
	}
	if messages := countMessages(t, database); messages != len(requested) {
		t.Fatalf("the database holds %d messages, want one per recipient", messages)
	}
	var attempts int
	if err := database.Pool.QueryRow(ctx, `select count(*) from message_deliveries`).Scan(&attempts); err != nil {
		t.Fatalf("count the attempts: %v", err)
	}
	if attempts != len(requested) {
		t.Fatalf("the database holds %d attempts, want the one the write opened per recipient", attempts)
	}
}
