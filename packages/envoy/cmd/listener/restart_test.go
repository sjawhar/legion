package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A NATS server restart leaves the listener following the interest registry, and a shutdown soon
// after one is still ordered. A restart loses the server's ephemeral consumers, each KV watcher's
// ordered consumer among them. nats.go replaces a lost ordered consumer only once it notices the
// missed heartbeats, up to twenty seconds later, so a watcher the reconnect does not replace keeps
// serving the cache it had while another listener's subscriptions go unseen, and a drain in that
// window deletes a consumer the server no longer has and logs the refusal at ERROR (LEGION-278).
func TestListenerFollowsTheInterestRegistryAcrossNATSRestarts(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	listener := startListenerProcess(t, buildListener(t), uri, "restart-test")
	port, cmd, output, exited := listener.port, listener.cmd, listener.output, listener.exited
	waitHealthy(t, port, cmd, output)

	for restart := 1; restart <= 3; restart++ {
		testnats.Stop(t, ctr)
		if err := ctr.Start(context.Background()); err != nil {
			t.Fatalf("restart %d: start NATS again: %v", restart, err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for strings.Count(output.String(), "envoy nats reconnected") < restart {
			if time.Now().After(deadline) {
				t.Fatalf("restart %d: the listener never reconnected:\n%s", restart, output.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
		reconnectedAt := time.Now()

		writer := testnats.Connect(t, uri)
		registry, err := store.Open(writer)
		if err != nil {
			t.Fatalf("restart %d: open the interest registry as another listener: %v", restart, err)
		}
		sessionID := "ses_restart_writer_" + strconv.Itoa(restart)
		if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "other-listener"}, []string{"notifications.agent." + sessionID}); err != nil {
			t.Fatalf("restart %d: subscribe through another listener: %v", restart, err)
		}
		writer.Close()
		deadline = time.Now().Add(3 * time.Second)
		for !listsInterest(t, port, sessionID) {
			if time.Now().After(deadline) {
				t.Fatalf("restart %d: %s after the listener reconnected, its interest cache still missed a subscription another listener wrote", restart, time.Since(reconnectedAt).Round(time.Millisecond))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("the listener did not exit after SIGTERM:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "envoy-listener shutdown complete") {
		t.Fatalf("the listener did not finish its ordered shutdown:\n%s", output.String())
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("NATS restarts and the shutdown after them logged an error: %s", line)
		}
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
	request.Header.Set("Authorization", "Bearer sigterm-test-token")
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
