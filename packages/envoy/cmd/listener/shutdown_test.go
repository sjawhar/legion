package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// errorLine matches an ERROR record in each format the listener's process writes: its own JSON
// handler, the default slog text handler and slog's default log.Logger bridge.
var errorLine = regexp.MustCompile(`"level":"ERROR"|level=ERROR|^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ERROR `)

// A SIGTERM is an ordered shutdown, not a failure: the listener stops HTTP, drains NATS and exits,
// and nothing on that path is an error or a reconnect. The listener is stopped with role-lane
// traffic in flight, as a production listener is: a holder on its own connection answers each
// receipt after 150 ms and a role message arrives every 100 ms, so at the signal one forward is
// in its handler and more are queued behind it. Each must reach the holder: the forward opens a
// receipt subscription, which a connection that is already draining refuses. The drain also
// closes the session registry's KV watcher, which logs at ERROR if the registry still expects
// it, and looks like a lost connection to the bus client, whose recovery would re-dial NATS.
// Whether a goroutine logs before the exit is a race, so the test stops the real binary several
// times.
func TestListenerSIGTERMIsAnOrderedShutdown(t *testing.T) {
	_, uri := testnats.Start(t)
	testnats.Connect(t, uri).Close()

	binary := cmdtest.Build(t, "envoy-listener")
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)

	for run := 1; run <= 10; run++ {
		listener := startListenerProcess(t, binary, uri, "sigterm-test-"+strconv.Itoa(run))
		listener.waitHealthy(t)

		role := "sigterm-test-" + strconv.Itoa(run)
		sessionID := "ses_sigterm_holder_" + strconv.Itoa(run)
		postListener(t, listener.port, "/v1/interests/subscribe", `{"session_id":"`+sessionID+`","topics":[],"self_subscribed":true}`)
		postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)
		holder := testnats.Connect(t, uri)
		if _, err := holder.Subscribe(contracts.AgentSubject(sessionID), func(message *natsgo.Msg) {
			time.Sleep(150 * time.Millisecond)
			_ = message.Respond(nil)
		}); err != nil {
			t.Fatalf("run %d: holder subscribe: %v", run, err)
		}
		if err := holder.Flush(); err != nil {
			t.Fatalf("run %d: holder flush: %v", run, err)
		}
		stopTraffic := make(chan struct{})
		trafficDone := make(chan struct{})
		go func() {
			defer close(trafficDone)
			for index := 0; ; index++ {
				select {
				case <-stopTraffic:
					return
				case <-time.After(100 * time.Millisecond):
				}
				item := contracts.Envelope{
					EventID:        "evt-sigterm-" + strconv.Itoa(run) + "-" + strconv.Itoa(index),
					Source:         "agent",
					SourceEventID:  "source-sigterm",
					Topic:          contracts.RoleTopicPrefix + role,
					DedupeKey:      "sigterm." + strconv.Itoa(run) + "." + strconv.Itoa(index),
					IssuedAt:       contracts.NowMillis(),
					PayloadSummary: "sigterm role traffic",
					TraceID:        "trace-sigterm",
				}
				data, _ := json.Marshal(item)
				_ = publisher.Publish(item.Topic, data)
			}
		}()
		time.Sleep(time.Second)

		listener.Terminate(t)
		listener.WaitExit(t, "SIGTERM")
		close(stopTraffic)
		<-trafficDone
		holder.Close()
		if !strings.Contains(listener.Output.String(), "envoy-listener shutdown complete") {
			t.Fatalf("run %d: the listener did not finish its ordered shutdown:\n%s", run, listener.Output.String())
		}
		for _, line := range strings.Split(listener.Output.String(), "\n") {
			if errorLine.MatchString(line) {
				t.Fatalf("run %d: a SIGTERM shutdown logged an error: %s", run, line)
			}
		}
		_, afterSignal, _ := strings.Cut(listener.Output.String(), "received signal, shutting down")
		if strings.Contains(afterSignal, "envoy nats recovery attempt") {
			t.Fatalf("run %d: the listener reconnected to NATS during its shutdown:\n%s", run, afterSignal)
		}
		if !strings.Contains(afterSignal, "listener role forwarded") {
			t.Fatalf("run %d: no role message in flight at the signal reached the holder:\n%s", run, afterSignal)
		}
	}
}

// A standby serves /v1 while another pod holds the lease, so a SIGTERM there (a circuit-breaker
// rollback, a second deploy) is an ordered shutdown like any other: HTTP drains, NATS drains,
// nothing on the path is an error, and the active pod's lease and subscription are left alone.
func TestASIGTERMOnAStandbyIsAnOrderedShutdown(t *testing.T) {
	const machineID = "sigterm-standby"
	uri := testnats.URL(t)
	binary := cmdtest.Build(t, "envoy-listener")
	active := startListenerProcess(t, binary, uri, machineID, "ENVOY_INSTANCE_ID=pod-active")
	active.waitHealthy(t)

	listener := startListenerProcess(t, binary, uri, machineID, "ENVOY_INSTANCE_ID=pod-standby")
	listener.waitStandby(t, "pod-active")
	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM")
	output := listener.Output.String()
	if code := listener.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1 (-1 is a death by the signal, with no ordered shutdown):\n%s", code, output)
	}
	for _, line := range []string{"received signal, shutting down", "envoy-listener shutdown complete"} {
		if !strings.Contains(output, line) {
			t.Fatalf("the listener's output has no %q:\n%s", line, output)
		}
	}
	for line := range strings.SplitSeq(output, "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("a SIGTERM on a standby logged an error: %s", line)
		}
	}
	if role := active.health(t)["role"]; role != "active" {
		t.Fatalf("the active pod's role after the standby stopped: %v, want active", role)
	}
}

