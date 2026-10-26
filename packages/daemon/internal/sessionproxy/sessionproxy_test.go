package sessionproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testInstance is a placeholder connection name: this repository names no real instance.
const testInstance = "example-project:us-central1:example-instance"

// testProofTimeout bounds the proof dial of a test whose upstream answers at once, so it only has
// to be long enough never to end a dial a loaded machine is slow to complete.
const testProofTimeout = 10 * time.Second

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
	return startConfigured(ctx, t, up, Config{Socket: socket, NewDialer: dialer.build})
}

// startConfigured is startProxy for a test that sets more of the Config: it fills in the test's
// instance and proof bound, starts the proxy and takes the connection the proof dial made.
func startConfigured(ctx context.Context, t *testing.T, up *upstream, cfg Config) *Proxy {
	t.Helper()
	cfg.Instance, cfg.ProofTimeout = testInstance, testProofTimeout
	proxy, err := Start(ctx, cfg)
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
		Socket:       socket,
		Instance:     testInstance,
		NewDialer:    dialer.build,
		ProofTimeout: testProofTimeout,
	})
	if err == nil {
		t.Fatal("Start accepted a dialer that cannot reach the instance")
	}
	// A dial the instance refused keeps the dial's own words: it is no timeout.
	if want := "sessionproxy: dial " + testInstance + ": the instance refused the login"; err.Error() != want {
		t.Fatalf("Start = %v, want %q", err, want)
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

// silentUpstream is an instance that never answers: a real listener that accepts every connection
// and never writes to it, holding each one open until the test ends.
func silentUpstream(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen as the silent upstream: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}

// stallingDialer stands in for the Cloud SQL connector dialing an instance that never answers: it
// opens a real connection to a silent upstream and runs a real TLS handshake over it, which, like
// the connector's own, ends only when its context does.
type stallingDialer struct {
	addr string

	mu     sync.Mutex
	closed int
}

func (s *stallingDialer) Dial(ctx context.Context, _ string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	client := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "x"})
	if err := client.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func (s *stallingDialer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

func (s *stallingDialer) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *stallingDialer) build(context.Context) (Dialer, error) { return s, nil }

// A proof dial the instance never answers ends at ProofTimeout, however long the start's own
// context lives: Start refuses with the instance and the bound named, the dial's deadline still
// readable in the error, the socket path free and the dialer closed, as for a dial the instance
// refused.
func TestStartRefusesWhenTheProofDialGetsNoAnswerWithinItsBound(t *testing.T) {
	dialer := &stallingDialer{addr: silentUpstream(t)}
	socket := socketPath(t)

	began := time.Now()
	_, err := Start(context.Background(), Config{
		Socket:       socket,
		Instance:     testInstance,
		NewDialer:    dialer.build,
		ProofTimeout: 200 * time.Millisecond,
	})
	waited := time.Since(began)
	if err == nil {
		t.Fatal("Start served although its proof dial was never answered")
	}
	if waited >= 2*time.Second {
		t.Fatalf("Start refused %s after it began, want within its 200ms proof bound", waited)
	}
	if want := "sessionproxy: dial " + testInstance + ": no answer within 200ms"; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Start = %v, want it to begin %q", err, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("a refused Start closed the dialer %d times", got)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("the socket path is still held after a refused Start: %v", err)
	}
	_ = listener.Close()
}

// A proof dial is never unbounded: a Start given no ProofTimeout is refused, naming the field,
// before it binds or dials anything.
func TestStartRefusesAnUnboundedProofDial(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)

	_, err := Start(context.Background(), Config{
		Socket:    socket,
		Instance:  testInstance,
		NewDialer: dialer.build,
	})
	if err == nil || !strings.Contains(err.Error(), "ProofTimeout") {
		t.Fatalf("Start = %v, want a refusal naming ProofTimeout", err)
	}
	if got := dialer.dials(); got != 0 {
		t.Fatalf("a Start with no proof bound dialed the upstream %d times", got)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a Start with no proof bound left something at the socket path: %v", err)
	}
}

