// Package stream is the daemon's end of the worker stream: the one listener every
// `legion worker-shim` dials — over a unix socket under tmux, over TCP from a pod — the hello that
// authenticates a shim by its pane's boot token, and the connection that carries the daemon's
// requests to Oh My Pi and its turn facts back.
//
// It is a peer of the runtime, not a part of it. The runtime reports process facts; this package
// reports agent facts (Events), and hands out connections by claim (Conn, Await) — which is how
// the runtime reaches an agent for a graceful stop without importing this package: the Listener
// is the runtime.Conns it is constructed with.
//
// Behaviour is kept from the shipped listener and RPC client —
// packages/daemon/src/daemon/{worker-stream-listener,worker-rpc}.ts — cited at each rule. The
// structure is Go's.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

var _ runtime.Conns = (*Listener)(nil)

// ErrListenerClosed is Await on a listener whose context has ended.
var ErrListenerClosed = errors.New("worker stream listener closed")

// HelloResolver maps a hello's boot token to the claim it was minted for, from the persisted
// hash — the shim re-sends its hello on every reconnect, a daemon restart included, so a token
// resolves for the claim's whole life. ok is false for a token nothing minted; stale is true for
// one minted for a generation the claim has since left.
type HelloResolver func(bootToken string) (c claim.Token, generation uint64, stale bool, ok bool)

// Options are the listener's settings.
type Options struct {
	// RPCTimeout is worker_rpc_timeout_seconds: how long a connection has to complete its hello,
	// and the bound on every request's answer (worker-stream-listener.ts:19-21).
	RPCTimeout time.Duration
	// Log receives the listener's lines; nil is slog.Default().
	Log *slog.Logger
}

// Listener accepts shim connections and keeps the live one for each claim. Its life is the
// context Listen was given: when that ends it stops accepting, closes every connection — each
// with its Closed event — and then closes Events().
type Listener struct {
	addr    string
	ln      net.Listener
	resolve HelloResolver
	timeout time.Duration
	log     *slog.Logger
	events  *eventQueue

	mu    sync.Mutex
	conns map[claim.Token]*Conn
	// accepted is every connection not yet finished, the ones still in their hello included, so
	// the end of the listener can close them all.
	accepted map[net.Conn]struct{}
	// registered is closed and replaced on every registration, and closed for good when the
	// listener ends: Await's wake-up.
	registered chan struct{}
	closed     bool

	wg sync.WaitGroup
}

// Listen binds addr — "unix:///<absolute path>" or "tcp://<host>:<port>" — and serves it until
// ctx ends.
//
// A unix path whose socket a dead listener left behind (a daemon killed before it could unlink)
// is taken over; one a live listener still serves, or a path that is not a socket, is refused.
func Listen(ctx context.Context, addr string, resolve HelloResolver, opts Options) (*Listener, error) {
	if opts.RPCTimeout <= 0 {
		return nil, fmt.Errorf("worker stream: the RPC timeout must be positive, got %s", opts.RPCTimeout)
	}
	network, address, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}
	ln, err := listen(network, address, opts.RPCTimeout)
	if err != nil {
		return nil, fmt.Errorf("worker stream %s is unavailable: %w", addr, err)
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	l := &Listener{
		addr:       network + "://" + ln.Addr().String(),
		ln:         ln,
		resolve:    resolve,
		timeout:    opts.RPCTimeout,
		log:        log,
		events:     newEventQueue(),
		conns:      map[claim.Token]*Conn{},
		accepted:   map[net.Conn]struct{}{},
		registered: make(chan struct{}),
	}
	l.wg.Add(1)
	go l.accept()
	context.AfterFunc(ctx, l.shutdown)
	return l, nil
}

// Addr is the address a shim dials: the one Listen was given, with the port the kernel chose
// when it was asked for port 0.
func (l *Listener) Addr() string { return l.addr }

// Events is every agent fact the stream observes, in order per connection, and closes once the
// listener has ended and every connection's Closed has been delivered. The consumer ranges over
// it until it closes.
func (l *Listener) Events() <-chan Event { return l.events.out }

