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
// which is how a slow NATS dial looks from the client.
type delayingProxy struct {
	listener net.Listener
	target   string
	delay    atomic.Int64
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
