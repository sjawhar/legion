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
	"errors"
	"fmt"
	"os"
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
	// no two tests share its state.
	held sync.Mutex

	mainRuns  bool
	startOnce sync.Once
	shared    *tcnats.NATSContainer
	serverURL string
	startErr  error
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
// streams, consumers or messages, and holds it for t until t ends.
func URL(t *testing.T) string {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which removes the shared NATS container")
	}
	held.Lock()
	t.Cleanup(held.Unlock)
	startOnce.Do(func() {
		started, err := start()
		if err != nil {
			startErr = err
			return
		}
		shared = started
		serverURL, startErr = started.ConnectionString(context.Background())
	})
	if startErr != nil {
		t.Fatalf("start NATS JetStream: %v", startErr)
	}
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
	return serverURL
}

// JetStream returns a JetStream client of the package's NATS server, as URL gives it to t.
func JetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	URL(t)
	conn, js := connect(t)
	t.Cleanup(conn.Close)
	return js
}

// start runs a NATS container. A container Docker created and never saw ready (a readiness wait
// that timed out under load) comes back beside the error, and is removed before start returns:
// no test's end would remove a container the package shares.
func start(options ...testcontainers.ContainerCustomizer) (*tcnats.NATSContainer, error) {
	started, err := tcnats.Run(context.Background(), "nats:2.10", options...)
	if err != nil {
		return started, errors.Join(err, testcontainers.TerminateContainer(started))
	}
	return started, nil
}

// connect returns a connection to the server once its JetStream API answers.
func connect(t *testing.T) (*nats.Conn, jetstream.JetStream) {
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
