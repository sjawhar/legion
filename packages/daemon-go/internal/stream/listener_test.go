package stream

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// One listener, both address families: a hello over either is acked, the connection is
// registered under the claim its boot token resolves to, and the arrival is a Hello event
// carrying the claim's generation.
func TestListenAcceptsAHelloOverUnixAndTCP(t *testing.T) {
	for _, addr := range []string{"unix://" + shortSocketPath(t), "tcp://127.0.0.1:0"} {
		t.Run(strings.SplitN(addr, ":", 2)[0], func(t *testing.T) {
			h := startListener(t, harnessOptions{addr: addr})
			got := h.listener.Addr()
			if strings.HasPrefix(addr, "tcp://") {
				// The kernel's port, not the :0 the listener was asked for: this is the address
				// a shim is told to dial.
				if !strings.HasPrefix(got, "tcp://127.0.0.1:") || strings.HasSuffix(got, ":0") {
					t.Fatalf("Addr() = %q, want tcp://127.0.0.1:<bound port>", got)
				}
			} else if got != addr {
				t.Fatalf("Addr() = %q, want %q", got, addr)
			}

			p := dial(t, got)
			p.hello(testToken)
			p.expect(shimwire.TypeHelloAck)
			if event := h.next(); event != (Hello{Claim: testClaim, Generation: testGeneration}) {
				t.Fatalf("event = %#v, want Hello{%s, %d}", event, testClaim, testGeneration)
			}
			if _, ok := h.listener.Conn(testClaim); !ok {
				t.Fatal("Conn: the accepted connection is not registered")
			}
			if lines := h.logs.Lines(); len(lines) != 0 {
				t.Fatalf("an accepted hello logged %q", lines)
			}
		})
	}
}

func TestListenRefusesAnAddressItDoesNotSpeak(t *testing.T) {
	for _, addr := range []string{
		"",
		"127.0.0.1:0",
		"http://127.0.0.1:0",
		"unix://relative/worker-stream.sock",
		"unix://",
		"tcp://127.0.0.1",
		"tcp://127.0.0.1:port",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := Listen(ctx, addr, defaultResolver().resolve, Options{RPCTimeout: time.Second})
		cancel()
		if err == nil {
			t.Errorf("Listen(%q) accepted an address it does not speak", addr)
		}
	}
}

func TestListenRefusesANonPositiveRPCTimeout(t *testing.T) {
	_, err := Listen(context.Background(), "tcp://127.0.0.1:0", defaultResolver().resolve, Options{})
	if err == nil {
		t.Fatal("Listen accepted a zero RPCTimeout: every hello would time out at once")
	}
}

// Every way a hello is refused: the connection is closed with nothing written, one exact line
// is logged, the token is resolved at most once, and nothing changes — no registration, no
// event (worker-stream-listener.ts:112-161).
func TestHelloRejectionsCloseLogAndChangeNothing(t *testing.T) {
	stale := &resolver{fn: func(string) (claim.Token, uint64, bool, bool) {
		return testClaim, testGeneration - 1, true, true
	}}
	for _, test := range []struct {
		name          string
		reason        string
		payload       string
		resolver      *resolver
		resolverCalls int32
	}{
		{"not json", "not json", "nope\n", nil, 0},
		{"a JSON value that is not an object", "malformed hello", "123\n", nil, 0},
		{"another frame type", "malformed hello", `{"type":"hi","bootToken":"boot-token-1"}` + "\n", nil, 0},
		{"no boot token", "malformed hello", `{"type":"hello"}` + "\n", nil, 0},
		{"an empty boot token", "malformed hello", `{"type":"hello","bootToken":""}` + "\n", nil, 0},
		{"a boot token that is not a string", "malformed hello", `{"type":"hello","bootToken":7}` + "\n", nil, 0},
		{"an unknown boot token", "unknown boot token", `{"type":"hello","bootToken":"boot-token-2"}` + "\n", nil, 1},
		{"a stale generation", "stale worker generation", `{"type":"hello","bootToken":"boot-token-1"}` + "\n", stale, 1},
		{"no newline within MaxHelloBytes", "hello too long", strings.Repeat("a", shimwire.MaxHelloBytes+1), nil, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startListener(t, harnessOptions{resolver: test.resolver})
			p := dial(t, h.listener.Addr())
			p.writeRaw(test.payload)
			p.awaitClosed()

			want := []string{"worker-stream: rejected hello (" + test.reason + ")"}
			if got := h.logs.Lines(); !slices.Equal(got, want) {
				t.Fatalf("logs = %q, want %q", got, want)
			}
			if calls := h.resolver.calls.Load(); calls != test.resolverCalls {
				t.Fatalf("resolver called %d times, want %d", calls, test.resolverCalls)
			}
			if _, ok := h.listener.Conn(testClaim); ok {
				t.Fatal("a rejected hello registered a connection")
			}
			if events := h.stop(); len(events) != 0 {
				t.Fatalf("a rejected hello emitted %#v", events)
			}
		})
	}
}

