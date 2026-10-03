package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// listenerRelay lets this test hold one NATS request from the listener, then drop the live link so
// nats.go reconnects through the same relay. New connections forward normally.
type listenerRelay struct {
	listener net.Listener
	target   string
	relayed  string

	mu        sync.Mutex
	trigger   []byte
	holdAfter int
	links     []*listenerRelayLink

	swallowed chan struct{}
}

type listenerRelayLink struct {
	client, server net.Conn
	swallow        atomic.Bool
	seen           []byte
}

func startListenerRelay(t *testing.T, uri string) *listenerRelay {
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
	relay := &listenerRelay{
		listener:  listener,
		target:    server.Host,
		relayed:   relayed.String(),
		swallowed: make(chan struct{}, 1),
	}
	t.Cleanup(relay.close)
	go relay.serve()
	return relay
}

func (r *listenerRelay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.relay(client)
	}
}

func (r *listenerRelay) relay(client net.Conn) {
	link := &listenerRelayLink{client: client}
	r.mu.Lock()
	r.links = append(r.links, link)
	r.mu.Unlock()
	server, err := net.Dial("tcp", r.target)
	if err != nil {
		_ = client.Close()
		return
	}
	link.server = server
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	defer func() { _ = server.Close() }()

	buffer := make([]byte, 32*1024)
	for {
		n, err := client.Read(buffer)
		if n > 0 && !link.swallow.Load() {
			if r.shouldSwallow(link, buffer[:n]) {
				link.swallow.Store(true)
				select {
				case r.swallowed <- struct{}{}:
				default:
				}
			} else if _, writeErr := server.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *listenerRelay) shouldSwallow(link *listenerRelayLink, data []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	link.seen = append(link.seen, data...)
	return r.holdAfter > 0 && bytes.Count(link.seen, r.trigger) >= r.holdAfter
}

func (r *listenerRelay) arm(trigger string, occurrence int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trigger = []byte(trigger)
	r.holdAfter = occurrence
	for _, link := range r.links {
		link.seen = nil
	}
}

func (r *listenerRelay) drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, link := range r.links {
		_ = link.client.Close()
		if link.server != nil {
			_ = link.server.Close()
		}
	}
	r.links = nil
}

func (r *listenerRelay) close() {
	_ = r.listener.Close()
	r.drop()
}

// During a deploy, a reconnect can interrupt the replacement's durable bind. The reconnect must
// leave that bind to finish, keep the role lane forwarding, then make SIGTERM an ordered shutdown.
func TestListenerReconnectWithBindInFlightShutsDownAfterForwardingRoles(t *testing.T) {
	const (
		machineID = "reconnect-bind-sigterm"
		holderID  = "ses_reconnect_bind_sigterm_holder"
		role      = "reconnect-bind-sigterm-role"
		messages  = 2
	)
	uri := testnats.URL(t)
	relay := startListenerRelay(t, uri)
	_, durable := durableHeldElsewhere(t, uri, "listener-"+machineID)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), relay.relayed, machineID)
	listener.WaitForOutput(t, "subscribe failed, retrying")

	holder, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	t.Cleanup(holder.Close)
	postListener(t, listener.port, "/v1/interests/subscribe", `{"session_id":"`+holderID+`","topics":[],"self_subscribed":true}`)
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+holderID+`","role":"`+role+`"}`)
	frames := receiveAsSession(t, holder, holderID)

	relay.arm("$JS.API.CONSUMER.INFO.", 2)
	select {
	case <-relay.swallowed:
	case <-time.After(10 * time.Second):
		t.Fatal("the listener's durable bind never reached the controlled relay")
	}
	relay.drop()
	waitFor(t, 10*time.Second, "the listener to reconnect", func() bool {
		return strings.Contains(listener.Output.String(), "envoy nats reconnected")
	})
	waitFor(t, 10*time.Second, "the restore to report the bind in flight", func() bool {
		output := listener.Output.String()
		return strings.Contains(output, "envoy nats resubscribe left to the bind in flight") ||
			strings.Contains(output, "envoy nats resubscribe failed")
	})
	output := listener.Output.String()
	if !strings.Contains(output, "envoy nats resubscribe left to the bind in flight") {
		t.Fatalf("the restore did not report the bind in flight at INFO:\n%s", output)
	}
	if line := firstErrorLine(output); line != "" {
		t.Fatalf("the reconnect with a bind in flight logged ERROR: %s", line)
	}
	if strings.Contains(output, "durable bound") {
		t.Fatalf("the durable bound before the role messages proved the reconnect window:\n%s", output)
	}

	publisher := testnats.Connect(t, uri)
	defer publisher.Close()
	for index := range messages {
		id := strconv.Itoa(index)
		item := contracts.Envelope{
			EventID:        "evt-reconnect-bind-sigterm-" + id,
			Source:         "agent",
			SourceEventID:  "source-reconnect-bind-sigterm",
			Topic:          contracts.RoleTopicPrefix + role,
			DedupeKey:      "reconnect-bind-sigterm-" + id,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "role message during reconnect",
			TraceID:        "trace-reconnect-bind-sigterm",
		}
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal role message %d: %v", index, err)
		}
		if err := publisher.Publish(item.Topic, data); err != nil {
			t.Fatalf("publish role message %d: %v", index, err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush role messages: %v", err)
	}
	for received := range messages {
		select {
		case frame := <-frames:
			if !strings.HasPrefix(frame.DedupeKey, roleForwardDedupePrefix) {
				t.Fatalf("role message %d forwarded with dedupe key %q, want %s prefix", received, frame.DedupeKey, roleForwardDedupePrefix)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the listener forwarded %d of %d routed role messages during the reconnect:\n%s", received, messages, listener.Output.String())
		}
	}

	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM during the bind in flight")
	output = listener.Output.String()
	for _, line := range []string{"received signal, shutting down", "envoy-listener shutdown complete"} {
		if !strings.Contains(output, line) {
			t.Fatalf("the listener did not log %q:\n%s", line, output)
		}
	}
	if line := firstErrorLine(output); line != "" {
		t.Fatalf("the ordered SIGTERM shutdown logged ERROR: %s", line)
	}
	if !durable.IsValid() {
		t.Fatal("the old task's durable subscription was lost when the replacement stopped")
	}
}

func firstErrorLine(output string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if errorLine.MatchString(line) {
			return line
		}
	}
	return ""
}
