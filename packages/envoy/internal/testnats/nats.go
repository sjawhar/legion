// Package testnats provides the NATS image and bounded readiness helpers for Envoy's NATS tests, and
// one server per test binary for the tests that need no server of their own.
//
// A package's tests share that server rather than start a container each: testcontainers keeps its
// Ryuk reaper only while a container of the process is connected, and a reaper left idle past its
// timeout removes itself, so a later start, finding it removing, fails ("unexpected container status
// removing"). Tests that stop, restart or configure their server still start their own (Start,
// StartRestartable); while the shared server runs, the reaper stays connected for them too.
package testnats

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	natsgo "github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// Image is the NATS server image every Envoy test container runs. The envoy-go CI job reads this
// declaration and pulls the image before its first test step, so no test reaches the registry
// mid-run; keep it a single-line string constant.
const Image = "nats:2.10"

var (
	// held is locked by the test using the shared server, from URL until the test's cleanups have
	// run, so no two tests share its state.
	held sync.Mutex

	mainRuns   bool
	sharedOnce sync.Once
	shared     *tcnats.NATSContainer
	sharedURI  string
	sharedErr  error
)

// Main runs the package's tests, then removes the shared server if a test started it, and returns
// the exit code. A package whose tests use URL calls it from TestMain: os.Exit(testnats.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	if shared != nil {
		if err := testcontainers.TerminateContainer(shared); err != nil {
			fmt.Fprintf(os.Stderr, "remove the package's shared NATS container: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

// URL returns the package's shared NATS server with no stream on it, so none of a previous test's
// streams, KV buckets, consumers or messages, and holds it for t until t ends.
func URL(t testing.TB) string {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which removes the shared NATS container")
	}
	held.Lock()
	t.Cleanup(held.Unlock)
	sharedOnce.Do(func() {
		ctx := context.Background()
		ctr, err := tcnats.Run(ctx, Image)
		if err != nil {
			sharedErr = errors.Join(err, testcontainers.TerminateContainer(ctr))
			return
		}
		shared = ctr
		sharedURI, sharedErr = ctr.ConnectionString(ctx)
	})
	if sharedErr != nil {
		t.Fatalf("start the shared NATS: %v", sharedErr)
	}
	conn := Connect(t, sharedURI)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream on the shared NATS: %v", err)
	}
	var streams []string
	for name := range js.StreamNames() {
		streams = append(streams, name)
	}
	for _, name := range streams {
		if err := js.DeleteStream(name); err != nil {
			t.Fatalf("empty the shared NATS of stream %s: %v", name, err)
		}
	}
	return sharedURI
}

// Start runs a NATS test container on Image, removed when the test ends (even when its start
// fails), and returns it with its client URL.
func Start(t testing.TB) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, Image)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	return ctr, uri
}

// StartRestartable runs a NATS test container on a fixed host port, which survives the container's
// stop and start as a server's address does, and returns it with its URL once it answers. A
// restart keeps the container's JetStream store, as a server restarted on its own volume does.
func StartRestartable(t testing.TB) (*tcnats.NATSContainer, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	ctr, err := tcnats.Run(context.Background(), Image, testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
		hostConfig.PortBindings = nat.PortMap{"4222/tcp": {{HostIP: "127.0.0.1", HostPort: port}}}
	}))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	uri := "nats://127.0.0.1:" + port
	Connect(t, uri).Close()
	return ctr, uri
}

// Stop stops a NATS test container within one second, which its Start restarts.
func Stop(t testing.TB, ctr *tcnats.NATSContainer) {
	t.Helper()
	timeout := time.Second
	if err := ctr.Stop(context.Background(), &timeout); err != nil {
		t.Fatalf("stop NATS: %v", err)
	}
}

const (
	connectTimeout = 30 * time.Second
	retryInterval  = 100 * time.Millisecond
	dialTimeout    = time.Second
)

// Connect waits for a test NATS server to accept a real connection and JetStream to answer an
// operation before returning it. Testcontainers can report a mapped port before the NATS protocol
// handshake, and NATS can complete that handshake while JetStream is still starting; a KV test
// needs both readiness boundaries.
func Connect(t testing.TB, uri string) *natsgo.Conn {
	t.Helper()

	deadline := time.Now().Add(connectTimeout)
	var conn *natsgo.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = natsgo.Connect(uri, natsgo.Timeout(dialTimeout), natsgo.NoReconnect())
		if err == nil {
			break
		}
		time.Sleep(retryInterval)
	}
	if conn == nil {
		t.Fatalf("failed to connect to NATS within %s: %v", connectTimeout, err)
		return nil
	}
	waitForJetStream(t, func() error {
		js, err := conn.JetStream()
		if err != nil {
			return err
		}
		_, err = js.AccountInfo()
		return err
	})
	return conn
}

// waitForJetStream retries a real JetStream operation rather than trusting a container port or a
// successful core-NATS connect. Kept separate so its transient boundary has a direct unit test.
func waitForJetStream(t testing.TB, check func() error) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(retryInterval)
	}
	t.Fatalf("JetStream was not ready within %s: %v", connectTimeout, err)
}
