// Package testnats serves daemon tests one JetStream server per test binary, started on first use
// and removed by Main. Each test gets it empty and to itself.
//
// One container per package, not per test: testcontainers keeps its Ryuk reaper only while a
// container of the process is connected, and a reaper left idle past its timeout removes itself,
// so a later test's start, finding it removing, fails ("unexpected container status removing"). A
// per-test container left such a gap whenever the tests between two NATS tests outlasted the
// timeout, as a loaded Docker host makes them.
package testnats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// readinessTimeout bounds the wait for the JetStream API once the container runs. The container
// start is not bounded by it: how long Docker takes to create a container is the daemon's load.
const readinessTimeout = 30 * time.Second

var (
	// held is locked by the test using the server, from URL until the test's cleanups have run, so
	// no two tests share its state. holder names that test, so that a second take by it or by one
	// of its subtests, which would wait on itself, fails instead.
	held     sync.Mutex
	holderMu sync.Mutex
	holder   string

	mainRuns   bool
	startOnce  sync.Once
	shared     *tcnats.NATSContainer
	serverURL  string
	monitorURL string
	startErr   error
)

// Main runs the package's tests, then removes the NATS container if a test started one, and
// returns the exit code. A package whose tests use URL or JetStream calls it from TestMain:
// os.Exit(testnats.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	if shared != nil {
		if err := testcontainers.TerminateContainer(shared); err != nil {
			fmt.Fprintf(os.Stderr, "remove the package's NATS container: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

// URL returns the package's NATS server with no stream on it, so none of a previous test's
// streams, consumers or messages, and holds it for t until t ends. The reset waits for the server
// to report no client connection first: a previous test that published without waiting for acks
// leaves messages the server still routes after the test ends, into a stream this test recreates.
func URL(t testing.TB) string {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which removes the shared NATS container")
	}
	take(t)
	startOnce.Do(func() {
		started, err := start()
		if err != nil {
			startErr = err
			return
		}
		shared = started
		serverURL, startErr = started.ConnectionString(context.Background())
		if startErr == nil {
			monitorURL, startErr = monitor(started)
		}
	})
	if startErr != nil {
		t.Fatalf("start NATS JetStream: %v", startErr)
	}
	drained(t)
	conn, js := connect(t)
	defer conn.Close()
	names := js.StreamNames(t.Context())
	var streams []string
	for name := range names.Name() {
		streams = append(streams, name)
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the NATS server's streams: %v", err)
	}
	for _, name := range streams {
		if err := js.DeleteStream(t.Context(), name); err != nil {
			t.Fatalf("empty the NATS server of stream %s: %v", name, err)
		}
	}
	info, err := js.AccountInfo(t.Context())
	if err != nil {
		t.Fatalf("read the NATS server's account after emptying it: %v", err)
	}
	if info.Streams != 0 {
		t.Fatalf("the NATS server still holds %d streams after emptying it", info.Streams)
	}
	return serverURL
}

// take holds the shared server for t until t ends, refusing a take by the test already holding it
// or by one of its subtests, which would wait for itself for the whole test binary's timeout.
func take(t testing.TB) {
	t.Helper()
	holderMu.Lock()
	current := holder
	holderMu.Unlock()
	if current != "" && (t.Name() == current || strings.HasPrefix(t.Name(), current+"/")) {
		t.Fatalf("testnats: %s already holds the shared NATS server, so %s would wait for itself: take it once per test, through URL or JetStream", current, t.Name())
	}
	held.Lock()
	holderMu.Lock()
	holder = t.Name()
	holderMu.Unlock()
	t.Cleanup(func() {
		holderMu.Lock()
		holder = ""
		holderMu.Unlock()
		held.Unlock()
	})
}

// drained waits, within readinessTimeout, for the server's monitoring endpoint to report no client
// connection, and fails naming the connections left when it does not.
func drained(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(readinessTimeout)
	for {
		connections, err := clientConnections()
		if err == nil && len(connections) == 0 {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("read the NATS server's connections before emptying it: %v", err)
			}
			t.Fatalf("the NATS server still has %d client connections %v from a previous test after %s", len(connections), connections, readinessTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// monitorClient bounds each monitoring request, so one the server never answers cannot outlast the
// drain's own bound.
var monitorClient = &http.Client{Timeout: 5 * time.Second}

// clientConnections lists the server's open client connections, each by id and name.
func clientConnections() ([]string, error) {
	response, err := monitorClient.Get(monitorURL + "/connz")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/connz answered %s", monitorURL, response.Status)
	}
	var connz struct {
		Connections []struct {
			CID  uint64 `json:"cid"`
			Name string `json:"name"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		return nil, fmt.Errorf("decode %s/connz: %w", monitorURL, err)
	}
	connections := make([]string, 0, len(connz.Connections))
	for _, c := range connz.Connections {
		connections = append(connections, fmt.Sprintf("%d %q", c.CID, c.Name))
	}
	return connections, nil
}

// monitor returns the base URL of a container's NATS monitoring endpoint.
func monitor(container *tcnats.NATSContainer) (string, error) {
	ctx := context.Background()
	host, err := container.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := container.MappedPort(ctx, "8222/tcp")
	if err != nil {
		return "", err
	}
	return "http://" + host + ":" + port.Port(), nil
}

// JetStream returns a JetStream client of the package's NATS server, as URL gives it to t.
func JetStream(t testing.TB) jetstream.JetStream {
	t.Helper()
	URL(t)
	conn, js := connect(t)
	t.Cleanup(conn.Close)
	return js
}

// start runs a NATS container, with its monitoring endpoint on 8222. A container Docker created and never saw ready (a readiness wait
// that timed out under load) comes back beside the error, and is removed before start returns:
// no test's end would remove a container the package shares.
func start(options ...testcontainers.ContainerCustomizer) (*tcnats.NATSContainer, error) {
	options = append([]testcontainers.ContainerCustomizer{tcnats.WithArgument("http_port", "8222")}, options...)
	started, err := tcnats.Run(context.Background(), "nats:2.10", options...)
	if err != nil {
		return started, errors.Join(err, testcontainers.TerminateContainer(started))
	}
	return started, nil
}

// connect returns a connection to the server once its JetStream API answers.
func connect(t testing.TB) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	defer cancel()
	for {
		conn, js, err := ready(ctx, serverURL)
		if err == nil {
			return conn, js
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for NATS JetStream readiness: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func ready(ctx context.Context, url string) (*nats.Conn, jetstream.JetStream, error) {
	conn, err := nats.Connect(url, nats.Timeout(time.Second))
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(conn)
	if err == nil {
		_, err = js.AccountInfo(ctx)
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, js, nil
}
