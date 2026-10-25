package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The tests that read GET /auth/whoami's event_head off a real event log open a database of their
// own (storetest.Open); the rest of this package's tests hold none.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// standInEventLog gives a router built with no Store an event log whose newest event is head, so
// GET /auth/whoami answers it: the sign-in tests read whoami for the person it names, and their
// routers hold no database to read a head from.
func standInEventLog(ctx *AppContext, head int64) {
	ctx.eventHead = func(context.Context) (int64, error) { return head, nil }
}

// storeRouter is the router main serves over database, with header identity naming the person and
// a Lifetime, so it mounts GET /ws/live as production does.
func storeRouter(t *testing.T, database *store.Store) http.Handler {
	t.Helper()
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	people := newMemoryPeople()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: "signing-key",
		People:     people,
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Store:      database,
		Docs:       documents,
		Events:     broker,
		Lifetime:   t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, wait := New(ctx)
	t.Cleanup(wait)
	return handler
}

// whoamiAnswer is what GET /auth/whoami answers a signed-in person.
type whoamiAnswer struct {
	Kind      string `json:"kind"`
	Login     string `json:"login"`
	EventHead string `json:"event_head"`
}

// askWhoami answers GET /auth/whoami as alice@d.example, failing the test unless it answers 200.
func askWhoami(t *testing.T, handler http.Handler) whoamiAnswer {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", "alice@d.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("whoami: status=%d body=%s", response.Code, response.Body.String())
	}
	var answer whoamiAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode whoami %s: %v", response.Body.String(), err)
	}
	if answer.Kind != "user" || answer.Login != "alice@d.example" {
		t.Fatalf("whoami = %+v, want alice@d.example", answer)
	}
	return answer
}

// createProject creates the project key through the router's API, as alice@d.example, which
// appends one event, project.created.
func createProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://dispatch.test/api/v1/projects", strings.NewReader(`{"key":"`+key+`","name":"`+key+` project"}`))
	request.Header.Set("X-Dispatch-User", "alice@d.example")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

// GET /auth/whoami carries event_head, the decimal id of the newest committed event, read after the
// sign-in check: "0" on an empty log, and the third event's id once three have committed.
func TestWhoamiCarriesTheNewestEventID(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	if answer := askWhoami(t, handler); answer.EventHead != "0" {
		t.Fatalf("whoami on an empty event log: event_head = %q, want \"0\"", answer.EventHead)
	}

	for _, key := range []string{"ONE", "TWO", "THREE"} {
		createProject(t, handler, key)
	}
	rows, err := database.Pool.Query(context.Background(), `select id from events order by id`)
	if err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read an event id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("the log holds %d events, want the three projects' (%v)", len(ids), ids)
	}
	if answer := askWhoami(t, handler); answer.EventHead != strconv.FormatInt(ids[2], 10) {
		t.Fatalf("whoami after three events: event_head = %q, want the third's id %d", answer.EventHead, ids[2])
	}
}

// A page starts its event-affected reads only after whoami answers and resumes its first events
// subscription from whoami's event_head, so a change committed while the page loads -- after the
// answer, before the subscription -- reaches it during catch-up, and the subscription's
// events_start names that head.
func TestAnEventsSubscriptionFromTheWhoamiHeadReplaysWhatCommittedSince(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	createProject(t, handler, "BEFORE")
	head := askWhoami(t, handler).EventHead

	// Committed while the page loads, between whoami's answer and its subscription.
	createProject(t, handler, "LOADING")

	conn, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", http.Header{"X-Dispatch-User": {"alice@d.example"}})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open the live socket: %v (%s)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{
		"type": "subscribe", "sub": "events", "view": map[string]string{"kind": "events"},
		"resume": map[string]string{"since": head},
	}); err != nil {
		t.Fatalf("subscribe to the event feed: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	next := func(what string) (string, []byte) {
		t.Helper()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read %s: %v", what, err)
			}
			var frame struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Fatalf("decode %s %s: %v", what, data, err)
			}
			if frame.Type != "heartbeat" {
				return frame.Type, data
			}
		}
	}

	kind, data := next("the events_start")
	var start api.LiveEventsStart
	if err := json.Unmarshal(data, &start); err != nil || kind != "events_start" || start.Sub != "events" || start.Since != head {
		t.Fatalf("first frame = %s (%v), want events' events_start from whoami's head %s", data, err, head)
	}
	kind, data = next("the change committed while the page loaded")
	var frame api.LiveEventFrame
	if err := json.Unmarshal(data, &frame); err != nil || kind != "event" {
		t.Fatalf("second frame = %s (%v), want an event", data, err)
	}
	var event struct {
		Type    string `json:"type"`
		Payload struct {
			Key string `json:"key"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(frame.Event, &event); err != nil || event.Type != "project.created" || event.Payload.Key != "LOADING" {
		t.Fatalf("the catch-up's first event = %s (%v), want LOADING's project.created", frame.Event, err)
	}
}

// A whoami that cannot read the event log's head answers 503 EVENT_HEAD_UNAVAILABLE rather than a
// whoami without it: a page that took a missing head for an older server's answer would start its
// feed at the server's head and could miss a change committed while it loaded.
func TestWhoamiRefusesWhenItCannotReadTheEventHead(t *testing.T) {
	for name, build := range map[string]func(t *testing.T) AppContextOptions{
		"a router with no store": func(*testing.T) AppContextOptions { return AppContextOptions{} },
		"a database that stopped answering": func(t *testing.T) AppContextOptions {
			database := storetest.Open(t)
			database.Pool.Close()
			return AppContextOptions{Store: database}
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := build(t)
			people := newMemoryPeople()
			options.SigningKey = "signing-key"
			options.People = people
			options.Identity = identity.HeaderIdentity{Header: "X-Dispatch-User", People: people}
			ctx, err := BuildAppContext(options)
			if err != nil {
				t.Fatalf("build context: %v", err)
			}
			handler, _ := New(ctx)
			request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
			request.Header.Set("X-Dispatch-User", "alice@d.example")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusServiceUnavailable ||
				body.Code != "EVENT_HEAD_UNAVAILABLE" || body.Error == "" {
				t.Fatalf("whoami: status=%d body=%s, want 503 EVENT_HEAD_UNAVAILABLE", response.Code, response.Body.String())
			}
		})
	}
}
