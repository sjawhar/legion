package main

import (
	"context"
	"encoding/json"
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

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// errorLine matches an ERROR record in each format the listener's process writes: its own JSON
// handler, the default slog text handler and slog's default log.Logger bridge.
var errorLine = regexp.MustCompile(`"level":"ERROR"|level=ERROR|^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ERROR `)

// A SIGTERM is an ordered shutdown, not a failure: the listener stops HTTP, drains NATS and exits,
// and nothing on that path is an error or a reconnect. The drain closes the session registry's KV
// watcher, and a watcher that ends while the registry still expects it logs "session registry
// watcher stopped" at ERROR; the drain's close also looks like a lost connection to the bus
// client, whose recovery re-dials NATS and re-subscribes a process that is exiting. Whether the
// process exits before either goroutine logs is a race, so the test stops the real binary
// several times.
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

	for run := 1; run <= 10; run++ {
		port := freeTCPPort(t)
		output := &lockedBuffer{}
		cmd := exec.Command(binary)
		cmd.Env = []string{
			"PORT=" + strconv.Itoa(port),
			"ENVOY_LISTEN_HOST=127.0.0.1",
			"ENVOY_MACHINE_ID=sigterm-test",
			"NATS_URLS=" + uri,
			"ENVOY_API_TOKEN=sigterm-test-token",
		}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatalf("run %d: start the listener: %v", run, err)
		}
		waitHealthy(t, port, cmd, output)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("run %d: SIGTERM: %v", run, err)
		}
		_ = cmd.Wait()
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
