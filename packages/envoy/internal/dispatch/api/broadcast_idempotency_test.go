package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// LEGION-446. A repeat of one send - the same human, key and request, as a double press, a
// proxy's retry or the Sends strip's Retry makes it - is answered the broadcast the first
// request made, so every recipient is handed the message once rather than twice.
func TestBroadcastRepeatWithItsKeyAnswersTheOriginalAndDeliversOnce(t *testing.T) {
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	send := func() *httptest.ResponseRecorder {
		t.Helper()
		// The reviewer advertises aside alone, so a steer excludes it.
		return postBroadcast(t, handler, map[string]any{
			"body": "Status?", "delivery": "steer", "session_ids": []string{"planner", "tester", "reviewer"},
			"idempotency_key": "press-1",
		}, "alice")
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
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat: status=%d body=%s, want 200 with the original broadcast", repeat.Code, repeat.Body.String())
	}
	replayed := decodeBody[broadcastResponse](t, repeat)
	if replayed.ID != original.ID {
		t.Fatalf("the repeat answered broadcast %s, want the original %s", replayed.ID, original.ID)
	}
	if got := sendTargets(replayed); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("the repeat's recipients = %v", got)
	}
	if len(replayed.Excluded) != 1 || replayed.Excluded[0].SessionID != "reviewer" ||
		replayed.Excluded[0].Reason != "excluded by the original send" {
		t.Fatalf("the repeat's exclusions = %#v, want the reviewer the original left out", replayed.Excluded)
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
		return postBroadcast(t, handler, map[string]any{
			"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"}, "idempotency_key": key,
		}, "alice")
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
	if repeat.Code != http.StatusOK {
		t.Fatalf("a repeat while the listener is down: status=%d body=%s, want 200", repeat.Code, repeat.Body.String())
	}
	if got := decodeBody[broadcastResponse](t, repeat).ID; got != original.ID {
		t.Fatalf("the repeat answered broadcast %s, want the original %s", got, original.ID)
	}
}

// A key names one request. Reused for a different one, it is refused and nothing is sent: the
// key's broadcast says something else, so answering with it would tell the caller a message it
// never sent had gone out.
func TestBroadcastKeyReusedForADifferentRequestIsRefused(t *testing.T) {
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"}, "idempotency_key": "reused",
	}, "alice")
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
			changed["idempotency_key"] = "reused"
			response := postBroadcast(t, handler, changed, "alice")
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", response.Code, response.Body.String())
			}
			refusal := decodeBody[struct {
				Code        string `json:"code"`
				Error       string `json:"error"`
				BroadcastID string `json:"broadcast_id"`
			}](t, response)
			if refusal.Code != "BROADCAST_KEY_REUSED" || refusal.BroadcastID != original.ID ||
				!strings.Contains(refusal.Error, "this request was not sent") {
				t.Fatalf("refusal = %#v, want BROADCAST_KEY_REUSED naming %s and saying nothing was sent", refusal, original.ID)
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
		return postBroadcast(t, handler, map[string]any{
			"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"}, "idempotency_key": "shared",
		}, login)
	}

	alices := send("alice")
	if alices.Code != http.StatusCreated {
		t.Fatalf("alice's send: status=%d body=%s", alices.Code, alices.Body.String())
	}
	original := decodeBody[broadcastResponse](t, alices).ID
	bobs := send("bob")
	if bobs.Code != http.StatusCreated {
		t.Fatalf("bob's send under alice's key: status=%d body=%s, want a broadcast of his own", bobs.Code, bobs.Body.String())
	}
	if got := decodeBody[broadcastResponse](t, bobs).ID; got == original {
		t.Fatalf("bob's send answered alice's broadcast %s", original)
	}
	again := send("Alice")
	if again.Code != http.StatusOK {
		t.Fatalf("Alice's repeat: status=%d body=%s, want 200", again.Code, again.Body.String())
	}
	if got := decodeBody[broadcastResponse](t, again).ID; got != original {
		t.Fatalf("Alice's repeat answered %s, want alice's broadcast %s", got, original)
	}
}

// A retry racing its original: two requests carrying one key, both past the replay check before
// either writes. The key's primary key makes the second wait for the first and roll back, so one
// broadcast is written and each recipient is handed one frame.
func TestConcurrentBroadcastsWithOneKeyMakeOneBroadcast(t *testing.T) {
	var reads atomic.Int32
	var both sync.WaitGroup
	both.Add(2)
	listener, sends := newBroadcastListener(t, broadcastSessions, func(_ http.ResponseWriter, r *http.Request) bool {
		// Only the two creates meet here: each recipient's delivery reads the registry again
		// after the 201 (envoy_resolve.go), and a third Done would drive the group negative.
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" && reads.Add(1) <= 2 {
			both.Done()
			both.Wait()
		}
		return false
	})
	handler, database := newTargetedMessageHandler(t, listener.URL)

	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			responses <- postBroadcast(t, handler, map[string]any{
				"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"}, "idempotency_key": "race",
			}, "alice")
		}()
	}
	var codes []int
	ids := map[string]struct{}{}
	for range 2 {
		select {
		case response := <-responses:
			codes = append(codes, response.Code)
			if response.Code == http.StatusOK || response.Code == http.StatusCreated {
				ids[decodeBody[broadcastResponse](t, response).ID] = struct{}{}
			} else {
				t.Errorf("status=%d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a concurrent send did not answer")
		}
	}
	sort.Ints(codes)
	if len(codes) != 2 || codes[0] != http.StatusOK || codes[1] != http.StatusCreated {
		t.Fatalf("status codes = %v, want one 201 and one 200", codes)
	}
	if len(ids) != 1 {
		t.Fatalf("the two answers name %d broadcasts, want one", len(ids))
	}
	if listed := broadcastCount(t, handler); listed != 1 {
		t.Fatalf("the broadcast list holds %d broadcasts, want one", listed)
	}
	if messages := countMessages(t, database); messages != 2 {
		t.Fatalf("the database holds %d messages, want one per recipient", messages)
	}
	for id := range ids {
		awaitBroadcastDeliveries(t, handler, id)
	}
	if got := sends.targets(); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("listener sends = %v, want one frame per recipient", got)
	}
}
