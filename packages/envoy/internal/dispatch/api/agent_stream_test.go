package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// openAgentStream connects a viewer to session and returns the response plus a reader over its
// events. The caller closes the response body to end the stream.
const plannerSessionID = "01a0e090-4848-7473-acc5-fc96e6a646d3"

func openAgentStream(t *testing.T, server *httptest.Server, session, credential string) (*http.Response, *bufio.Reader) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/api/v1/agents/"+session+"/stream", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	switch credential {
	case "cookie":
		request.Header.Set("X-Dispatch-User", "alice")
	case "bearer":
		request.Header.Set("Authorization", "Bearer agent-token")
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	return response, bufio.NewReader(response.Body)
}

// readStreamEvent reads until the next named SSE event and returns its name and data. Heartbeat
// comments are skipped, which is what a viewer does too.
func readStreamEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	name := ""
	data := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v (event %q so far)", err, name)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if name != "" {
				return name, data
			}
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// tableCounts is every row in the Dispatch schema, by table. The relay's promise is that a
// session's conversation reaches a viewer and is never written down, so the proof is that not
// one row appears anywhere while frames flow.
func tableCounts(t *testing.T, database *store.Store) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select table_name from information_schema.tables
		where table_schema = 'public' and table_type = 'BASE TABLE'
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("the schema has no tables; the count proof would be vacuous")
	}
	sort.Strings(names)
	counts := make(map[string]int, len(names))
	for _, name := range names {
		var count int
		if err := database.Pool.QueryRow(ctx, fmt.Sprintf(`select count(*) from %q`, name)).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		counts[name] = count
	}
	return counts
}

func TestAgentStreamRelaysTheSessionsOwnReplayThenItsLiveFrames(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"replay-body"}`))
	handler, _, _ := newTestServer(t, testServerOptions{agentStream: source})
	server := httptest.NewServer(handler)
	defer server.Close()

	response, reader := openAgentStream(t, server, plannerSessionID, "cookie")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("open stream: %d", response.StatusCode)
	}

	// Every stream opens by saying whether the session is answering at all: an empty transcript
	// means one thing for a session that answers and another for one nobody is listening for.
	event, data := readStreamEvent(t, reader)
	if event != "responder" || data != `{"v":1,"responding":true}` {
		t.Fatalf("first event is %q %q, want the responder verdict", event, data)
	}
	event, data = readStreamEvent(t, reader)
	if event != "replay" || data != `{"v":1,"kind":"replay-body"}` {
		t.Fatalf("second event is %q %q, want the session's replay", event, data)
	}
	// The session publishes only while a viewer is attached, so the relay must have told it so
	// before any turn ran — not on its first ten-second tick.
	if got := source.Watches(plannerSessionID); got < 1 {
		t.Fatalf("the relay armed the session %d times, want at least 1", got)
	}

	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":7}`))
	event, data = readStreamEvent(t, reader)
	if event != "frame" || data != `{"v":1,"kind":"message","seq":7}` {
		t.Fatalf("live event is %q %q, want the published frame", event, data)
	}
}

// A session nobody answers for is a different statement from a session with an empty history,
// and it is the only thing that tells a plugin that cannot stream from one that has not spoken:
// every release that can stream answers the control subject, and none that cannot does. The
// relay says so on the wire rather than leaving a viewer waiting on a turn that cannot arrive.
func TestAgentStreamReportsASessionNobodyAnswersFor(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetResponder(plannerSessionID, false)
	handler, _, _ := newTestServer(t, testServerOptions{agentStream: source})
	server := httptest.NewServer(handler)
	defer server.Close()

	response, reader := openAgentStream(t, server, plannerSessionID, "cookie")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("open stream: %d", response.StatusCode)
	}
	event, data := readStreamEvent(t, reader)
	if event != "responder" || data != `{"v":1,"responding":false}` {
		t.Fatalf("first event is %q %q, want the no-responder verdict", event, data)
	}
	// The stream itself is healthy: a frame published to it still reaches the viewer, which is
	// what makes the verdict a statement about the session rather than about the relay.
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"message","seq":1}`))
	event, data = readStreamEvent(t, reader)
	if event != "frame" || data != `{"v":1,"kind":"message","seq":1}` {
		t.Fatalf("live event is %q %q, want the published frame", event, data)
	}
}

func TestAgentStreamWritesNothingToPostgres(t *testing.T) {
	source := agentstream.NewMemory()
	source.SetReplay(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"replay-body"}`))
	handler, database, _ := newTestServer(t, testServerOptions{agentStream: source})
	server := httptest.NewServer(handler)
	defer server.Close()

	before := tableCounts(t, database)

	response, reader := openAgentStream(t, server, plannerSessionID, "cookie")
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("open stream: %d", response.StatusCode)
	}
	if event, _ := readStreamEvent(t, reader); event != "responder" {
		response.Body.Close()
		t.Fatalf("first event is %q, want the responder verdict", event)
	}
	if event, _ := readStreamEvent(t, reader); event != "replay" {
		response.Body.Close()
		t.Fatalf("second event is %q, want replay", event)
	}
	source.Publish(plannerSessionID, agentstream.Frame(`{"v":1,"kind":"tool-result","secret":"hunter2"}`))
	if event, _ := readStreamEvent(t, reader); event != "frame" {
		response.Body.Close()
		t.Fatalf("live event is %q, want frame", event)
	}
	response.Body.Close()

	after := tableCounts(t, database)
	for name, count := range after {
		if count != before[name] {
			t.Fatalf("relaying a session's conversation wrote to %s: %d rows before, %d after", name, before[name], count)
		}
	}
}

