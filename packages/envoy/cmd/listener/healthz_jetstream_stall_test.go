package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// healthzWithin is how long /healthz may take to answer a JetStream that stopped answering: its
// healthzBudget (3 s) and a margin, well inside the JetStream MaxWait (10 s).
const healthzWithin = 3500 * time.Millisecond

// subjectStallRelay forwards NATS client connections to a server. Once stall names subject
// prefixes, it drops each PUB or HPUB from the client whose subject starts with one, payload and
// all, and forwards every other line: the server never sees those requests, so they get no
// answer, while a core PING still gets its PONG. It is how JetStream alone stops answering.
type subjectStallRelay struct {
	listener net.Listener
	target   string
	relayed  string

	mu       sync.Mutex
	prefixes []string
	conns    []net.Conn
}

func startSubjectStallRelay(t *testing.T, uri string) *subjectStallRelay {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse NATS URL %q: %v", uri, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for relay: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	relay := &subjectStallRelay{listener: listener, target: server.Host, relayed: relayed.String()}
	t.Cleanup(relay.close)
	go relay.serve()
	return relay
}

func (r *subjectStallRelay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.relay(client)
	}
}

func (r *subjectStallRelay) relay(client net.Conn) {
	server, err := net.Dial("tcp", r.target)
	if err != nil {
		_ = client.Close()
		return
	}
	r.mu.Lock()
	r.conns = append(r.conns, client, server)
	r.mu.Unlock()
	defer func() { _ = server.Close() }()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	lines := bufio.NewReader(client)
	for {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(string(line))
		var payload []byte
		if len(fields) >= 3 && (strings.EqualFold(fields[0], "PUB") || strings.EqualFold(fields[0], "HPUB")) {
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				return
			}
			payload = make([]byte, size+len("\r\n"))
			if _, err := io.ReadFull(lines, payload); err != nil {
				return
			}
			if r.stalls(fields[1]) {
				continue
			}
		}
		if _, err := server.Write(append(line, payload...)); err != nil {
			return
		}
	}
}

func (r *subjectStallRelay) stalls(subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.prefixes, func(prefix string) bool { return strings.HasPrefix(subject, prefix) })
}

// stall drops every request from here on whose subject starts with one of prefixes.
func (r *subjectStallRelay) stall(prefixes ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefixes = prefixes
}

func (r *subjectStallRelay) close() {
	_ = r.listener.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, conn := range r.conns {
		_ = conn.Close()
	}
}

// /healthz flushes the connection first, a core PING, and then pings each cache's bucket and reads
// the durable consumer's info, which are JetStream requests. nats.go's KV Status takes no context
// and the info read waited out the JetStream MaxWait (10 s), so a JetStream that stops answering
// while core NATS still does held /healthz past a probe's own timeout. Each must answer inside
// /healthz's 3 s budget instead, with the answer the same check waiting out that wait always had:
// 200 degraded, naming the dependency and the context's deadline. The negative control is /healthz
// on the same rig before the stall: 200 healthy.
func TestHealthzAnswersAJetStreamThatStoppedAnsweringInsideItsBudget(t *testing.T) {
	for _, stall := range []struct {
		name     string
		prefixes []string
		want     string
	}{
		{"every JetStream request", []string{"$JS.", "$KV."}, "interest KV unavailable: context deadline exceeded"},
		{"the durable consumer's info read", []string{"$JS.API.CONSUMER.INFO."}, "durable consumer unavailable: context deadline exceeded"},
	} {
		t.Run(stall.name, func(t *testing.T) {
			t.Parallel()
			relay := startSubjectStallRelay(t, testnats.URL(t))
			client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener's bus through the relay: %v", err)
			}
			t.Cleanup(client.Close)
			const consumer = "listener-healthz-jetstream-stall"
			durable, err := startListenerSubscription(client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
			if err != nil {
				t.Fatalf("bind the durable: %v", err)
			}
			t.Cleanup(func() { _ = durable.Unsubscribe() })
			registry, err := store.Open(client.Conn, store.WithReplicas(1))
			if err != nil {
				t.Fatalf("open the interest registry: %v", err)
			}
			t.Cleanup(registry.StopWatch)
			sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
			if err != nil {
				t.Fatalf("open the session registry: %v", err)
			}
			t.Cleanup(sessions.StopWatch)
			ready, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := registry.WaitForCacheReady(ready); err != nil {
				t.Fatalf("wait for the interest cache: %v", err)
			}
			if err := sessions.WaitForCacheReady(ready); err != nil {
				t.Fatalf("wait for the session cache: %v", err)
			}
			var deps atomic.Pointer[listenerDeps]
			deps.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: listenerCaches(registry, sessions, nil), consumer: consumer})
			get := func() (int, map[string]any, time.Duration) {
				recorder := httptest.NewRecorder()
				started := time.Now()
				healthzHandler(&deps).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
				elapsed := time.Since(started)
				var body map[string]any
				_ = json.Unmarshal(recorder.Body.Bytes(), &body)
				return recorder.Code, body, elapsed
			}
			if code, body, _ := get(); code != http.StatusOK || body["status"] != "healthy" {
				t.Fatalf("/healthz with JetStream answering = %d %v, want 200 healthy", code, body)
			}

			relay.stall(stall.prefixes...)
			code, body, elapsed := get()
			if elapsed >= healthzWithin {
				t.Fatalf("/healthz answered after %s (%d %v), want inside %s - a JetStream that stopped answering must be reported inside the budget",
					elapsed, code, body, healthzWithin)
			}
			if code != http.StatusOK || body["status"] != "degraded" || body["error"] != stall.want {
				t.Fatalf("/healthz with %s stalled = %d %v, want 200 degraded with the error %q", stall.name, code, body, stall.want)
			}
			t.Logf("/healthz with %s stalled: %d in %s: %v", stall.name, code, elapsed, body["error"])
		})
	}
}
