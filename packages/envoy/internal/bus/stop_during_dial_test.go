package bus_test

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// delayingProxy forwards TCP connections to target, holding each new connection for delay first,
// which is how a slow NATS dial looks from the client. With refuse set it closes each connection
// after the delay instead, which is how a dial to a NATS that is gone looks.
type delayingProxy struct {
	listener net.Listener
	target   string
	delay    atomic.Int64
	refuse   atomic.Bool
	held     chan struct{}
	mu       sync.Mutex
	conns    []net.Conn
}

func startDelayingProxy(t *testing.T, target string) *delayingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxy := &delayingProxy{listener: listener, target: target, held: make(chan struct{}, 16)}
	t.Cleanup(proxy.close)
	go proxy.serve()
	return proxy
}

func (p *delayingProxy) url() string { return "nats://" + p.listener.Addr().String() }

func (p *delayingProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.track(client)
		go func() {
			if delay := time.Duration(p.delay.Load()); delay > 0 {
				p.held <- struct{}{}
				time.Sleep(delay)
			}
			if p.refuse.Load() {
				_ = client.Close()
				return
			}
			server, err := net.Dial("tcp", p.target)
			if err != nil {
				_ = client.Close()
				return
			}
			p.track(server)
			go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
			_, _ = io.Copy(client, server)
			_ = client.Close()
		}()
	}
}

func (p *delayingProxy) track(conn net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, conn)
	p.mu.Unlock()
}

func (p *delayingProxy) close() {
	_ = p.listener.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		_ = conn.Close()
	}
}

// A recovery that is dialling when Drain stops the client neither installs the connection its dial
// produces nor reports the stop as a failure: the connection would be one nothing drains, and the
// stop is a shutdown.
func TestARecoveryStoppedMidDialInstallsNothingAndLogsNoError(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startDelayingProxy(t, strings.TrimPrefix(uri, "nats://"))
	client, err := bus.Connect([]string{proxy.url()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	var logs bytes.Buffer
	var logsMu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	proxy.delay.Store(int64(time.Second))
	client.Conn.Close()
	select {
	case <-proxy.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the recovery never dialled")
	}
	_ = client.Drain(5 * time.Second)
	time.Sleep(2 * time.Second)

	if client.Connected() {
		t.Fatal("the recovery installed the connection it dialled after Drain stopped the client")
	}
	logsMu.Lock()
	defer logsMu.Unlock()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Fatalf("a recovery stopped by Drain logged an error: %s", line)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A recovery whose dial is failing when Close stops the client ends at the stop: it logs the
// cancellation and never a reconnect failure. Its dial retries a lost server for up to ten
// attempts, so without that the recovery would outlive the client by seconds and then report a
// shutdown as an ERROR.
func TestARecoveryDiallingALostServerEndsAtTheStop(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startDelayingProxy(t, strings.TrimPrefix(uri, "nats://"))
	client, err := bus.Connect([]string{proxy.url()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	var logs bytes.Buffer
	var logsMu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	logged := func(fragment string) bool {
		logsMu.Lock()
		defer logsMu.Unlock()
		return strings.Contains(logs.String(), fragment)
	}

	proxy.refuse.Store(true)
	proxy.delay.Store(int64(500 * time.Millisecond))
	client.Conn.Close()
	select {
	case <-proxy.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the recovery never dialled")
	}
	client.Close()

	deadline := time.Now().Add(3 * time.Second)
	for !logged("envoy nats recovery cancelled") {
		if time.Now().After(deadline) {
			t.Fatal("the recovery was still dialling 3s after Close stopped the client")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	logsMu.Lock()
	defer logsMu.Unlock()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Fatalf("a recovery stopped by Close logged an error: %s", line)
		}
	}
}