// A negative drain is refused, naming the field, before Start binds or dials anything.
func TestStartRefusesANegativeDrain(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)

	_, err := Start(context.Background(), Config{
		Socket:       socket,
		Instance:     testInstance,
		NewDialer:    dialer.build,
		ProofTimeout: testProofTimeout,
		Drain:        -time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "Drain") {
		t.Fatalf("Start = %v, want a refusal naming Drain", err)
	}
	if got := dialer.dials(); got != 0 {
		t.Fatalf("a Start with a negative drain dialed the upstream %d times", got)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a Start with a negative drain left something at the socket path: %v", err)
	}
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
		Socket:       socket,
		Instance:     testInstance,
		NewDialer:    dialer.build,
		ProofTimeout: testProofTimeout,
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
		Socket:       socket,
		Instance:     testInstance,
		NewDialer:    dialer.build,
		ProofTimeout: testProofTimeout,
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

// expectClosed fails unless conn's far end has been closed: a read ends with no bytes, at once and
// not for want of time.
func expectClosed(t *testing.T, conn net.Conn, who string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 {
		t.Fatalf("%s read %q on a connection that should have been closed", who, buf[:n])
	}
	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("%s's connection is still open: %v", who, err)
	}
}

// Close ends the proxy however long its clients hold on: a client that keeps its connection open
// is given the drain and no more, after which Close closes both ends of that connection, says so
// and returns. The address is gone, the dialer is closed once, and a second Close is a no-op.
func TestCloseEndsTheProxy(t *testing.T) {
	for _, drain := range []time.Duration{0, 200 * time.Millisecond} {
		t.Run("drain "+drain.String(), func(t *testing.T) {
			up := newUpstream(t)
			dialer := &fakeDialer{addr: up.listener.Addr().String()}
			socket := socketPath(t)
			var logged strings.Builder
			proxy := startConfigured(context.Background(), t, up, Config{
				Socket: socket, NewDialer: dialer.build, Drain: drain, Log: &logged,
			})

			// Bytes cross both ways first, so the connection is established end to end, and the
			// client then holds it open and never hangs up.
			client, err := net.Dial("unix", socket)
			if err != nil {
				t.Fatalf("dial the proxy: %v", err)
			}
			defer client.Close()
			writeAll(t, client, "in flight")
			server := up.next(t)
			defer server.Close()
			if got := readString(t, server, len("in flight")); got != "in flight" {
				t.Fatalf("the upstream read %q", got)
			}
			writeAll(t, server, "answered")
			if got := readString(t, client, len("answered")); got != "answered" {
				t.Fatalf("the client read %q", got)
			}

			began := time.Now()
			closed := make(chan error, 1)
			go func() { closed <- proxy.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("Close did not return within 2s while a client held its connection past the %s drain", drain)
			}
			if waited := time.Since(began); waited < drain {
				t.Fatalf("Close returned after %s, inside the %s drain an open connection is owed", waited, drain)
			}

			expectClosed(t, client, "the client")
			expectClosed(t, server, "the upstream")
			if conn, err := net.Dial("unix", socket); err == nil {
				_ = conn.Close()
				t.Fatal("the proxy still accepts connections after Close")
			}
			if got := dialer.closeCount(); got != 1 {
				t.Fatalf("Close closed the dialer %d times", got)
			}
			want := fmt.Sprintf("sessionproxy: closed 1 connections to %s still open after %s", testInstance, drain)
			if !strings.Contains(logged.String(), want) {
				t.Fatalf("the proxy logged %q, want %q", logged.String(), want)
			}
			if err := proxy.Close(); err != nil {
				t.Fatalf("a second Close: %v", err)
			}
		})
	}
}

