package main

import (
	"bytes"
	"encoding/json"
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

// listenerRelay stands between one listener and its NATS server, so a test can fail the link the
// ways a pod's link fails. arm holds one NATS request from the listener; drop closes every link,
// which nats.go reconnects through the same relay at once; cut closes them and refuses new ones
// for a while, an outage the listener reconnects in place after; sever closes them and refuses
// every new one for the rest of the test, which is how a bus that has gone away under the listener
// looks; freeze stops moving bytes in either direction with every socket left open, as a lost node
// or network leaves the server's side of the connection; delay holds every byte from the server for
// a time before the listener reads it, a slow link. New connections forward normally.
type listenerRelay struct {
	listener net.Listener
	target   string
	relayed  string
	done     chan struct{}
	closing  sync.Once
	// severed is read by relay before it dials the server, so a connection accepted after sever is
	// closed rather than relayed; it is atomic because relay runs on its own goroutine per
	// connection.
	severed atomic.Bool

	mu        sync.Mutex
	trigger   []byte
	holdAfter int
	links     []*listenerRelayLink
	// refuseUntil is when a cut ends: a connection the relay accepts before it is closed at once,
	// as a refused dial would be.
	refuseUntil time.Time
	// gate is nil while the relay moves bytes and open while it is frozen: every copy waits for it
	// to close, which thawing does.
	gate chan struct{}
	// latency is how long each byte from the server waits before the listener may read it.
	latency time.Duration

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
		done:      make(chan struct{}),
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
	if r.severed.Load() {
		_ = client.Close()
		return
	}
	link := &listenerRelayLink{client: client}
	r.mu.Lock()
	if time.Now().Before(r.refuseUntil) {
		r.mu.Unlock()
		_ = client.Close()
		return
	}
	r.links = append(r.links, link)
	r.mu.Unlock()
	server, err := net.Dial("tcp", r.target)
	if err != nil {
		_ = client.Close()
		return
	}
	link.server = server
	go r.toClient(client, server)
	defer func() { _ = server.Close() }()

	buffer := make([]byte, 32*1024)
	for {
		r.pass()
		n, err := client.Read(buffer)
		if n > 0 && !link.swallow.Load() {
			if r.shouldSwallow(link, buffer[:n]) {
				link.swallow.Store(true)
				select {
				case r.swallowed <- struct{}{}:
				default:
				}
			} else {
				r.pass()
				if _, writeErr := server.Write(buffer[:n]); writeErr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// toClient copies what the server sends to the listener, each read held for the relay's latency
// as it was when the read came, in order, and none while the relay is frozen. When the server's
// side ends, what was read is still written, then the listener's side is closed.
func (r *listenerRelay) toClient(client, server net.Conn) {
	type chunk struct {
		data []byte
		due  time.Time
	}
	chunks := make(chan chunk, 1024)
	go func() {
		defer func() { _ = client.Close() }()
		failed := false
		for c := range chunks {
			if failed {
				continue
			}
			if wait := time.Until(c.due); wait > 0 {
				select {
				case <-time.After(wait):
				case <-r.done:
				}
			}
			r.pass()
			if _, err := client.Write(c.data); err != nil {
				// Keep draining, so the read below never blocks on a full channel.
				failed = true
			}
		}
	}()
	defer close(chunks)
	buffer := make([]byte, 32*1024)
	for {
		r.pass()
		n, err := server.Read(buffer)
		if n > 0 {
			r.mu.Lock()
			latency := r.latency
			r.mu.Unlock()
			chunks <- chunk{data: bytes.Clone(buffer[:n]), due: time.Now().Add(latency)}
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

// cut drops every link and refuses each new one for d, after which the listener's reconnect, which
// nats.go retries about once a second, lands on the same server again.
func (r *listenerRelay) cut(d time.Duration) {
	r.mu.Lock()
	r.refuseUntil = time.Now().Add(d)
	r.mu.Unlock()
	r.drop()
}

// freeze stops the relay moving bytes in either direction, every socket left open, and returns the
// func that thaws it, which runs once however often it is called. What either side sent meanwhile
// is delivered, in order, once the relay thaws.
func (r *listenerRelay) freeze() func() {
	r.mu.Lock()
	if r.gate == nil {
		r.gate = make(chan struct{})
	}
	gate := r.gate
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		owned := r.gate == gate
		if owned {
			r.gate = nil
		}
		r.mu.Unlock()
		if owned {
			close(gate)
		}
	})
}

// delay holds every byte the server sends for d before the listener may read it, from the next
// read on.
func (r *listenerRelay) delay(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latency = d
}

// pass waits while the relay is frozen, and returns at once once it is thawed or closed.
func (r *listenerRelay) pass() {
	r.mu.Lock()
	gate := r.gate
	r.mu.Unlock()
	if gate == nil {
		return
	}
	select {
	case <-gate:
	case <-r.done:
	}
}

// sever drops every live link as drop does and refuses every connection accepted after it, so the
// client's reconnect (MaxReconnect -1, internal/bus's options) never completes and the bus stays
// unreachable for the rest of the test. drop alone leaves the relay forwarding, which is what a
// reconnect through the same relay needs, and cut refuses connections only for a while.
func (r *listenerRelay) sever() {
	r.severed.Store(true)
	r.drop()
}

func (r *listenerRelay) close() {
	r.closing.Do(func() {
		close(r.done)
		_ = r.listener.Close()
	})
	r.drop()
}

// During the first roll onto the lease, a reconnect can interrupt the new pod's read of the durable
// while it waits out a pre-lease build's binding. The reconnect must leave the wait to go on, keep
// the role lane forwarding, log no error, and then a SIGTERM is an ordered shutdown that leaves the
// old build's binding alone.
func TestListenerReconnectWhileWaitingOutABindingShutsDownAfterForwardingRoles(t *testing.T) {
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
	listener.WaitForOutput(t, "durable is push-bound by a subscriber without the lease; waiting for its binding to end")

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
		t.Fatal("the listener's durable read never reached the controlled relay")
	}
	// One read is interrupted. Left armed, the relay would swallow the reconnected link too, as soon
	// as the wait's once-a-second polls of the durable reached the count again, and with it the role
	// lane's traffic.
	relay.arm("", 0)
	relay.drop()
	// The lease's own connection goes through the relay too and logs its reconnect under
	// "envoy-listener-lease"; the role lane holds a connection of its own (bus.ConnectRoleLane),
	// which reconnects and sends its subscription again beside the main client's.
	waitFor(t, 10*time.Second, "the listener's main client and its role lane to reconnect", func() bool {
		output := listener.Output.String()
		return strings.Contains(output, `envoy nats reconnected name="envoy-listener `) &&
			strings.Contains(output, "envoy nats reconnected name=envoy-role-lane ")
	})
	output := listener.Output.String()
	if line := firstErrorLine(output); line != "" {
		t.Fatalf("the reconnect during the wait logged ERROR: %s", line)
	}
	if strings.Contains(output, "durable receiving") {
		t.Fatalf("the listener received the durable the old build holds:\n%s", output)
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
	listener.WaitExit(t, "SIGTERM during the wait for the binding")
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
		t.Fatal("the old build's durable subscription was lost when the new pod stopped")
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