// Conn is the claim's live connection, if it has one. A claim with none — not connected yet, or
// gone — is an ordinary state, not an error.
func (l *Listener) Conn(c claim.Token) (runtime.Conn, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	conn, ok := l.conns[c]
	if !ok {
		return nil, false
	}
	return conn, true
}

// Await is the claim's connection: at once if it is registered, else when its shim's hello is
// accepted. It fails with ctx, or with ErrListenerClosed once the listener has ended.
func (l *Listener) Await(ctx context.Context, c claim.Token) (runtime.Conn, error) {
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, ErrListenerClosed
		}
		if conn, ok := l.conns[c]; ok {
			l.mu.Unlock()
			return conn, nil
		}
		registered := l.registered
		l.mu.Unlock()
		select {
		case <-registered:
		case <-ctx.Done():
			return nil, fmt.Errorf("worker-stream: %s did not connect: %w", c, ctx.Err())
		}
	}
}

// parseAddr splits a listener address into what net.Listen takes.
func parseAddr(addr string) (network, address string, err error) {
	if path, ok := strings.CutPrefix(addr, "unix://"); ok {
		if !filepath.IsAbs(path) {
			return "", "", fmt.Errorf("worker stream address %q: a unix socket is named by an absolute path, unix:///<path>", addr)
		}
		return "unix", path, nil
	}
	if hostPort, ok := strings.CutPrefix(addr, "tcp://"); ok {
		_, port, err := net.SplitHostPort(hostPort)
		if err == nil {
			_, err = strconv.ParseUint(port, 10, 16)
		}
		if err != nil {
			return "", "", fmt.Errorf("worker stream address %q: want tcp://<host>:<port>: %w", addr, err)
		}
		return "tcp", hostPort, nil
	}
	return "", "", fmt.Errorf("worker stream address %q is neither unix:///<path> nor tcp://<host>:<port>", addr)
}

// listen binds the address, taking over a unix socket file no listener serves any more.
func listen(network, address string, probeTimeout time.Duration) (net.Listener, error) {
	ln, err := net.Listen(network, address)
	if err == nil || network != "unix" || !errors.Is(err, syscall.EADDRINUSE) {
		return ln, err
	}
	if info, statErr := os.Lstat(address); statErr != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, err
	}
	probe, dialErr := net.DialTimeout("unix", address, probeTimeout)
	if dialErr == nil {
		probe.Close()
		return nil, fmt.Errorf("%w: another listener is serving it", err)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return nil, err
	}
	if err := os.Remove(address); err != nil {
		return nil, err
	}
	return net.Listen(network, address)
}