// A connection that finishes on its own inside the drain is not cut: its last write, made after
// Close began, reaches the upstream whole, the answer reaches the client, and Close returns as soon
// as the client hangs up, long before the drain is over, without closing anything itself.
func TestCloseLetsAConnectionFinishInsideTheDrain(t *testing.T) {
	const drain = 5 * time.Second
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	var logged strings.Builder
	proxy := startConfigured(context.Background(), t, up, Config{
		Socket: socket, NewDialer: dialer.build, Drain: drain, Log: &logged,
	})

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	writeAll(t, client, "first")
	server := up.next(t)
	defer server.Close()
	if got := readString(t, server, len("first")); got != "first" {
		t.Fatalf("the upstream read %q", got)
	}

	began := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- proxy.Close() }()
	// Close closes the dialer before it waits on the connections, so a closed dialer means Close
	// is under way.
	for deadline := time.Now().Add(2 * time.Second); dialer.closeCount() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Close never closed the dialer")
		}
	}

	// Larger than a socket buffer holds, so the copy is still under way as the write is made.
	payload := strings.Repeat("last write ", 64<<10)
	wrote := make(chan error, 1)
	go func() {
		_, err := client.Write([]byte(payload))
		wrote <- err
	}()
	if got := readString(t, server, len(payload)); got != payload {
		t.Fatalf("the upstream read %d bytes that are not the client's last write", len(got))
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the client's last write: %v", err)
	}
	writeAll(t, server, "done")
	if got := readString(t, client, len("done")); got != "done" {
		t.Fatalf("the client read %q", got)
	}
	_ = client.Close()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return once the connection had finished")
	}
	if waited := time.Since(began); waited >= drain {
		t.Fatalf("Close returned %s after it began, not before the %s drain was over", waited, drain)
	}
	expectClosed(t, server, "the upstream")
	if strings.Contains(logged.String(), "still open") {
		t.Fatalf("Close closed a connection that finished inside the drain: %q", logged.String())
	}
}

// The context a proxy was started with bounds its start and nothing after: once Start has returned,
// cancelling it leaves the proxy serving, with its dialer open and every connection still dialed
// upstream, and only Close ends it. The launcher relies on this to serve a child through its stop
// grace when the container is told to stop and the context it started the proxy with is gone.
func TestCancellingTheContextLeavesAStartedProxyServingUntilClose(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	proxy := startProxy(ctx, t, up, dialer, socket)

	cancel()
	// Long enough for anything that watched the context to have acted on it.
	time.Sleep(100 * time.Millisecond)

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy after its start context ended: %v", err)
	}
	defer client.Close()
	writeAll(t, client, "last write")
	server := up.next(t)
	if got := readString(t, server, len("last write")); got != "last write" {
		t.Fatalf("the upstream read %q", got)
	}
	writeAll(t, server, "written")
	if got := readString(t, client, len("written")); got != "written" {
		t.Fatalf("the client read %q", got)
	}
	if got := dialer.closeCount(); got != 0 {
		t.Fatalf("the dialer was closed %d times while the proxy still served", got)
	}

	_ = client.Close()
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn, err := net.Dial("unix", socket); err == nil {
		_ = conn.Close()
		t.Fatal("the proxy still accepts connections after Close")
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("Close closed the dialer %d times", got)
	}
}

// A start whose context has already ended is refused: its proof dial cannot succeed, so nothing is
// left bound at the socket path and the dialer it built is closed.
func TestStartRefusesWhenItsContextHasEnded(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Start(ctx, Config{
		Socket:       socket,
		Instance:     testInstance,
		NewDialer:    dialer.build,
		ProofTimeout: testProofTimeout,
	})
	if err == nil {
		t.Fatal("Start served although its context had ended")
	}
	if !strings.Contains(err.Error(), testInstance) {
		t.Fatalf("the error does not name the instance: %v", err)
	}
	if got := dialer.closeCount(); got != 1 {
		t.Fatalf("a refused Start closed the dialer %d times", got)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("the socket path is still held after a refused Start: %v", err)
	}
	_ = listener.Close()
}
