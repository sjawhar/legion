package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The routes that write a session's interest record answer over a record this build cannot decode
// (store.ErrMalformedInterest) instead of failing on the read, which every retry would repeat:
// DELETE /v1/interests/<id> deletes it (204), POST /v1/interests/subscribe replaces it with the
// topics the subscribe names (200) and the registry logs that at WARN naming the session, and POST
// /v1/interests/unsubscribe answers 200 and leaves it as it is.
func TestTheInterestWriteRoutesAnswerOverAnUnreadableInterestRecord(t *testing.T) {
	const unreadableRecord = "not a json interest record"
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	logs := &lockedBuffer{}
	registry, err := store.Open(client.Conn, store.WithReplicas(1),
		store.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	interestBucket, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, registry: registry, sessions: sessions}, "test-machine", logging.New("test"))
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	storeUnreadable := func(t *testing.T, sessionID string) {
		t.Helper()
		if _, err := interestBucket.Put(sessionID, []byte(unreadableRecord)); err != nil {
			t.Fatalf("write %s's unreadable interest record: %v", sessionID, err)
		}
	}

	t.Run("DELETE /v1/interests/<id>", func(t *testing.T) {
		const sessionID = "ses_unreadable_delete"
		storeUnreadable(t, sessionID)
		if recorder := serve(http.MethodDelete, "/v1/interests/"+sessionID, ""); recorder.Code != http.StatusNoContent {
			t.Fatalf("status %d, body %s; want 204", recorder.Code, recorder.Body.String())
		}
		if entry, err := interestBucket.Get(sessionID); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("the bucket's %s key after the delete = %v, %v; want ErrKeyNotFound", sessionID, entry, err)
		}
	})

	t.Run("POST /v1/interests/subscribe", func(t *testing.T) {
		const (
			sessionID = "ses_unreadable_subscribe"
			topic     = "notifications.example.subscribed"
		)
		storeUnreadable(t, sessionID)
		before := len(logs.String())
		recorder := serve(http.MethodPost, "/v1/interests/subscribe", `{"session_id":"`+sessionID+`","topics":["`+topic+`"]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status %d, body %s; want 200", recorder.Code, recorder.Body.String())
		}
		entry, err := interestBucket.Get(sessionID)
		if err != nil {
			t.Fatalf("read %s's record after the subscribe: %v", sessionID, err)
		}
		var stored store.Interest
		if err := json.Unmarshal(entry.Value(), &stored); err != nil {
			t.Fatalf("the record the subscribe wrote does not decode: %q: %v", entry.Value(), err)
		}
		want := contracts.AgentSubject(sessionID) + " " + topic
		if got := strings.Join(stored.Topics, " "); got != want || stored.MachineID != "test-machine" {
			t.Fatalf("the record the subscribe wrote = %+v, want topics %q on test-machine", stored, want)
		}
		var replaced int
		for _, line := range strings.Split(strings.TrimSpace(logs.String()[before:]), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "WARN" &&
				record["msg"] == "registry replaced an interest record it cannot decode" && record["session_id"] == sessionID {
				replaced++
			}
		}
		if replaced != 1 {
			t.Fatalf("the subscribe logged %d WARN lines replacing %s's record, want 1: %s", replaced, sessionID, logs.String()[before:])
		}
	})

	t.Run("POST /v1/interests/unsubscribe", func(t *testing.T) {
		const sessionID = "ses_unreadable_unsubscribe"
		storeUnreadable(t, sessionID)
		recorder := serve(http.MethodPost, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":["notifications.example.unsubscribed"]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status %d, body %s; want 200", recorder.Code, recorder.Body.String())
		}
		if entry, err := interestBucket.Get(sessionID); err != nil || string(entry.Value()) != unreadableRecord {
			t.Fatalf("the bucket's %s record after the unsubscribe = %v, %v; want it unchanged", sessionID, entry, err)
		}
	})
}