// With the live topics lane on, a SIGTERM is an ordered shutdown inside the 30 s every runtime
// gives even while a live delivery is blocked on a serving port that never answers: the live
// lane's drain gives that delivery its 3 s and no more, as core NATS retries nothing, and the main
// client drains after it. Nothing on the path is an error.
func TestASIGTERMWithALiveDeliveryBlockedIsAnOrderedShutdown(t *testing.T) {
	uri := testnats.URL(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "sigterm-live", liveLaneEnv...)
	listener.waitHealthy(t)

	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
	}))
	// Cleanups run last first: the handler returns before the server's Close waits for it.
	t.Cleanup(blocked.Close)
	t.Cleanup(func() { close(release) })
	topic := "notifications.team." + id.New()
	sessionID := "ses_blocked_" + id.New()
	postListener(t, listener.port, "/v1/interests/subscribe", `{"session_id":"`+sessionID+`","topics":["`+topic+`"],"port":`+strconv.Itoa(endpointPort(t, blocked.URL))+`,"host":"127.0.0.1"}`)
	publish(t, connectPublisher(t, uri), newEnvelope("agent", topic, "a message the session never answers"))
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatalf("the live delivery never reached the session's port:\n%s", listener.Output.String())
	}

	signalled := listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM")
	output := listener.Output.String()
	if code := listener.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1 (-1 is a death by the signal, with no ordered shutdown):\n%s", code, output)
	}
	t.Logf("the listener exited %s after the SIGTERM", time.Since(signalled).Round(time.Millisecond))
	if !strings.Contains(output, "envoy-listener shutdown complete") {
		t.Fatalf("the listener did not finish its ordered shutdown:\n%s", output)
	}
	for line := range strings.SplitSeq(output, "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("a SIGTERM with a live delivery blocked logged an error: %s", line)
		}
	}
}

// listenerTestToken is the shared /v1 bearer every test-started listener is given.
const listenerTestToken = "listener-test-token"

// listenerProcess is a listener binary a test started, serving on port.
type listenerProcess struct {
	*cmdtest.Process
	port int
}

// startListenerProcess starts binary as machine machineID against the NATS at uri, on a free port,
// with env added to its environment, and kills it when the test ends if it is still running.
func startListenerProcess(t *testing.T, binary, uri, machineID string, env ...string) *listenerProcess {
	t.Helper()
	port := cmdtest.FreeTCPPort(t)
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + machineID,
		"NATS_URLS=" + uri,
		"ENVOY_API_TOKEN=" + listenerTestToken,
	}, env...)
	return &listenerProcess{Process: cmdtest.Start(t, "the listener "+machineID, cmd), port: port}
}

// postListener calls a listener /v1 route as the test's shared-token caller and requires a 200.
func postListener(t *testing.T, port int, path, body string) {
	t.Helper()
	if status, answer := callListener(t, port, http.MethodPost, path, body); status != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, status, answer)
	}
}

// health reads the listener's /healthz body, whatever its status.
func (p *listenerProcess) health(t *testing.T) map[string]any {
	t.Helper()
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(p.port) + "/healthz")
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var health map[string]any
	if json.NewDecoder(response.Body).Decode(&health) != nil {
		return nil
	}
	return health
}

// waitHealth waits up to 30 s for /healthz to report "healthy" with every field of want, and
// kills the listener and fails the test when it never does. /healthz answers 200 with "starting"
// from the moment the port is bound, before NATS connects and before the listener installs its
// SIGTERM handler, and a SIGTERM in that window kills the process without an ordered shutdown.
func (p *listenerProcess) waitHealth(t *testing.T, what string, want map[string]any) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		health := p.health(t)
		matches := health["status"] == "healthy"
		for field, value := range want {
			matches = matches && health[field] == value
		}
		if matches {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = p.Cmd.Process.Kill()
	t.Fatalf("the listener never became %s:\n%s", what, p.Output.String())
}

// waitHealthy waits for the listener to be healthy and the machine's active pod: it holds the
// lease and receives the durable.
func (p *listenerProcess) waitHealthy(t *testing.T) {
	t.Helper()
	p.waitHealth(t, "healthy and active", map[string]any{"role": "active"})
}

// waitStandby waits for the listener to be healthy as a standby of the pod named activeInstance.
func (p *listenerProcess) waitStandby(t *testing.T, activeInstance string) {
	t.Helper()
	p.waitHealth(t, "a healthy standby of "+activeInstance, map[string]any{"role": "standby", "active_instance": activeInstance})
}
