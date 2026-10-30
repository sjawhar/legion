package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

// listenerProcess is a listener binary a test started, serving on the loopback port port.
type listenerProcess struct {
	*cmdtest.Process
	port int
}

// startListenerProcess starts binary as machine machineID against the NATS at uri, with env added
// to its environment, and kills it when the test ends if it is still running. It serves on a
// loopback port the kernel chose and the test holds from that moment, which the process inherits
// as ENVOY_LISTEN_FD and serves on, so no other program can take the port before it serves. PORT
// names the same port, which the listener checks against the socket.
func startListenerProcess(t *testing.T, binary, uri, machineID string, env ...string) *listenerProcess {
	t.Helper()
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + machineID,
		"NATS_URLS=" + uri,
		"ENVOY_API_TOKEN=" + listenerTestToken,
	}, env...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	return &listenerProcess{Process: cmdtest.Start(t, "the listener "+machineID, cmd), port: held.Port}
}

// A harness that holds the port hands the listener the listening socket and sets neither
// ENVOY_LISTEN_HOST nor PORT: the listener serves on the socket, at the address the kernel chose
// for it, and says that address in its listening line. Without the socket it would bind
// 127.0.0.1:9020.
func TestListenerServesOnTheListenSocketItInheritsWithNoAddressSet(t *testing.T) {
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"ENVOY_MACHINE_ID=listen-fd-unset-address",
		"NATS_URLS=" + testnats.URL(t),
		"ENVOY_API_TOKEN=" + listenerTestToken,
	}
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	listener := &listenerProcess{Process: cmdtest.Start(t, "the listener", cmd), port: held.Port}
	// waitHealthy takes the listening line only when its addr is the held socket's, 127.0.0.1 on
	// held.Port, then waits for /healthz there to report healthy.
	listener.waitHealthy(t)
}

// PORT=0 with no inherited socket still binds a port the kernel chooses, and the listening line
// names the port it got, so a harness that starts the listener this way reads its port there.
func TestListenerOnPortZeroLogsThePortTheKernelChose(t *testing.T) {
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"PORT=0",
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=listen-port-zero",
		"NATS_URLS=" + testnats.URL(t),
		"ENVOY_API_TOKEN=" + listenerTestToken,
	}
	process := cmdtest.Start(t, "the listener", cmd)
	addr := listeningAddr(t, process)
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("the listener logged that it listens on %q, which is no address: %v", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if host != "127.0.0.1" || err != nil || port == 0 {
		t.Fatalf("the listener logged that it listens on %q, want 127.0.0.1 and the port the kernel chose:\n%s", addr, process.Output.String())
	}
	(&listenerProcess{Process: process, port: port}).waitHealthy(t)
}

// An inherited socket on every interface is judged by the API auth fence as ENVOY_LISTEN_HOST=0.0.0.0
// is, though ENVOY_LISTEN_HOST is unset and would mean loopback: with no bearer, no OIDC pair and
// no ENVOY_API_ALLOW_UNAUTHENTICATED, the listener refuses to start and serves nothing.
func TestListenerRefusesAnInheritedSocketOnEveryInterfaceWithoutCredentials(t *testing.T) {
	wildcard, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen on every interface: %v", err)
	}
	socket, err := wildcard.(*net.TCPListener).File()
	_ = wildcard.Close()
	if err != nil {
		t.Fatalf("take the listening socket's descriptor: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"ENVOY_MACHINE_ID=listen-fd-wildcard",
		"NATS_URLS=nats://127.0.0.1:1",
		"ENVOY_LISTEN_FD=3",
	}
	cmd.ExtraFiles = []*os.File{socket}
	process := refusedStart(t, cmd)
	refusal := "ENVOY_LISTEN_FD=3, a socket listening on " + wildcard.Addr().String() + ", is not on loopback"
	if output := process.Output.String(); !strings.Contains(output, refusal) || !strings.Contains(output, "ENVOY_API_TOKEN") {
		t.Errorf("the listener did not refuse its start with %q, naming the credentials it needs:\n%s", refusal, output)
	}
}

// PORT, when set beside an inherited socket, must name the socket's port, and PORT=0 names none:
// the listener refuses to start, naming PORT and ENVOY_LISTEN_FD, and serves nothing.
func TestListenerRefusesAPortThatIsNotItsInheritedSocketsPort(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for name, port := range map[string]func(held *cmdtest.HeldPort) string{
		"another port": func(held *cmdtest.HeldPort) string { return strconv.Itoa(held.Port%65535 + 1) },
		"port zero":    func(*cmdtest.HeldPort) string { return "0" },
	} {
		t.Run(name, func(t *testing.T) {
			held := cmdtest.HoldPort(t)
			cmd := exec.Command(binary)
			cmd.Env = []string{
				"PORT=" + port(held),
				"ENVOY_LISTEN_HOST=127.0.0.1",
				"ENVOY_MACHINE_ID=listen-fd-port-mismatch",
				"NATS_URLS=nats://127.0.0.1:1",
				"ENVOY_API_TOKEN=" + listenerTestToken,
			}
			held.Pass(cmd, "ENVOY_LISTEN_FD")
			process := refusedStart(t, cmd)
			setting, socket := "PORT="+port(held)+" ", "ENVOY_LISTEN_FD=3, a socket listening on "+held.Addr
			if output := process.Output.String(); !strings.Contains(output, setting) || !strings.Contains(output, socket) {
				t.Errorf("the listener did not refuse its start naming %q and %q:\n%s", setting, socket, output)
			}
		})
	}
}

