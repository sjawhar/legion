package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// GET /v1/sessions lists every live session the session bucket holds, even one whose id the
// interest bucket refuses as a key (bus.ErrRefused): the bus did not fail to answer, so the list is
// not refused for it, and the session is listed with no topics and roles, as it was before the
// interest read was bounded. An earlier listener stored session ids past this one's write bound
// (testnats.LegacyKeys); the unreadable one, 3,987 bytes in envoy_sessions, is past envoy_interests'
// 3,974-byte get bound, so its interest read is bus.ErrTooLarge, while the readable one, 3,969
// bytes, still fits it and reads as no record. A direct bucket write can also store an id outside
// nats.go's key alphabet, `ses:bad`, whose interest read is bus.ErrInvalidKey. Each refused one is
// logged at WARN once per listing, naming the session. GET /v1/registry/<session_id> answers such a
// live session the same way, 200 with no topics and roles, logged at WARN, and Dispatch's envoy
// client finds it through that route rather than reading a 413 or 400 as a session that cannot
// exist. GET /v1/interests/<session_id>, which asks for the interest record by that key, still
// answers the refused key as the caller's own input: 413 for the too-long id and 400 for the
// invalid one, with no Retry-After, since a retry sends the same key.
func TestTheSessionListKeepsASessionWhoseIDTheInterestBucketRefuses(t *testing.T) {
	const (
		ordinary   = "ses_refused_list_ordinary"
		invalidKey = "ses:bad"
		topic      = "notifications.role.reviewer"
	)
	readableLegacy, unreadableLegacy := testnats.LegacyKeys(bus.SessionBucket, "q")
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
	t.Cleanup(sessions.StopWatch)
	if err := sessions.Put(ordinary, session.SessionEntry{Port: 1, MachineID: "test-machine", Dir: "/test/ordinary", Title: ordinary}); err != nil {
		t.Fatalf("register %s: %v", ordinary, err)
	}
	if _, err := registry.Upsert(store.Interest{SessionID: ordinary, MachineID: "test-machine", Dir: "/test/ordinary"},
		[]string{contracts.AgentSubject(ordinary), topic}); err != nil {
		t.Fatalf("write %s's interest record: %v", ordinary, err)
	}

	// The keys this listener refuses to write go in as an earlier listener or another writer put
	// them: the legacy ids through nats.go's own KV put, and `ses:bad`, which nats.go refuses as a
	// key, as a publish to the bucket's subject.
	raw, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	stored := func(id string) []byte {
		value, err := json.Marshal(session.SessionEntry{Port: 1, MachineID: "earlier", Dir: "/test/refused", Title: "refused", UpdatedAt: time.Now().UnixMilli()})
		if err != nil {
			t.Fatalf("encode %d-byte session %.16s's entry: %v", len(id), id, err)
		}
		return value
	}
	for _, id := range []string{readableLegacy, unreadableLegacy} {
		if _, err := raw.Put(id, stored(id)); err != nil {
			t.Fatalf("store the %d-byte session as an earlier listener did: %v", len(id), err)
		}
	}
	if _, err := client.JS().Publish("$KV."+bus.SessionBucket+"."+invalidKey, stored(invalidKey)); err != nil {
		t.Fatalf("store session %s by a direct bucket write: %v", invalidKey, err)
	}
	listed := []string{ordinary, readableLegacy, unreadableLegacy, invalidKey}
	deadline := time.Now().Add(5 * time.Second)
	for _, id := range listed {
		for {
			if _, err := sessions.Get(id); err == nil {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("the session cache never held the %d-byte session %.16s: %v", len(id), id, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	get := func(handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	before := len(logs.String())
	recorder := get(sessionsHandler(registry, sessions), "/v1/sessions")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions: status %d, body %.300s; want 200 listing every session", recorder.Code, recorder.Body.String())
	}
	var rows []sessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode GET /v1/sessions %.300s: %v", recorder.Body.String(), err)
	}
	byID := map[string]sessionInfo{}
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	if len(rows) != len(listed) {
		t.Fatalf("GET /v1/sessions listed %d rows, want %d", len(rows), len(listed))
	}
	if row := byID[ordinary]; !slices.Equal(row.Topics, []string{contracts.AgentSubject(ordinary), topic}) || !slices.Equal(row.Roles, []string{"reviewer"}) {
		t.Fatalf("GET /v1/sessions row for %s = %+v, want its topics and the role reviewer", ordinary, row)
	}
	for _, id := range []string{readableLegacy, unreadableLegacy, invalidKey} {
		row, ok := byID[id]
		if !ok || row.Title != "refused" || len(row.Topics) != 0 || len(row.Roles) != 0 {
			t.Fatalf("GET /v1/sessions row for the %d-byte session %.16s = %+v (listed %v), want it listed by title with no topics and roles", len(id), id, row, ok)
		}
	}

	warned := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()[before:]), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["level"] != "WARN" {
			continue
		}
		id, _ := record["session_id"].(string)
		message, _ := record["error"].(string)
		if !strings.Contains(message, "key") {
			t.Fatalf("a WARN line for the %d-byte session %.16s carries %.200q, want the key refusal", len(id), id, message)
		}
		warned[id]++
	}
	if len(warned) != 2 || warned[unreadableLegacy] != 1 || warned[invalidKey] != 1 {
		counts := map[int]int{}
		for id, n := range warned {
			counts[len(id)] = n
		}
		t.Fatalf("GET /v1/sessions logged WARN lines by session id length %v, want one each for the %d-byte id and %s", counts, len(unreadableLegacy), invalidKey)
	}

	// GET /v1/registry/<session_id> answers each live session the interest bucket refuses as the list
	// shows it, and logs it once.
	for _, id := range []string{unreadableLegacy, invalidKey} {
		t.Run("registry/"+id[:min(len(id), 8)], func(t *testing.T) {
			before := len(logs.String())
			recorder := get(sessionEntryHandler(state), "/v1/registry/"+id)
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET /v1/registry/<the %d-byte id %.16s>: status %d, body %.300s; want 200 with no topics", len(id), id, recorder.Code, recorder.Body.String())
			}
			info := decodeSessionInfo(t, recorder)
			if info.SessionID != id || info.Title != "refused" || len(info.Topics) != 0 || len(info.Roles) != 0 {
				t.Fatalf("GET /v1/registry/<the %d-byte id %.16s> = %+v, want it by title with no topics and roles", len(id), id, info)
			}
			var warned int
			for _, line := range strings.Split(strings.TrimSpace(logs.String()[before:]), "\n") {
				var record map[string]any
				if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "WARN" && record["session_id"] == id {
					warned++
				}
			}
			if warned != 1 {
				t.Fatalf("GET /v1/registry/<the %d-byte id %.16s> logged %d WARN lines naming it, want 1", len(id), id, warned)
			}
		})
	}

	// Dispatch's envoy client reads a 413 or 400 from that route as a session that cannot exist
	// (envoy.ErrNotFound), so it is asked through the real route too: it must find each session.
	mux := http.NewServeMux()
	registerV1Routes(mux, state, "test-machine", logging.New("test"))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	dispatch := envoy.New(server.URL)
	for _, id := range []string{unreadableLegacy, invalidKey} {
		t.Run("dispatch/"+id[:min(len(id), 8)], func(t *testing.T) {
			found, err := dispatch.Session(context.Background(), id)
			if err != nil {
				t.Fatalf("Dispatch's envoy client asked for the live %d-byte session %.16s: %v; want it found", len(id), id, err)
			}
			if found.SessionID != id || found.Title != "refused" || len(found.Roles) != 0 {
				t.Fatalf("Dispatch's envoy client found %+v for the %d-byte session %.16s, want it by title with no roles", found, len(id), id)
			}
		})
	}

	for _, refused := range []struct {
		id   string
		want int
	}{
		{unreadableLegacy, http.StatusRequestEntityTooLarge},
		{invalidKey, http.StatusBadRequest},
	} {
		t.Run("interests/"+refused.id[:min(len(refused.id), 8)], func(t *testing.T) {
			recorder := get(adminInterestsHandler(registry), "/v1/interests/"+refused.id)
			if recorder.Code != refused.want {
				t.Fatalf("GET /v1/interests/<the %d-byte id %.16s>: status %d, body %.300s; want %d", len(refused.id), refused.id, recorder.Code, recorder.Body.String(), refused.want)
			}
			if got := recorder.Header().Get("Retry-After"); got != "" {
				t.Fatalf("Retry-After = %q, want none: a retry sends the same key", got)
			}
		})
	}
}
