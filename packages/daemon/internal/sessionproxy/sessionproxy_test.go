package sessionproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testInstance is a placeholder connection name: this repository names no real instance.
const testInstance = "example-project:us-central1:example-instance"

// upstream is the database end of a test: a real local listener, so the bytes the proxy copies
// really cross a connection and a test can read them.
type upstream struct {
	listener net.Listener
	accepted chan net.Conn
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen as the upstream: %v", err)
	}
	up := &upstream{listener: listener, accepted: make(chan net.Conn, 8)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			up.accepted <- conn
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		for {
			select {
			case conn := <-up.accepted:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	return up
}

// next is the connection the upstream accepted next, which is the one the proxy just dialed.
func (u *upstream) next(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-u.accepted:
		return conn
	case <-time.After(2 * time.Second):
		t.Fatal("the proxy did not dial the upstream")
		return nil
	}
}

// fakeDialer stands in for the Cloud SQL connector: it records what the proxy asked of it and
// hands back a real connection to the test's upstream listener.
type fakeDialer struct {
	addr string

	mu        sync.Mutex
	instances []string
	err       error
	closed    int
}

func (f *fakeDialer) Dial(ctx context.Context, instance string) (net.Conn, error) {
	f.mu.Lock()
	f.instances = append(f.instances, instance)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", f.addr)
}

func (f *fakeDialer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeDialer) dials() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.instances)
}

func (f *fakeDialer) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeDialer) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// build is the Config.NewDialer a test passes: the proxy gets this very fake.
func (f *fakeDialer) build(context.Context) (Dialer, error) { return f, nil }

func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "session.sock")
}

// startProxy starts a proxy and takes the connection Start's proof dial made, so that the next
// upstream connection a test reads is the one its own client caused.
func startProxy(ctx context.Context, t *testing.T, up *upstream, dialer *fakeDialer, socket string) *Proxy {
	t.Helper()
	proxy, err := Start(ctx, Config{
		Socket:    socket,
		Instance:  testInstance,
		NewDialer: dialer.build,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = up.next(t).Close()
	return proxy
}

func writeAll(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a write deadline: %v", err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write %q: %v", payload, err)
	}
}

func readString(t *testing.T, conn net.Conn, n int) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return string(buf)
}

// expectNothing fails if anything arrives on conn within a short wait.
func expectNothing(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 {
		t.Fatalf("read %q on a connection that should have carried nothing", buf[:n])
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("read on a connection that should have carried nothing: %v", err)
	}
}

func TestProxyCarriesBytesBothWays(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	proxy := startProxy(context.Background(), t, up, dialer, socket)

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	writeAll(t, client, "startup packet")
	server := up.next(t)
	if got := readString(t, server, len("startup packet")); got != "startup packet" {
		t.Fatalf("the upstream read %q", got)
	}
	writeAll(t, server, "ready for query")
	if got := readString(t, client, len("ready for query")); got != "ready for query" {
		t.Fatalf("the client read %q", got)
	}

	_ = client.Close()
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStartRefusesWhenTheProofDialFails(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	dialer.failWith(errors.New("the instance refused the login"))
	socket := socketPath(t)

	_, err := Start(context.Background(), Config{
		Socket:    socket,
		Instance:  testInstance,
		NewDialer: dialer.build,
	})
	if err == nil {
		t.Fatal("Start accepted a dialer that cannot reach the instance")
	}
	if !strings.Contains(err.Error(), testInstance) {
		t.Fatalf("the error does not name the instance: %v", err)
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("a refused Start closed the dialer %d times", got)
	}

	// Nothing holds the address afterwards, so the generation can be started again.
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("the socket path is still held after a refused Start: %v", err)
	}
	_ = listener.Close()
}