// A hello of exactly MaxHelloBytes is still a hello; the bound is on the line, not the frame.
func TestAHelloAtTheByteBoundIsRead(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := dial(t, h.listener.Addr())
	line := `{"type":"hello","bootToken":"boot-token-1"}`
	p.writeRaw(line + strings.Repeat(" ", shimwire.MaxHelloBytes-len(line)) + "\n")
	p.expect(shimwire.TypeHelloAck)
	if lines := h.logs.Lines(); len(lines) != 0 {
		t.Fatalf("logs = %q, want none", lines)
	}
}

// Silence is refused on the clock, not the byte bound, which never fires on a connection that
// sends nothing — or a line that never ends (worker-stream-listener.ts:54-57).
func TestAHelloNotCompletedInTimeIsRejected(t *testing.T) {
	for name, payload := range map[string]string{"silence": "", "an unfinished line": `{"type":"hel`} {
		t.Run(name, func(t *testing.T) {
			h := startListener(t, harnessOptions{rpcTimeout: 50 * time.Millisecond})
			p := dial(t, h.listener.Addr())
			if payload != "" {
				p.writeRaw(payload)
			}
			p.awaitClosed()
			want := []string{"worker-stream: rejected hello (hello timeout)"}
			if got := h.logs.Lines(); !slices.Equal(got, want) {
				t.Fatalf("logs = %q, want %q", got, want)
			}
			if events := h.stop(); len(events) != 0 {
				t.Fatalf("a timed-out hello emitted %#v", events)
			}
		})
	}
}

// A peer that goes away before its hello is not a rejection: nothing was refused, and there is
// nothing for an operator to read.
func TestAConnectionThatClosesBeforeItsHelloLogsNothing(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := dial(t, h.listener.Addr())
	p.writeRaw(`{"type":"hel`)
	p.conn.Close()
	if events := h.stop(); len(events) != 0 {
		t.Fatalf("events = %#v, want none", events)
	}
	if lines := h.logs.Lines(); len(lines) != 0 {
		t.Fatalf("logs = %q, want none", lines)
	}
}

// A claim bound by a live connection keeps it: the second hello is refused and the first
// stream is untouched.
func TestAHelloForAClaimBoundByALiveConnectionIsRejected(t *testing.T) {
	h := startListener(t, harnessOptions{})
	first := h.connect(testToken)
	bound := h.conn()

	second := dial(t, h.listener.Addr())
	second.hello(testToken)
	second.awaitClosed()

	want := []string{"worker-stream: rejected hello (already bound to a live stream)"}
	if got := h.logs.Lines(); !slices.Equal(got, want) {
		t.Fatalf("logs = %q, want %q", got, want)
	}
	if h.conn() != bound {
		t.Fatal("the rejected hello replaced the live binding")
	}
	// The first stream still speaks.
	first.send(shimwire.AgentStart{})
	if event := h.next(); event != (TurnStart{Claim: testClaim}) {
		t.Fatalf("event = %#v, want the first stream's TurnStart", event)
	}
	if events := h.stop(); !slices.Equal(events, []Event{Closed{Claim: testClaim}}) {
		t.Fatalf("remaining events = %#v, want only the first stream's Closed", events)
	}
}

// The reconnect case: once a claim's connection has closed, the shim's next hello with the same
// boot token binds a new connection in its place (worker-stream-listener.ts:106-115).
func TestAHelloReplacesABindingWhoseConnectionClosed(t *testing.T) {
	h := startListener(t, harnessOptions{})
	first := h.connect(testToken)
	old := h.conn()
	first.conn.Close()
	if event := h.next(); event != (Closed{Claim: testClaim}) {
		t.Fatalf("event = %#v, want Closed", event)
	}
	if _, ok := h.listener.Conn(testClaim); ok {
		t.Fatal("a closed connection is still registered")
	}

	h.connect(testToken)
	if h.conn() == old {
		t.Fatal("the reconnect did not replace the closed binding")
	}
	if lines := h.logs.Lines(); len(lines) != 0 {
		t.Fatalf("logs = %q, want none", lines)
	}
}

