package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A NATS server restart leaves the listener following the interest registry, and a shutdown soon
// after one is still ordered. A restart loses the server's ephemeral consumers, each KV watcher's
// ordered consumer among them. nats.go replaces a lost ordered consumer only once it notices the
// missed heartbeats, up to twenty seconds later, so a watcher the reconnect does not replace keeps
// serving the cache it had while another listener's subscriptions go unseen, and a shutdown in
// that window deletes a consumer the server no longer has, which the watcher's end reports at WARN
// because the consumer is gone either way (kvwatch.Watcher.End, LEGION-278). Every ERROR line
// fails the test.
func TestListenerFollowsTheInterestRegistryAcrossNATSRestarts(t *testing.T) {
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "restart-test")
	listener.waitHealthy(t)

	// subscribe writes an interest through another listener's registry and waits for this
	// listener's cache to list it, failing as round's write when it does not within 3s of since.
	subscribe := func(round int, since time.Time) {
		writer := testnats.Connect(t, uri)
		registry, err := store.Open(writer)
		if err != nil {
			t.Fatalf("restart %d: open the interest registry as another listener: %v", round, err)
		}
		sessionID := "ses_restart_writer_" + strconv.Itoa(round)
		if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "other-listener"}, []string{"notifications.agent." + sessionID}); err != nil {
			t.Fatalf("restart %d: subscribe through another listener: %v", round, err)
		}
		writer.Close()
		deadline := time.Now().Add(3 * time.Second)
		for !listsInterest(t, listener.port, sessionID) {
			if time.Now().After(deadline) {
				t.Fatalf("restart %d: %s after the listener reconnected, its interest cache still missed a subscription another listener wrote", round, time.Since(since).Round(time.Millisecond))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// One interest before the first restart, so the cache has applied a revision a Rewatch can
	// resume after: an empty bucket has none, and is read in full.
	subscribe(0, time.Now())

	for restart := 1; restart <= 3; restart++ {
		server.Stop(t)
		server.Start(t)
		deadline := time.Now().Add(30 * time.Second)
		for strings.Count(listener.Output.String(), "envoy nats reconnected") < restart {
			if time.Now().After(deadline) {
				t.Fatalf("restart %d: the listener never reconnected:\n%s", restart, listener.Output.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
		reconnectedAt := time.Now()

		// The reconnect hook's Rewatch resumes the interest cache after the last revision it
		// applied rather than reading the bucket again. Nothing was written while the server was
		// down, and every write before it (the previous round's) reached the cache, so the
		// resumed watcher applies none. The write below waits for that line, so it cannot land
		// before the resumed consumer exists and be counted in it.
		warmUp := interestWarmUp(t, listener, restart+1)
		if warmUp["outcome"] != "resumed" || warmUp["entries"] != float64(0) || warmUp["delete_markers"] != float64(0) {
			t.Fatalf("restart %d: the interest cache's warm-up after the reconnect = %v, want resumed with no entries and no delete markers", restart, warmUp)
		}

		subscribe(restart, reconnectedAt)
	}

	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM")
	if !strings.Contains(listener.Output.String(), "envoy-listener shutdown complete") {
		t.Fatalf("the listener did not finish its ordered shutdown:\n%s", listener.Output.String())
	}
	for _, line := range strings.Split(listener.Output.String(), "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("NATS restarts and the shutdown after them logged an error: %s", line)
		}
	}
}

// interestWarmUp waits for the listener's nth interest registry warm-up line (1 is its start's)
// and returns it.
func interestWarmUp(t *testing.T, p *listenerProcess, n int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var lines []map[string]any
		for line := range strings.SplitSeq(p.Output.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "interest registry cache warm-up" {
				lines = append(lines, record)
			}
		}
		if len(lines) >= n {
			return lines[n-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no interest registry warm-up line %d within 10s:\n%s", n, p.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// listsInterest reports whether the listener's GET /v1/interests/, which answers from its cache,
// lists the session.
func listsInterest(t *testing.T, port int, sessionID string) bool {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/interests/", nil)
	if err != nil {
		t.Fatalf("interests request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var interests []store.Interest
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&interests) != nil {
		return false
	}
	for _, interest := range interests {
		if interest.SessionID == sessionID {
			return true
		}
	}
	return false
}
