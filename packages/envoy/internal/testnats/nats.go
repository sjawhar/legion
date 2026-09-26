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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
	// run, so no two tests share its state. holder names that test, so that a second take by it or
	// by one of its subtests, which would wait on itself, fails instead.
	held     sync.Mutex
	holderMu sync.Mutex
	holder   string

	mainRuns      bool
	sharedOnce    sync.Once
	shared        *tcnats.NATSContainer
	sharedURI     string
	sharedMonitor string
	sharedErr     error
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
// streams, KV buckets, consumers or messages, and holds it for t until t ends. The reset waits for
// the server to report no client connection first: a previous test that published without
// waiting for acks leaves messages the server still routes after the test ends, into a stream
// this test recreates.
func URL(t testing.TB) string {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which removes the shared NATS container")
	}
	take(t)
	sharedOnce.Do(func() {
		ctx := context.Background()
		ctr, err := tcnats.Run(ctx, Image, tcnats.WithArgument("http_port", "8222"))
		if err != nil {
			sharedErr = errors.Join(err, testcontainers.TerminateContainer(ctr))
			return
		}
		shared = ctr
		if sharedURI, sharedErr = ctr.ConnectionString(ctx); sharedErr != nil {
			return
		}
		sharedMonitor, sharedErr = monitor(ctr)
	})
	if sharedErr != nil {
		t.Fatalf("start the shared NATS: %v", sharedErr)
	}
	drained(t)
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
	// The legacy stream listing drops a request that failed or timed out and closes empty, so the
	// account says whether the server is empty.
	info, err := js.AccountInfo()
	if err != nil {
		t.Fatalf("read the shared NATS's account after emptying it: %v", err)
	}
	if info.Streams != 0 {
		t.Fatalf("the shared NATS still holds %d streams after emptying it", info.Streams)
	}
	return sharedURI
}

// take holds the shared server for t until t ends, refusing a take by the test already holding it
// or by one of its subtests, which would wait for itself for the whole test binary's timeout.
func take(t testing.TB) {
	t.Helper()
	holderMu.Lock()
	current := holder
	holderMu.Unlock()
	if current != "" && (t.Name() == current || strings.HasPrefix(t.Name(), current+"/")) {
		t.Fatalf("testnats: %s already holds the shared NATS server, so %s would wait for itself: take it once per test, through URL", current, t.Name())
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

// drained waits, within connectTimeout, for the shared server's monitoring endpoint to report no
// client connection, and fails naming the connections left when it does not.
func drained(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	for {
		connections, err := clientConnections()
		if err == nil && len(connections) == 0 {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("read the shared NATS's connections before emptying it: %v", err)
			}
			t.Fatalf("the shared NATS still has %d client connections %v from a previous test after %s", len(connections), connections, connectTimeout)
		}
		time.Sleep(retryInterval)
	}
}

// clientConnections lists the shared server's open client connections, each by id and name.
func clientConnections() ([]string, error) {
	response, err := http.Get(sharedMonitor + "/connz")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/connz answered %s", sharedMonitor, response.Status)
	}
	var connz struct {
		Connections []struct {
			CID  uint64 `json:"cid"`
			Name string `json:"name"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		return nil, fmt.Errorf("decode %s/connz: %w", sharedMonitor, err)
	}
	connections := make([]string, 0, len(connz.Connections))
	for _, c := range connz.Connections {
		connections = append(connections, fmt.Sprintf("%d %q", c.CID, c.Name))
	}
	return connections, nil
}

// monitor returns the base URL of a container's NATS monitoring endpoint.
func monitor(ctr *tcnats.NATSContainer) (string, error) {
	ctx := context.Background()
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := ctr.MappedPort(ctx, "8222/tcp")
	if err != nil {
		return "", err
	}
	return "http://" + host + ":" + port.Port(), nil
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

// StartRestartable runs a NATS test container a test can stop and start as a server restarts, and
// returns it, once it answers, with a URL that stays the server's across every restart. The URL's
// port is a listener this process holds for the test's life, which relays each connection to
// wherever Docker maps the container's client port at that moment. Docker releases a stopped
// container's host port, so a fixed mapping could be taken by another container or a free-port
// pick before the restart, and the restart then fails with "address already in use". A
// connection made while the server is down is closed at once, as a refused dial would be, and one
// the server drops is dropped on the client's side too. A restart keeps the container's JetStream
// store, as a server restarted on its own volume does.
func StartRestartable(t testing.TB) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctr, err := tcnats.Run(context.Background(), Image)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold the server's port: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go relay(listener, func(ctx context.Context) (string, error) {
		return ctr.PortEndpoint(ctx, "4222/tcp", "")
	})
	uri := "nats://" + listener.Addr().String()
	Connect(t, uri).Close()
	return ctr, uri
}

// relay joins each connection listener accepts to the server's current address, until the
// listener closes.
func relay(listener net.Listener, server func(context.Context) (string, error)) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		go join(client, server)
	}
}

// join copies client and the server's connection into each other until either side ends, then
// closes both. A server without a mapped port (it is stopped) or one that refuses the dial ends
// the client at once.
func join(client net.Conn, server func(context.Context) (string, error)) {
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	address, err := server(ctx)
	cancel()
	if err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return
	}
	defer upstream.Close()
	ended := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); ended <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); ended <- struct{}{} }()
	<-ended
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