func TestAgentStreamRefusesEveryCallerButAHuman(t *testing.T) {
	source := agentstream.NewMemory()
	handler, _, _ := newTestServer(t, testServerOptions{agentStream: source})
	server := httptest.NewServer(handler)
	defer server.Close()

	for _, credential := range []string{"bearer", "anonymous"} {
		t.Run(credential, func(t *testing.T) {
			response, _ := openAgentStream(t, server, plannerSessionID, credential)
			defer response.Body.Close()
			if credential == "anonymous" && response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("an anonymous viewer got %d, want 401", response.StatusCode)
			}
			if credential != "bearer" {
				return
			}
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("a bearer viewer got %d, want 403", response.StatusCode)
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			if body.Code != "HUMAN_ONLY" {
				t.Fatalf("a bearer viewer was refused with %q, want HUMAN_ONLY", body.Code)
			}
		})
	}
	// A refused caller never reached the session: it was never told a viewer is attached, so
	// the session it asked about stays silent.
	if got := source.Watches(plannerSessionID); got != 0 {
		t.Fatalf("a refused viewer armed the session %d times, want 0", got)
	}
}

func TestAgentStreamReportsADeploymentWithNoRelay(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	server := httptest.NewServer(handler)
	defer server.Close()

	response, _ := openAgentStream(t, server, plannerSessionID, "cookie")
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a deployment with no relay answered %d, want 503", response.StatusCode)
	}
}

// A session id goes straight into a NATS subject, where `*` and `>` are wildcards: one request
// carrying either would subscribe a single viewer to every armed session on the bus. The route
// refuses anything that is not a session uuid before it touches NATS at all.
func TestAgentStreamRefusesASessionIdThatIsNotAUUID(t *testing.T) {
	source := agentstream.NewMemory()
	handler, _, _ := newTestServer(t, testServerOptions{agentStream: source})
	server := httptest.NewServer(handler)
	defer server.Close()

	for _, id := range []string{"*", ">", "planner-session", plannerSessionID + ".>"} {
		t.Run(id, func(t *testing.T) {
			response, _ := openAgentStream(t, server, url.PathEscape(id), "cookie")
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("session id %q got %d, want 400", id, response.StatusCode)
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			if body.Code != "SESSION_ID_INPUT" {
				t.Fatalf("session id %q refused with %q, want SESSION_ID_INPUT", id, body.Code)
			}
			if got := source.Watches(id); got != 0 {
				t.Fatalf("a refused session id reached the bus %d times, want 0", got)
			}
		})
	}
}

// A viewer that falls behind ends its stream rather than losing a frame. The relay never parses
// a frame, so it cannot tell a superseded snapshot from a tool result or from the settled frame
// of an earlier message; dropping the wrong one would leave a tool call reading "running…" with
// nothing later to correct it. Ending the response costs a reconnect, and the reconnect rebuilds
// from the session's own replay.
func TestAgentStreamEndsTheStreamWhenAViewerFallsBehind(t *testing.T) {
	source := agentstream.NewMemory()
	handler, _, _ := newTestServer(t, testServerOptions{agentStream: source})
	server := httptest.NewServer(handler)
	defer server.Close()

	response, reader := openAgentStream(t, server, plannerSessionID, "cookie")
	defer response.Body.Close()
	// Overrun the buffer without reading anything.
	for index := range agentStreamBuffer * 4 {
		source.Publish(plannerSessionID, agentstream.Frame(fmt.Sprintf(`{"seq":%d}`, index)))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		_ = line
	}
	t.Fatal("the stream stayed open after the viewer fell behind")
}