func TestStartRefusesWhenSomethingHoldsTheSocket(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	held, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen as the address's holder: %v", err)
	}
	defer held.Close()

	_, err = Start(context.Background(), Config{
		Socket:    socket,
		Instance:  testInstance,
		NewDialer: dialer.build,
	})
	if err == nil {
		t.Fatal("Start took an address another listener holds")
	}
	if !strings.Contains(err.Error(), socket) {
		t.Fatalf("the error does not name the socket path: %v", err)
	}
	if got := dialer.dials(); got != 0 {
		t.Fatalf("a Start that could not bind still dialed the upstream %d times", got)
	}

	// The holder keeps its address: nothing removed the socket under it.
	go func() {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			_ = conn.Close()
		}
	}()
	if err := held.(*net.UnixListener).SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set an accept deadline: %v", err)
	}
	conn, err := held.Accept()
	if err != nil {
		t.Fatalf("the holder lost its address: %v", err)
	}
	_ = conn.Close()
}

func TestConnectionIsClosedWhenItsUpstreamDialFails(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	proxy := startProxy(context.Background(), t, up, dialer, socket)
	// The credential path worked at Start and stops working afterwards, as a revoked identity
	// would.
	dialer.failWith(errors.New("the instance refused the login"))

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if n != 0 {
		t.Fatalf("the proxy wrote %q to a connection it could not serve", buf[:n])
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the client's read: %v, want EOF", err)
	}

	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestEachConnectionDialsItsOwnUpstream(t *testing.T) {
	// Every connection is dialed on its own, which is how each one carries a freshly minted
	// credential: the Cloud SQL connector mints the login per dial, out of the certificate cache
	// it keeps refreshed, in the ephemeral-certificate exchange. The dial count is the observable
	// here; no token source is faked.
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)

	proxy, err := Start(context.Background(), Config{
		Socket:    socket,
		Instance:  testInstance,
		NewDialer: dialer.build,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := dialer.dials(); got != 1 {
		t.Fatalf("Start made %d dials, want the one that proves the credential path", got)
	}
	_ = up.next(t).Close()

	first, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	writeAll(t, first, "one")
	firstUpstream := up.next(t)
	if got := readString(t, firstUpstream, 3); got != "one" {
		t.Fatalf("the first connection's upstream read %q", got)
	}

	second, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy a second time: %v", err)
	}
	writeAll(t, second, "two")
	secondUpstream := up.next(t)
	if got := readString(t, secondUpstream, 3); got != "two" {
		t.Fatalf("the second connection's upstream read %q", got)
	}
	if got := dialer.dials(); got != 3 {
		t.Fatalf("the proxy made %d dials, want the proof dial plus one per connection", got)
	}

	// The two connections share no upstream: nothing the second wrote reached the first's.
	expectNothing(t, firstUpstream)

	// And the first connection still carries bytes after the second arrived.
	writeAll(t, first, "still here")
	if got := readString(t, firstUpstream, len("still here")); got != "still here" {
		t.Fatalf("the first connection's upstream read %q", got)
	}

	_ = first.Close()
	_ = second.Close()
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCloseEndsTheProxy(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	proxy := startProxy(context.Background(), t, up, dialer, socket)

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	writeAll(t, client, "in flight")
	server := up.next(t)
	if got := readString(t, server, len("in flight")); got != "in flight" {
		t.Fatalf("the upstream read %q", got)
	}

	closed := make(chan error, 1)
	go func() { closed <- proxy.Close() }()

	select {
	case err := <-closed:
		t.Fatalf("Close returned while a copy was still in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// The address is gone even though Close has not returned yet.
	if conn, err := net.Dial("unix", socket); err == nil {
		_ = conn.Close()
		t.Fatal("the proxy still accepts connections after Close")
	}

	_ = client.Close()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return once the copy had finished")
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("Close closed the dialer %d times", got)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

func TestCancellingTheContextStopsTheProxy(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	proxy := startProxy(ctx, t, up, dialer, socket)

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the proxy still accepts connections after its context ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close after the context ended: %v", err)
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("the dialer was closed %d times", got)
	}
}