// Bytes that follow the hello in the same write are the stream's first frames, never lost to
// the hello's own read (worker-stream-listener.ts:86-103).
func TestFramesInTheHellosOwnWriteAreRead(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := dial(t, h.listener.Addr())
	p.writeRaw(`{"type":"hello","bootToken":"boot-token-1"}` + "\n" + `{"type":"agent_start"}` + "\n")
	p.expect(shimwire.TypeHelloAck)
	if event := h.next(); event != (Hello{Claim: testClaim, Generation: testGeneration}) {
		t.Fatalf("event = %#v, want Hello", event)
	}
	if event := h.next(); event != (TurnStart{Claim: testClaim}) {
		t.Fatalf("event = %#v, want TurnStart", event)
	}
}

func TestAwaitReturnsWhenTheRegistrationLands(t *testing.T) {
	h := startListener(t, harnessOptions{})
	awaited := make(chan error, 1)
	var got any
	go func() {
		conn, err := h.listener.Await(context.Background(), testClaim)
		got = conn
		awaited <- err
	}()
	select {
	case err := <-awaited:
		t.Fatalf("Await returned before any hello: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	h.connect(testToken)
	if err := awaitResult(t, awaited); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if got != any(h.conn()) {
		t.Fatal("Await returned a connection other than the registered one")
	}
	// Already registered: at once.
	conn, err := h.listener.Await(context.Background(), testClaim)
	if err != nil || conn != any(h.conn()) {
		t.Fatalf("Await on a registered claim = %v, %v", conn, err)
	}
}

func TestAwaitHonoursItsContext(t *testing.T) {
	h := startListener(t, harnessOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	conn, err := h.listener.Await(ctx, testClaim)
	if !errors.Is(err, context.DeadlineExceeded) || conn != nil {
		t.Fatalf("Await = %v, %v; want nil and context.DeadlineExceeded", conn, err)
	}
}

// The listener's context is its life: when it ends, every connection closes (each with its
// Closed event), Events() closes after them, a waiter is released, and the unix socket file is
// gone.
func TestTheListenerEndsWithItsContext(t *testing.T) {
	path := shortSocketPath(t)
	h := startListener(t, harnessOptions{addr: "unix://" + path})
	p := h.connect(testToken)
	pre := dial(t, h.listener.Addr()) // a connection still in its hello phase

	awaited := async(func() error {
		_, err := h.listener.Await(context.Background(), "legion-acme-LEGION-2-planner")
		return err
	})
	events := h.stop()
	if !slices.Equal(events, []Event{Closed{Claim: testClaim}}) {
		t.Fatalf("events after the context ended = %#v, want the one Closed", events)
	}
	if err := awaitResult(t, awaited); !errors.Is(err, ErrListenerClosed) {
		t.Fatalf("Await after the listener ended = %v, want ErrListenerClosed", err)
	}
	if _, err := h.listener.Await(context.Background(), testClaim); !errors.Is(err, ErrListenerClosed) {
		t.Fatalf("Await on an ended listener = %v, want ErrListenerClosed", err)
	}
	p.awaitClosed()
	pre.awaitClosed()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket file outlived the listener: %v", err)
	}
	if lines := h.logs.Lines(); len(lines) != 0 {
		t.Fatalf("shutting down logged %q", lines)
	}
}

// A daemon that died without unlinking its socket leaves the file behind; the next daemon on the
// same state directory takes the path over. A path something still listens on is refused, and
// a path that is not a socket is never removed.
func TestAUnixSocketLeftByADeadListenerIsTakenOver(t *testing.T) {
	path := shortSocketPath(t)
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	dead.Close()

	h := startListener(t, harnessOptions{addr: "unix://" + path})
	h.connect(testToken)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Listen(ctx, "unix://"+path, defaultResolver().resolve, Options{RPCTimeout: time.Second}); err == nil {
		t.Fatal("Listen took over a socket another listener is serving")
	}

	regular := shortSocketPath(t)
	if err := os.WriteFile(regular, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(ctx, "unix://"+regular, defaultResolver().resolve, Options{RPCTimeout: time.Second}); err == nil {
		t.Fatal("Listen bound over a regular file")
	}
	if content, err := os.ReadFile(regular); err != nil || string(content) != "not a socket" {
		t.Fatalf("the regular file was touched: %q, %v", content, err)
	}
}