// accept hands every connection to its own goroutine until the listener closes. An accept that
// fails for want of a descriptor or an interrupted call is retried with net/http's backoff — 5 ms
// doubling to a second — over the same class of error (syscall.Errno.Temporary); anything else
// means the listener is broken, and is logged as the end of accepting.
func (l *Listener) accept() {
	defer l.wg.Done()
	var delay time.Duration
	for {
		nc, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || l.isClosed() {
				return
			}
			var errno syscall.Errno
			if !errors.As(err, &errno) || !errno.Temporary() {
				l.log.Error("worker-stream: the listener stopped accepting", "address", l.addr, "error", err)
				return
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			l.log.Warn("worker-stream: accept failed; retrying", "address", l.addr, "error", err, "delay", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			nc.Close()
			continue
		}
		l.accepted[nc] = struct{}{}
		l.wg.Add(1)
		l.mu.Unlock()
		go l.serve(nc)
	}
}

// serve is one connection's life: its hello, then — if the hello is accepted — its stream until
// either side ends it.
func (l *Listener) serve(nc net.Conn) {
	defer l.wg.Done()
	defer func() {
		l.mu.Lock()
		delete(l.accepted, nc)
		l.mu.Unlock()
		nc.Close()
	}()
	// Sized so a hello of MaxHelloBytes and its newline fit, and one byte more without a newline
	// does not: the bound the shipped listener applies before its first newline
	// (worker-stream-listener.ts:11-13). The frames after the hello are read through the same
	// buffer, so bytes that followed the hello in its own write are not lost.
	src := bufio.NewReaderSize(nc, shimwire.MaxHelloBytes+1)
	conn, reason := l.hello(nc, src)
	if conn == nil {
		if reason != "" {
			l.log.Warn("worker-stream: rejected hello (" + reason + ")")
		}
		return
	}
	conn.read(src)
	// Finished first, so a consumer that reads this connection's Closed finds every request on
	// it already failing; then, under the lock registration takes, the Closed is queued ahead of
	// the Hello of any connection that replaces it — no other connection can hold the claim
	// while this one is registered.
	conn.finish()
	l.mu.Lock()
	l.events.push(Closed{Claim: conn.claim})
	delete(l.conns, conn.claim)
	l.mu.Unlock()
}

// hello reads and judges the connection's first line. It returns the registered connection, or
// the reason it refused — empty when there is nothing to say: the peer went away, or the
// listener is ending (worker-stream-listener.ts:107-173).
func (l *Listener) hello(nc net.Conn, src *bufio.Reader) (*Conn, string) {
	if err := nc.SetReadDeadline(time.Now().Add(l.timeout)); err != nil {
		return nil, ""
	}
	line, err := src.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, "hello too long"
	case errors.Is(err, os.ErrDeadlineExceeded) && !l.isClosed():
		// The byte bound never fires on silence, and a connection that never finishes its hello
		// would hold its descriptor for ever (worker-stream-listener.ts:54-57).
		return nil, "hello timeout"
	case err != nil:
		return nil, ""
	}
	line = bytes.TrimSpace(line)
	if !json.Valid(line) {
		return nil, "not json"
	}
	frame, err := shimwire.Decode(line)
	hello, ok := frame.(shimwire.Hello)
	if err != nil || !ok || hello.Validate() != nil {
		return nil, "malformed hello"
	}
	token, generation, stale, known := l.resolve(hello.BootToken)
	switch {
	case !known:
		return nil, "unknown boot token"
	case stale:
		return nil, "stale worker generation"
	}
	return l.register(nc, token, generation)
}

// register binds the claim to this connection, unless a live connection already holds it — the
// shim of a pane that has not died is not displaced by a second dial. A claim whose connection
// has closed is free again, which is the reconnect case.
func (l *Listener) register(nc net.Conn, token claim.Token, generation uint64) (*Conn, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ""
	}
	if _, bound := l.conns[token]; bound {
		return nil, "already bound to a live stream"
	}
	if err := nc.SetReadDeadline(time.Time{}); err != nil {
		return nil, "connection closed before hello_ack"
	}
	conn := newConn(nc, token, l.timeout, l.log, l.events)
	// The ack is written before the connection is registered, so it reaches the shim ahead of any
	// frame a caller sends the moment Conn or Await returns it — a shim drops every line that
	// precedes its hello_ack (worker-shim.ts:492-497). The lock is held across the write because
	// it cannot wait on the peer: a fresh connection's send buffer is empty, and a write that
	// fails means the connection is already gone (worker-stream-listener.ts:88-94).
	if err := conn.writer.WriteFrame(shimwire.HelloAck{}); err != nil {
		return nil, "connection closed before hello_ack"
	}
	l.conns[token] = conn
	l.events.push(Hello{Claim: token, Generation: generation})
	close(l.registered)
	l.registered = make(chan struct{})
	return conn, ""
}

func (l *Listener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// shutdown is the end of the listener's context: no more accepts (closing a unix listener
// unlinks its socket), every connection closed, every waiter released, and — once every
// connection's reader has returned and queued its Closed — the end of Events().
func (l *Listener) shutdown() {
	l.mu.Lock()
	l.closed = true
	close(l.registered)
	l.ln.Close()
	for nc := range l.accepted {
		nc.Close()
	}
	l.mu.Unlock()
	l.wg.Wait()
	l.events.close()
}
