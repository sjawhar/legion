package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// testClaim is the claim the default resolver hands out for testToken.
const (
	testClaim      claim.Token = "legion-acme-LEGION-1-tester"
	testToken                  = "boot-token-1"
	testGeneration uint64      = 3
	// wait bounds every test's wait for a real socket to do something; nothing here takes
	// more than milliseconds when it works.
	wait = 5 * time.Second
)

// logRecorder is a slog handler that keeps every line the listener logs, message and
// attributes, so a test can assert the exact text an operator would read.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	line := record.Message
	record.Attrs(func(attr slog.Attr) bool {
		line += " " + attr.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	return nil
}

// The listener logs through the logger it is given and never derives one; a handler that
// dropped attributes here would hide them from the assertions, so it refuses to be derived.
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { panic("stream derived a logger") }
func (r *logRecorder) WithGroup(string) slog.Handler      { panic("stream derived a logger") }

func (r *logRecorder) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

// resolver counts its calls, so a test can assert a rejected hello resolved its token at most
// once and a malformed one never.
type resolver struct {
	calls atomic.Int32
	fn    HelloResolver
}

func (r *resolver) resolve(bootToken string) (claim.Token, uint64, bool, bool) {
	r.calls.Add(1)
	return r.fn(bootToken)
}

func defaultResolver() *resolver {
	return &resolver{fn: func(bootToken string) (claim.Token, uint64, bool, bool) {
		if bootToken == testToken {
			return testClaim, testGeneration, false, true
		}
		return "", 0, false, false
	}}
}

// harness is one listener under test and the context that bounds it.
type harness struct {
	t        *testing.T
	listener *Listener
	logs     *logRecorder
	resolver *resolver
	cancel   context.CancelFunc
	drained  []Event
	done     bool
}

type harnessOptions struct {
	addr       string
	resolver   *resolver
	rpcTimeout time.Duration
}

func startListener(t *testing.T, options harnessOptions) *harness {
	t.Helper()
	if options.addr == "" {
		options.addr = "tcp://127.0.0.1:0"
	}
	if options.resolver == nil {
		options.resolver = defaultResolver()
	}
	if options.rpcTimeout == 0 {
		options.rpcTimeout = wait
	}
	logs := &logRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := Listen(ctx, options.addr, options.resolver.resolve, Options{
		RPCTimeout: options.rpcTimeout,
		Log:        slog.New(logs),
	})
	if err != nil {
		cancel()
		t.Fatalf("Listen(%q): %v", options.addr, err)
	}
	h := &harness{t: t, listener: listener, logs: logs, resolver: options.resolver, cancel: cancel}
	t.Cleanup(func() { h.stop() })
	return h
}

// next is the listener's next event, failing the test when none arrives.
func (h *harness) next() Event {
	h.t.Helper()
	select {
	case event, ok := <-h.listener.Events():
		if !ok {
			h.t.Fatal("Events() closed while an event was expected")
		}
		return event
	case <-time.After(wait):
		h.t.Fatal("no event within the wait")
		return nil
	}
}

// stop ends the listener and returns every event it had not yet handed to next, through the
// channel's close — which is the only way to prove an event was never emitted.
func (h *harness) stop() []Event {
	h.t.Helper()
	if h.done {
		return h.drained
	}
	h.done = true
	h.cancel()
	deadline := time.After(wait)
	for {
		select {
		case event, ok := <-h.listener.Events():
			if !ok {
				return h.drained
			}
			h.drained = append(h.drained, event)
		case <-deadline:
			h.t.Fatal("Events() did not close after the listener's context ended")
			return nil
		}
	}
}

// peer is a shim stand-in on the far side of one real connection: it writes raw lines and
// reads the daemon's frames back.
type peer struct {
	t      *testing.T
	conn   net.Conn
	frames chan shimwire.Frame
	closed chan struct{}
	writer *shimwire.Writer
}

func dial(t *testing.T, addr string) *peer {
	t.Helper()
	network, address, err := parseAddr(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn, err := net.DialTimeout(network, address, wait)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	p := &peer{
		t:      t,
		conn:   conn,
		frames: make(chan shimwire.Frame, 64),
		closed: make(chan struct{}),
		writer: shimwire.NewWriter(conn),
	}
	go func() {
		defer close(p.closed)
		reader := shimwire.NewReader(conn)
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				t.Errorf("the daemon wrote a line that is not a frame: %v", err)
				return
			}
			p.frames <- frame
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return p
}

func (p *peer) writeRaw(text string) {
	p.t.Helper()
	if _, err := io.WriteString(p.conn, text); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

func (p *peer) send(frame shimwire.Frame) {
	p.t.Helper()
	if err := p.writer.WriteFrame(frame); err != nil {
		p.t.Fatalf("peer write %s: %v", frame.FrameType(), err)
	}
}

func (p *peer) hello(bootToken string) {
	p.t.Helper()
	p.send(shimwire.Hello{BootToken: bootToken})
}

// expect is the daemon's next frame, which must be of the given type.
func (p *peer) expect(frameType string) shimwire.Frame {
	p.t.Helper()
	select {
	case frame := <-p.frames:
		if frame.FrameType() != frameType {
			p.t.Fatalf("the daemon sent %s (%#v), want %s", frame.FrameType(), frame, frameType)
		}
		return frame
	case <-p.closed:
		p.t.Fatalf("the daemon closed the connection while %s was expected", frameType)
	case <-time.After(wait):
		p.t.Fatalf("the daemon sent no %s within the wait", frameType)
	}
	return nil
}

// awaitClosed is the daemon hanging up, with nothing sent first.
func (p *peer) awaitClosed() {
	p.t.Helper()
	select {
	case <-p.closed:
	case <-time.After(wait):
		p.t.Fatal("the daemon did not close the connection")
	}
	select {
	case frame := <-p.frames:
		p.t.Fatalf("the daemon sent %s before closing", frame.FrameType())
	default:
	}
}

// connect is a peer whose hello the listener accepted: hello sent, hello_ack read, and the
// Hello event consumed.
func (h *harness) connect(bootToken string) *peer {
	h.t.Helper()
	p := dial(h.t, h.listener.Addr())
	p.hello(bootToken)
	p.expect(shimwire.TypeHelloAck)
	if event, ok := h.next().(Hello); !ok {
		h.t.Fatalf("first event = %#v, want Hello", event)
	}
	return p
}

// negotiate answers the negotiation the connection sends ahead of its first agent request.
func (p *peer) negotiate() {
	p.t.Helper()
	request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	if request.ProtocolVersion != 2 {
		p.t.Fatalf("negotiate_protocol asked for version %d, want 2", request.ProtocolVersion)
	}
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
}

// conn is the listener's connection for testClaim as the concrete type, which is what carries
// Negotiate.
func (h *harness) conn() *Conn {
	h.t.Helper()
	found, ok := h.listener.Conn(testClaim)
	if !ok {
		h.t.Fatalf("no connection registered for %s", testClaim)
	}
	return found.(*Conn)
}

// async runs call on its own goroutine — every Conn request blocks until the peer answers,
// and the peer is driven from the test goroutine.
func async(call func() error) <-chan error {
	result := make(chan error, 1)
	go func() { result <- call() }()
	return result
}

func awaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(wait):
		t.Fatal("the call did not return within the wait")
		return nil
	}
}

// shortSocketPath is a unix socket path under the kernel's 108-byte sun_path limit, which a
// test's own TempDir can exceed.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ws")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "worker-stream.sock")
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return encoded
}

func isClosed(err error) bool { return errors.Is(err, ErrClosed) }
