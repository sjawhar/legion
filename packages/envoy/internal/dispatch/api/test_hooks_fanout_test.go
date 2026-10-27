package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// holdTestHookLock is a process that is alive but never acknowledges a test hook: a connection of
// its own holding the lock a process with the hooks on holds, keyed as each process keys it, by
// its own backend pid, and listening on nothing. It answers that key; the lock goes when the
// returned connection closes, as a killed process's goes with its connection.
func holdTestHookLock(t *testing.T, database *store.Store) (*pgx.Conn, int32) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("open the silent process's connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	key := int32(conn.PgConn().PID())
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1, $2)`, testHookLockNamespace, key); err != nil {
		t.Fatalf("take the silent process's test-hook lock: %v", err)
	}
	return conn, key
}

// A hook answers only once every process holding the test-hook lock has acted, so a process that
// holds it and never acknowledges is reported rather than passed over: the caller answers
// 504 TEST_HOOK_TIMEOUT once the timeout has run out, naming that process's key.
func TestATestHookNamesTheProcessThatNeverAcknowledged(t *testing.T) {
	handler, database := newTestHandlerWithTestHooks(t)
	_, silent := holdTestHookLock(t, database)

	called := time.Now()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/events/_test/disconnect", nil, "alice")
	waited := time.Since(called)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("disconnect hook with a silent process: status=%d body=%s, want 504", response.Code, response.Body.String())
	}
	body := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if body.Code != "TEST_HOOK_TIMEOUT" || !strings.Contains(body.Error, strconv.Itoa(int(silent))) {
		t.Fatalf("timeout answer = %+v, want TEST_HOOK_TIMEOUT naming the silent process %d", body, silent)
	}
	if waited < testHookTimeout {
		t.Fatalf("the hook gave up after %v, before its %v timeout", waited, testHookTimeout)
	}
}

// A process that dies while a call waits for it takes its lock with its connection, and the call
// stops waiting for it: it answers once the processes still holding the lock have acknowledged,
// not at the timeout. Until then it does not answer, since that process was alive when the call
// was sent.
func TestATestHookStopsWaitingForAProcessWhoseLockIsGone(t *testing.T) {
	handler, database := newTestHandlerWithTestHooks(t)
	conn, _ := holdTestHookLock(t, database)
	const alive = 500 * time.Millisecond
	go func() {
		time.Sleep(alive)
		_ = conn.Close(context.Background())
	}()

	called := time.Now()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/events/_test/disconnect", nil, "alice")
	waited := time.Since(called)
	if response.Code != http.StatusOK {
		t.Fatalf("disconnect hook after the silent process died: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if waited < alive || waited >= testHookTimeout {
		t.Fatalf("the hook answered after %v, want once the silent process's lock went (%v) and inside the %v timeout",
			waited, alive, testHookTimeout)
	}
}

// secondHookProcess is a second server process on database: a pool of its own (and so a listening
// connection and a lock of its own), its own document service and its own in-process conversation
// relay, as a second envoy-dispatch process holds. It answers that process's server.
func secondHookProcess(t *testing.T, database *store.Store) *httptest.Server {
	t.Helper()
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: second, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store:            second,
		Identity:         headerIdentity(second),
		AgentTokens:      sharedAgentTokens(t, "agent-token"),
		ServerURL:        "https://dispatch.example",
		Docs:             documents,
		Events:           broker,
		TestHooksEnabled: true,
		Lifetime:         t.Context(),
		AgentStream:      agentstream.NewMemory(),
	})
	if err != nil {
		t.Fatalf("second process's dependencies: %v", err)
	}
	mux := http.NewServeMux()
	served := register(mux, deps)
	t.Cleanup(served.waitBackground)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// readStreamEventWithin is readStreamEvent bounded by within, failing the test rather than waiting
// for ever on a frame that never comes.
func readStreamEventWithin(t *testing.T, reader *bufio.Reader, within time.Duration) (string, string) {
	t.Helper()
	type event struct{ name, data string }
	read := make(chan event, 1)
	go func() {
		defer close(read)
		var current event
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch line = strings.TrimRight(line, "\r\n"); {
			case line == "":
				if current.name != "" {
					read <- current
					return
				}
			case strings.HasPrefix(line, "event: "):
				current.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case got, ok := <-read:
		if !ok {
			t.Fatal("the stream ended before its next event")
		}
		return got.name, got.data
	case <-time.After(within):
		t.Fatalf("no stream event within %v", within)
		return "", ""
	}
}

// A published frame travels to every process inside the hook's notification, and one
// notification carries less than Postgres's 8000-byte payload. A frame several times that - a
// replay of a long transcript, which the browser suite sets - reaches a viewer on the other
// process byte for byte, multi-byte characters included, whatever notification boundary they fall
// on.
func TestAFrameLargerThanOneNotificationReachesAViewerOnTheOtherProcessWhole(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{testHooks: true, agentStream: agentstream.NewMemory()})
	other := secondHookProcess(t, database)

	viewer, reader := openAgentStream(t, other, plannerSessionID, "cookie")
	defer viewer.Body.Close()
	if viewer.StatusCode != http.StatusOK {
		t.Fatalf("open the viewer on the other process: %d", viewer.StatusCode)
	}
	if event, _ := readStreamEventWithin(t, reader, 10*time.Second); event != "responder" {
		t.Fatalf("the viewer's first event is %q, want the responder verdict", event)
	}

	frame := map[string]any{"v": 1, "kind": "message", "text": strings.Repeat("ünïcødé ✓ the relay carries this turn. ", 700)}
	want, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode the frame: %v", err)
	}
	if len(want) < 3*8000 {
		t.Fatalf("the frame is %d bytes, want several notifications' worth", len(want))
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+plannerSessionID+"/stream/_test/publish", frame, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("publish a %d-byte frame: status=%d body=%s", len(want), response.Code, response.Body.String())
	}
	event, data := readStreamEventWithin(t, reader, 10*time.Second)
	if event != "frame" || data != string(want) {
		t.Fatalf("the other process's viewer read %q of %d bytes, want the %d-byte frame whole", event, len(data), len(want))
	}
}
