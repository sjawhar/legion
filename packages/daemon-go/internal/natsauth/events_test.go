package natsauth_test

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// signalled is the option, after natsauth.LogEvents, that sends on each channel once the handler
// LogEvents installed has run: the test waits on what it saw logged.
func signalled(asyncErr, disconnected, reconnected chan<- struct{}) nats.Option {
	return func(o *nats.Options) error {
		logErr, logDisconnect, logReconnect := o.AsyncErrorCB, o.DisconnectedErrCB, o.ReconnectedCB
		o.AsyncErrorCB = func(c *nats.Conn, s *nats.Subscription, err error) {
			logErr(c, s, err)
			notify(asyncErr)
		}
		o.DisconnectedErrCB = func(c *nats.Conn, err error) {
			logDisconnect(c, err)
			if err != nil {
				notify(disconnected)
			}
		}
		o.ReconnectedCB = func(c *nats.Conn) {
			logReconnect(c)
			notify(reconnected)
		}
		return nil
	}
}

// notify sends on ch unless it already holds a signal, so a repeated event never blocks nats.go's
// callback goroutine.
func notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// lockedBuffer is a log destination the connection's callback goroutine writes while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// wait fails t unless ch receives within 10s.
func wait(t *testing.T, ch <-chan struct{}, what string, out *lockedBuffer) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was not reported within 10s; log: %s", what, out.String())
	}
}

// An asynchronous error other than a permission refusal is logged at warn with the subject of the
// subscription it names: a slow consumer here.
func TestAnAsynchronousErrorIsLoggedWithItsSubscriptionSubject(t *testing.T) {
	var out lockedBuffer
	asyncErr := make(chan struct{}, 1)
	conn, err := natsauth.Connect([]string{testnats.URL(t)}, "", nats.Timeout(5*time.Second),
		natsauth.LogEvents(slog.New(slog.NewTextHandler(&out, nil))),
		signalled(asyncErr, make(chan struct{}, 1), make(chan struct{}, 1)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	const subject = "legion.test.slow"
	sub, err := conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.SetPendingLimits(1, -1); err != nil {
		t.Fatalf("set pending limits: %v", err)
	}
	for range 5 {
		if err := conn.Publish(subject, []byte("{}")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	wait(t, asyncErr, "the slow consumer", &out)
	want := `level=WARN msg="NATS reported an asynchronous error" subject=` + subject + ` error="nats: slow consumer, messages dropped`
	if !strings.Contains(out.String(), want) {
		t.Errorf("log = %s\nwant a line containing %s", out.String(), want)
	}
}

// A connection the network drops is logged at warn with its cause, and its reconnect at info naming
// the server; the connection's own close logs neither.
func TestADisconnectAndItsReconnectAreLogged(t *testing.T) {
	upstream := strings.TrimPrefix(testnats.URL(t), "nats://")
	proxy := dropProxy(t, upstream)
	var out lockedBuffer
	disconnected, reconnected := make(chan struct{}, 1), make(chan struct{}, 1)
	conn, err := natsauth.Connect([]string{"nats://" + proxy.addr}, "", nats.Timeout(5*time.Second),
		nats.ReconnectWait(10*time.Millisecond), nats.ReconnectJitter(0, 0),
		natsauth.LogEvents(slog.New(slog.NewTextHandler(&out, nil))),
		signalled(make(chan struct{}, 1), disconnected, reconnected))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	proxy.drop()
	wait(t, disconnected, "the disconnect", &out)
	wait(t, reconnected, "the reconnect", &out)
	conn.Close()
	logged := out.String()
	for _, want := range []string{
		`level=WARN msg="NATS connection lost" error=`,
		`level=INFO msg="NATS connection restored" server=nats://` + proxy.addr + "\n",
	} {
		if strings.Count(logged, want) != 1 {
			t.Errorf("log = %s\nwant exactly one line containing %s", logged, want)
		}
	}
}

// proxy forwards every connection to its address on to upstream until drop closes them.
type proxy struct {
	addr  string
	mu    sync.Mutex
	conns []net.Conn
}

// dropProxy is a proxy to upstream, closed with t.
func dropProxy(t *testing.T, upstream string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{addr: ln.Addr().String()}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, client, server)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(server, client); server.Close() }()
			go func() { _, _ = io.Copy(client, server); client.Close() }()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		p.drop()
	})
	return p
}

// drop closes every connection forwarded so far.
func (p *proxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}