// An ENVOY_LISTEN_FD that names something other than a listening socket refuses the start, naming
// the variable, and the listener binds nothing in its place: not PORT, which here names a port the
// test holds, so a bind would fail on it.
func TestListenerRefusesAListenFDThatIsNotASocketAndBindsNothing(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold the port PORT names: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	notASocket, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(occupied.Addr().(*net.TCPAddr).Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=listen-fd-not-a-socket",
		"NATS_URLS=nats://127.0.0.1:1",
		"ENVOY_API_TOKEN=" + listenerTestToken,
		"ENVOY_LISTEN_FD=3",
	}
	cmd.ExtraFiles = []*os.File{notASocket}
	process := refusedStart(t, cmd)
	output := process.Output.String()
	if !strings.Contains(output, "ENVOY_LISTEN_FD=3 is not a socket") {
		t.Errorf("the listener did not refuse its start naming ENVOY_LISTEN_FD=3 and what is wrong with it:\n%s", output)
	}
	if strings.Contains(output, "address already in use") {
		t.Errorf("the listener bound PORT in the descriptor's place:\n%s", output)
	}
}

// refusedStart starts the listener cmd and waits for it to exit, and fails the test unless it
// exited 1 without ever saying it listens.
func refusedStart(t *testing.T, cmd *exec.Cmd) *cmdtest.Process {
	t.Helper()
	process := cmdtest.Start(t, "the listener", cmd)
	process.WaitExit(t, "its start")
	output := process.Output.String()
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("the listener exited %d, want 1:\n%s", code, output)
	}
	if strings.Contains(output, "envoy-listener listening") {
		t.Errorf("the listener served despite refusing its start:\n%s", output)
	}
	return process
}

// listeningAddr waits for the listener's "envoy-listener listening" line and returns its addr.
func listeningAddr(t *testing.T, process *cmdtest.Process) string {
	t.Helper()
	process.WaitForOutput(t, `"msg":"envoy-listener listening"`)
	for line := range strings.SplitSeq(process.Output.String(), "\n") {
		var record struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Msg == "envoy-listener listening" {
			return record.Addr
		}
	}
	t.Fatalf("the listener's output has no JSON listening line:\n%s", process.Output.String())
	return ""
}

// postListener calls a listener /v1 route as the test's shared-token caller and requires a 200.
func postListener(t *testing.T, port int, path, body string) {
	t.Helper()
	if status, answer := callListener(t, port, http.MethodPost, path, body); status != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, status, answer)
	}
}

// health reads the listener's /healthz body, whatever its status. The request has a timeout, so one
// that waits on a socket nothing serves cannot outlast its caller's deadline.
func (p *listenerProcess) health(t *testing.T) map[string]any {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(p.port) + "/healthz")
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

// waitHealth waits up to 30 s for the listener to log that it listens on its own loopback port,
// then up to 30 s more for /healthz there to report "healthy" with every field of want, and kills
// the listener and fails the test when it never does. The socket the listener inherited is not
// readiness: the kernel queues connections on it from before the process starts, so a request
// there waits rather than fails until the process serves, and only its listening line says it
// does. /healthz answers 503 "starting" from the moment the listener serves until it is ready,
// which it is only after it installs its SIGTERM handler, and a SIGTERM before that kills the
// process without an ordered shutdown.
func (p *listenerProcess) waitHealth(t *testing.T, what string, want map[string]any) {
	t.Helper()
	if addr, own := listeningAddr(t, p.Process), "127.0.0.1:"+strconv.Itoa(p.port); addr != own {
		_ = p.Cmd.Process.Kill()
		t.Fatalf("the listener logged that it listens on %s, not on its own %s:\n%s", addr, own, p.Output.String())
	}
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

// A shutdown's wait on a loop that is stopping reads the loop before the context (awaitDone):
// every such wait runs with a context the stop can have run out of, and a select over the two
// ready channels together would pick either, so half of those waits would report work undone
// that was done - a lease left unreleased for a standby to wait out as an expired one
// (lease.Release), a hand-back reported unfinished (durableReceiver.Stop), a warning about loops
// that had returned (waitLoops, shutdownListener). A context that ended with the loop still
// running is its error.
func TestAwaitDoneReadsTheLoopBeforeTheContext(t *testing.T) {
	done := make(chan struct{})
	close(done)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	// Both channels are ready at every call, so a select over the two would take the context's
	// branch about half the time; 100 runs make that certain.
	for run := range 100 {
		if err := awaitDone(ended, done); err != nil {
			t.Fatalf("wait %d on a loop that had returned with an ended context: %v, want it counted done", run+1, err)
		}
	}
	if err := awaitDone(ended, make(chan struct{})); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait on a loop still running with an ended context = %v, want the context's error", err)
	}
}
