package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
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
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, testnats.Image)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	testnats.Connect(t, uri).Close()

	binary := filepath.Join(t.TempDir(), "envoy-listener")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build the listener: %v\n%s", err, out)
	}
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)

	for run := 1; run <= 10; run++ {
		port := freeTCPPort(t)
		output := &lockedBuffer{}
		cmd := exec.Command(binary)
		cmd.Env = []string{
			"PORT=" + strconv.Itoa(port),
			"ENVOY_LISTEN_HOST=127.0.0.1",
			"ENVOY_MACHINE_ID=sigterm-test-" + strconv.Itoa(run),
			"NATS_URLS=" + uri,
			"ENVOY_API_TOKEN=sigterm-test-token",
		}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatalf("run %d: start the listener: %v", run, err)
		}
		waitHealthy(t, port, cmd, output)

		role := "sigterm-test-" + strconv.Itoa(run)
		sessionID := "ses_sigterm_holder_" + strconv.Itoa(run)
		postListener(t, port, "/v1/interests/subscribe", `{"session_id":"`+sessionID+`","topics":[],"self_subscribed":true}`)
		postListener(t, port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)
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

		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("run %d: SIGTERM: %v", run, err)
		}
		_ = cmd.Wait()
		close(stopTraffic)
		<-trafficDone
		holder.Close()
		if !strings.Contains(output.String(), "envoy-listener shutdown complete") {
			t.Fatalf("run %d: the listener did not finish its ordered shutdown:\n%s", run, output.String())
		}
		for _, line := range strings.Split(output.String(), "\n") {
			if errorLine.MatchString(line) {
				t.Fatalf("run %d: a SIGTERM shutdown logged an error: %s", run, line)
			}
		}
		_, afterSignal, _ := strings.Cut(output.String(), "received signal, shutting down")
		if strings.Contains(afterSignal, "envoy nats recovery attempt") {
			t.Fatalf("run %d: the listener reconnected to NATS during its shutdown:\n%s", run, afterSignal)
		}
		if !strings.Contains(afterSignal, "listener role forwarded") {
			t.Fatalf("run %d: no role message in flight at the signal reached the holder:\n%s", run, afterSignal)
		}
	}
}

// postListener calls a listener /v1 route as the test's shared-token caller and requires a 200.
func postListener(t *testing.T, port int, path, body string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	request.Header.Set("Authorization", "Bearer sigterm-test-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var responseBody strings.Builder
		_, _ = io.Copy(&responseBody, response.Body)
		t.Fatalf("%s: status %d: %s", path, response.StatusCode, responseBody.String())
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// waitHealthy waits for /healthz to report "healthy". It answers 200 with "starting" from the
// moment the port is bound, before NATS connects and before the listener installs its SIGTERM
// handler, and a SIGTERM in that window kills the process without an ordered shutdown.
func waitHealthy(t *testing.T, port int, cmd *exec.Cmd, output *lockedBuffer) {
	t.Helper()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			var health struct {
				Status string `json:"status"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			if decodeErr == nil && health.Status == "healthy" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatalf("the listener never became healthy:\n%s", output.String())
}
