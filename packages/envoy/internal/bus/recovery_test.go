package bus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// startNATS launches a real NATS container and returns it with the connection
// URI. The container is terminated when the test completes.
func startNATS(t *testing.T) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctr, uri := testnats.Start(t)
	return ctr, uri
}

// waitFor polls a condition with timeout. Returns true if condition was met.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

func subscribeAllNotifications(client *bus.Client, handler natsgo.MsgHandler, opts ...natsgo.SubOpt) (*natsgo.Subscription, error) {
	subOpts := append([]natsgo.SubOpt{
		natsgo.BindStream(bus.Stream),
		natsgo.ConsumerFilterSubjects(bus.StreamSubjects()...),
	}, opts...)
	return client.Subscribe("", handler, subOpts...)
}

// TestSubOK_NoSubscription verifies SubOK is false when no subscription exists.
func TestSubOK_NoSubscription(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if client.SubOK() {
		t.Fatal("SubOK should be false without a subscription")
	}
}

func TestPublishBoundsReconnectWhenNATSUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		topic   string
		publish func(*bus.Client, contracts.Envelope) error
	}{
		{
			name:  "jetstream",
			topic: "notifications.github.acme.widgets.timeout",
			publish: func(client *bus.Client, item contracts.Envelope) error {
				return client.Publish(item)
			},
		},
		{
			name:  "core",
			topic: "notifications.role.legion-timeout",
			publish: func(client *bus.Client, item contracts.Envelope) error {
				return client.PublishCore(item)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctr, uri := startNATS(t)
			client, err := bus.ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)

			ctx := context.Background()
			if err := ctr.Terminate(ctx); err != nil {
				t.Fatalf("stop NATS: %v", err)
			}
			client.Conn.Close()

			result := make(chan error, 1)
			started := time.Now()
			go func() {
				result <- tc.publish(client, contracts.Envelope{
					EventID:        "evt-publish-reconnect-timeout-" + tc.name,
					Source:         "agent",
					SourceEventID:  "source-publish-reconnect-timeout-" + tc.name,
					Topic:          tc.topic,
					DedupeKey:      "publish-reconnect-timeout-" + tc.name,
					IssuedAt:       contracts.NowMillis(),
					PayloadSummary: "timeout",
					TraceID:        "trace-publish-reconnect-timeout-" + tc.name,
				})
			}()

			select {
			case err := <-result:
				if err == nil {
					t.Fatal("publish unexpectedly succeeded without NATS")
				}
				if elapsed := time.Since(started); elapsed > 6*time.Second {
					t.Fatalf("publish returned after %s, want a bounded reconnect within 6s", elapsed)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("publish remained blocked after the reconnect deadline")
			}
		})
	}
}

func TestCoreSubscriptionRestoresAfterConnectionRecovery(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	publisher := testnats.Connect(t, uri)
	defer publisher.Close()

	var received atomic.Int32
	_, err = client.SubscribeCore("notifications.role.recovery", func(*natsgo.Msg) {
		received.Add(1)
	})
	if err != nil {
		t.Fatalf("subscribe core role lane: %v", err)
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush core publisher: %v", err)
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "core role subscription to recover", func() bool {
		if err := publisher.Publish("notifications.role.recovery", []byte("recovered")); err != nil {
			t.Fatalf("publish to recovered core role lane: %v", err)
		}
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush recovered core role publish: %v", err)
		}
		return received.Load() > 0
	})
}

// While a listener waits out the task that holds its durable, its JetStream subscription is
// registered but unbound and its core role lane already delivers. A recovery onto a new connection
// in that window must restore the role lane though the durable still refuses the bind: the role
// lane is a queue member a role message can be handed to, and nothing else re-subscribes it.
func TestACoreSubscriptionRecoversWhileTheJetStreamOneCannotBind(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	publisher := testnats.Connect(t, uri)
	defer publisher.Close()
	waitOutHeldDurable(t, client, uri)

	var received atomic.Int32
	if _, err := client.SubscribeCore("notifications.role.held-durable", func(*natsgo.Msg) { received.Add(1) }); err != nil {
		t.Fatalf("subscribe core role lane: %v", err)
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "core role subscription to recover beside a durable that refuses the bind", func() bool {
		if err := publisher.Publish("notifications.role.held-durable", []byte("recovered")); err != nil {
			t.Fatalf("publish to the core role lane: %v", err)
		}
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush the core role publish: %v", err)
		}
		return received.Load() > 0
	})
	if client.SubOK() {
		t.Fatal("SubOK reports the durable bound while another connection holds it")
	}
}

// The reconnect hooks re-point state no subscription holds, the listener's interest, session and
// role stores among it, at the connection the bus now has. During the bind wait /v1 and the role
// lane already read those stores while the durable's restore is refused by design, so the hooks
// run once at each reconnect and on each replacement connection whatever that restore returned:
// gated on it, a replaced connection left every store on the closed one until the durable bound.
// Recovery keeps retrying the durable's restore, and does not run the hooks again for a connection
// they already moved to.
func TestReconnectHooksRunWhileTheDurableCannotBind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reconnect func(*bus.Client) error
	}{
		{name: "in place", reconnect: func(client *bus.Client) error { return client.Conn.ForceReconnect() }},
		{name: "replaced connection", reconnect: func(client *bus.Client) error {
			client.Conn.Close()
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, uri := startNATS(t)
			client, err := bus.ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer client.Close()
			waitOutHeldDurable(t, client, uri)
			hooked := make(chan *natsgo.Conn, 8)
			client.AddReconnectHook(func(conn *natsgo.Conn) error {
				hooked <- conn
				return nil
			})

			if err := tc.reconnect(client); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			select {
			case conn := <-hooked:
				if conn != client.Conn {
					t.Fatal("the reconnect hook ran on a connection the client no longer has")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the reconnect hook never ran while the durable refuses the bind")
			}
			if client.SubOK() {
				t.Fatal("SubOK reports the durable bound while another connection holds it")
			}
			// Recovery's next attempts come 1 s and 3 s after its first.
			time.Sleep(2500 * time.Millisecond)
			if runs := len(hooked); runs != 0 {
				t.Fatalf("the reconnect hook ran %d more time(s) for one reconnect", runs)
			}
		})
	}
}

// The reconnect hooks run one at a time, and a connection event that lands between runs gets a run
// of its own: a recovery that finds a run in progress waits for it, and starts no run for an event
// that run covered. Here a recovery waiting out a held durable runs them on the connection it
// dialed to replace a closed one, and an in-place reconnect of that connection right after the run
// ends runs them again, while the recovery's next attempt, a second later, lands during the
// reconnect's run. Two events between runs, two runs, never two at once: a recovery that read the
// hooks as due while the reconnect's run was still in progress started a third, alongside it.
func TestReconnectHooksRunOneAtATimeOnceForEachEventBetweenRuns(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	waitOutHeldDurable(t, client, uri)
	// Longer than the recovery's first backoff (1 s), so its second attempt comes during the run
	// the in-place reconnect starts as the first run ends.
	const hookTakes = 3 * time.Second
	var runs, running, mostAtOnce atomic.Int32
	ended := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		runs.Add(1)
		now := running.Add(1)
		for most := mostAtOnce.Load(); now > most; most = mostAtOnce.Load() {
			if mostAtOnce.CompareAndSwap(most, now) {
				break
			}
		}
		time.Sleep(hookTakes)
		running.Add(-1)
		ended <- struct{}{}
		return nil
	})

	client.Conn.Close()
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the hooks never ran on the connection the recovery dialed")
	}
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect the replacement in place: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the hooks never ran for the in-place reconnect")
	}
	// The recovery's attempt that came during the reconnect's run, and its next one 2 s later, have
	// both been made by now.
	time.Sleep(hookTakes)
	if got, most := runs.Load(), mostAtOnce.Load(); got != 2 || most != 1 {
		t.Fatalf("the hooks ran %d time(s) for two connection events between runs, at most %d at once; want 2 runs, one at a time", got, most)
	}
	if client.SubOK() {
		t.Fatal("SubOK reports the durable bound while another connection holds it")
	}
}

// A run of the reconnect hooks that fails is run again by a recovery, whatever restoring the
// subscriptions returned: an in-place reconnect that restored every subscription has no other
// reason to start one, so without it a failed run stayed failed, the state the hooks move left on
// what it last followed, until the next connection event. Here the hooks fail once after an
// in-place reconnect and run again with no other connection event.
func TestAFailedRunOfTheReconnectHooksRunsAgainWithoutAnotherConnectionEvent(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	ran := make(chan *natsgo.Conn, 8)
	var runs atomic.Int32
	client.AddReconnectHook(func(conn *natsgo.Conn) error {
		ran <- conn
		if runs.Add(1) == 1 {
			return errors.New("the first run fails")
		}
		return nil
	})

	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	for run := 1; run <= 2; run++ {
		select {
		case conn := <-ran:
			if conn != client.Conn {
				t.Fatalf("run %d of the reconnect hooks was on a connection the client no longer has", run)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("the reconnect hooks ran %d time(s) after an in-place reconnect whose first run failed; want them run again", run-1)
		}
	}
	// The run that succeeded covers the reconnect, so nothing runs the hooks again.
	time.Sleep(2500 * time.Millisecond)
	if extra := len(ran); extra != 0 {
		t.Fatalf("the reconnect hooks ran %d more time(s) after a run succeeded", extra)
	}
	if reconnects := client.Conn.Stats().Reconnects; reconnects != 1 {
		t.Fatalf("the connection reconnected %d time(s); want the one in-place reconnect, so no other connection event ran the hooks", reconnects)
	}
}

// A failure that wants a recovery while one is ending is not left to the next connection event:
// the caller finds the recovery's flag still set and starts none, so the recovery looks once more
// after it clears the flag. Here the connection a recovery dialed closes while that recovery's run
// of the hooks is finishing, a run that had made its requests and returns success: the closed
// connection's own recovery finds the first still running, and before the first looked again the
// client stayed on a closed connection for good.
func TestAConnectionThatClosesAsARecoveryEndsIsRecovered(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	running := make(chan *natsgo.Conn, 1)
	finish := make(chan struct{})
	var runs atomic.Int32
	client.AddReconnectHook(func(conn *natsgo.Conn) error {
		if runs.Add(1) == 1 {
			running <- conn
			<-finish
		}
		return nil
	})

	client.Conn.Close()
	var dialed *natsgo.Conn
	select {
	case dialed = <-running:
	case <-time.After(30 * time.Second):
		t.Fatal("the recovery never ran the hooks on the connection it dialed")
	}
	dialed.Close()
	// The closed callback runs on the connection's callback goroutine and starts its recovery at
	// once; a second is ample for that recovery to find the first still running.
	time.Sleep(time.Second)
	close(finish)
	waitFor(t, 15*time.Second, "the client to replace the connection that closed as the recovery ended", client.Connected)
	if client.Conn == dialed {
		t.Fatal("the client still holds the connection that closed")
	}
}

// While a bind's request is in flight, an in-place reconnect's restore and its run of the
// reconnect hooks do not wait for it. A reconnect can lose that request, which JetStream then waits
// out for 10 s, and a listener's bind attempts run every 2 s through its bind wait; a restore that
// waited for the bind left the stores /v1 and the role lane read on the old watchers for those 10 s.
// The registration that bind leaves is not lost and not bound twice: the recovery the reconnect
// starts binds it once the bind in flight has failed, and a message reaches its handler once.
func TestAnInPlaceReconnectDoesNotWaitForABindInFlight(t *testing.T) {
	_, uri := startNATS(t)
	proxy, relayed := startSwallowingProxy(t, uri, "$JS.API.CONSUMER.INFO.")
	client, err := bus.ConnectOwningStream([]string{relayed})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	const consumer = "bind-in-flight"
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: natsgo.NewInbox(),
		AckPolicy:      natsgo.AckExplicitPolicy,
		FilterSubjects: bus.StreamSubjects(),
	}); err != nil {
		t.Fatalf("add the durable: %v", err)
	}
	hooked := make(chan time.Time, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- time.Now()
		return nil
	})
	var delivered atomic.Int32
	type bindResult struct {
		err error
		at  time.Time
	}
	bound := make(chan bindResult, 1)

	proxy.arm()
	go func() {
		_, err := client.Subscribe("", func(msg *natsgo.Msg) {
			delivered.Add(1)
			_ = msg.Ack()
		}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
		bound <- bindResult{err: err, at: time.Now()}
	}()
	select {
	case <-proxy.swallowed:
	case <-time.After(10 * time.Second):
		t.Fatal("the bind's consumer lookup never reached the link")
	}
	dropped := time.Now()
	proxy.drop()

	var hookedAt time.Time
	select {
	case hookedAt = <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect hooks never ran")
	}
	var bind bindResult
	select {
	case bind = <-bound:
	case <-time.After(30 * time.Second):
		t.Fatal("the bind whose lookup was lost never returned")
	}
	if bind.err == nil {
		t.Fatal("the bind whose lookup the link lost succeeded, so nothing was in flight across the reconnect")
	}
	if !hookedAt.Before(bind.at) {
		t.Fatalf("the reconnect hooks ran %s after the link dropped, %s after the bind in flight gave up (%v); want them run while it was still in flight",
			hookedAt.Sub(dropped).Round(time.Millisecond), hookedAt.Sub(bind.at).Round(time.Millisecond), bind.err)
	}

	waitFor(t, 30*time.Second, "the recovery to bind the registration the lost bind left", client.SubOK)
	data, _ := json.Marshal(map[string]string{"test": "bind-in-flight"})
	if _, err := client.JS().Publish("notifications.github.test.bind-in-flight", data); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 5*time.Second, "the message to reach the bound registration", func() bool { return delivered.Load() >= 1 })
	time.Sleep(2 * time.Second)
	if count := delivered.Load(); count != 1 {
		t.Fatalf("the message reached the handler %d times; want once, from one binding", count)
	}
}

// swallowingProxy relays NATS connections to a server. Once armed, it forwards nothing more the
// client sends on the connections it relays at that moment, and reports on swallowed when what it
// held back carried its trigger: a request the client wrote is lost on its way to the server, as
// one in flight when a link fails is. drop closes those connections, so nats.go reconnects in place,
// through a relay that forwards again.
type swallowingProxy struct {
	listener  net.Listener
	target    string
	trigger   []byte
	swallowed chan struct{}

	mu    sync.Mutex
	links []*swallowedLink
}

type swallowedLink struct {
	client, server net.Conn
	swallow        atomic.Bool
}

// startSwallowingProxy relays to the server uri names and returns the proxy with uri pointed at it.
func startSwallowingProxy(t *testing.T, uri, trigger string) (*swallowingProxy, string) {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %q: %v", uri, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &swallowingProxy{listener: listener, target: server.Host, trigger: []byte(trigger), swallowed: make(chan struct{}, 1)}
	t.Cleanup(p.close)
	go p.serve()
	relayed := *server
	relayed.Host = listener.Addr().String()
	return p, relayed.String()
}

func (p *swallowingProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		link := &swallowedLink{client: client, server: server}
		p.mu.Lock()
		p.links = append(p.links, link)
		p.mu.Unlock()
		go func() {
			_, _ = io.Copy(client, server)
			_ = client.Close()
		}()
		go p.forward(link)
	}
}

// forward copies what the client sends to the server until either side ends, holding it back
// while the link swallows.
func (p *swallowingProxy) forward(link *swallowedLink) {
	defer func() { _ = link.server.Close() }()
	buf := make([]byte, 32*1024)
	var held []byte
	for {
		n, err := link.client.Read(buf)
		if n > 0 {
			if link.swallow.Load() {
				held = append(held, buf[:n]...)
				if bytes.Contains(held, p.trigger) {
					select {
					case p.swallowed <- struct{}{}:
					default:
					}
				}
			} else if _, err := link.server.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *swallowingProxy) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, link := range p.links {
		link.swallow.Store(true)
	}
}

func (p *swallowingProxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, link := range p.links {
		_ = link.client.Close()
		_ = link.server.Close()
	}
	p.links = nil
}

func (p *swallowingProxy) close() {
	_ = p.listener.Close()
	p.drop()
}

// waitOutHeldDurable puts client where a listener waiting out the task that holds its durable is:
// a durable bound from another connection on uri, and client's own bind of it refused, which
// leaves its JetStream subscription registered but unbound.
func waitOutHeldDurable(t *testing.T, client *bus.Client, uri string) {
	t.Helper()
	const consumer = "held-elsewhere"
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: natsgo.NewInbox(),
		AckPolicy:      natsgo.AckExplicitPolicy,
		FilterSubjects: bus.StreamSubjects(),
	}); err != nil {
		t.Fatalf("add the durable: %v", err)
	}
	holderConn := testnats.Connect(t, uri)
	t.Cleanup(holderConn.Close)
	holderJS, err := holderConn.JetStream()
	if err != nil {
		t.Fatalf("holder JetStream: %v", err)
	}
	if _, err := holderJS.Subscribe("", func(msg *natsgo.Msg) { _ = msg.Ack() }, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck()); err != nil {
		t.Fatalf("bind the durable as the other task: %v", err)
	}
	if err := holderConn.Flush(); err != nil {
		t.Fatalf("flush the holder: %v", err)
	}
	if _, err := client.Subscribe("", func(msg *natsgo.Msg) { _ = msg.Ack() }, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck()); err == nil {
		t.Fatal("bound a durable another connection holds")
	}
}

func TestCoreSubscriptionsShareQueueGroup(t *testing.T) {
	_, uri := startNATS(t)
	first, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect first client: %v", err)
	}
	defer first.Close()
	second, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect second client: %v", err)
	}
	defer second.Close()
	publisher := testnats.Connect(t, uri)
	defer publisher.Close()

	var received atomic.Int32
	handler := func(*natsgo.Msg) { received.Add(1) }
	const subject = "notifications.role.queue-group"
	const queue = "envoy-listener-test-machine"
	if _, err := first.SubscribeCore(subject, handler, queue); err != nil {
		t.Fatalf("subscribe first core role lane: %v", err)
	}
	if _, err := second.SubscribeCore(subject, handler, queue); err != nil {
		t.Fatalf("subscribe second core role lane: %v", err)
	}
	// A SUB is asynchronous on the wire: flush the subscribing connections, not
	// the publisher's, so the server holds both queue members before the first
	// publish. Core NATS drops a message with no subscriber.
	for name, client := range map[string]*bus.Client{"first": first, "second": second} {
		if err := client.Conn.Flush(); err != nil {
			t.Fatalf("flush %s subscription: %v", name, err)
		}
	}
	for range 3 {
		if err := publisher.Publish(subject, []byte("queued")); err != nil {
			t.Fatalf("publish queue test message: %v", err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush queue test messages: %v", err)
	}
	waitFor(t, 5*time.Second, "one queue delivery per role event", func() bool {
		return received.Load() == 3
	})
}

// TestSubOK_AfterSubscribe verifies SubOK is true after subscribing.
func TestSubOK_AfterSubscribe(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		_ = msg.Ack()
	}, natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if !client.SubOK() {
		t.Fatal("SubOK should be true after Subscribe")
	}
}

// TestRecovery_ClosedTriggersWithoutPublish verifies that when the NATS
// connection enters CLOSED state (without any Publish call), the ClosedCB
// triggers recovery and the subscription is restored. After recovery,
// messages flow normally through the restored subscription.
func TestRecovery_ClosedTriggersWithoutPublish(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var received atomic.Int32
	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Durable("recovery-test"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if !client.SubOK() {
		t.Fatal("SubOK should be true before disconnect")
	}

	// Force CLOSED state by closing the underlying connection directly.
	// This triggers ClosedCB → onClosed → go recover().
	// Note: we call Conn.Close() on the underlying nats.Conn, NOT client.Close()
	// which would also close stopCh and prevent recovery.
	client.Conn.Close()

	// SubOK should become false during recovery.
	time.Sleep(100 * time.Millisecond)

	// Wait for recovery to complete (recover creates new connection + resubscribes).
	waitFor(t, 30*time.Second, "SubOK to become true after recovery", client.SubOK)

	// Publish a message through the recovered connection and verify delivery.
	// This proves the subscription is functional after recovery — no Publish call
	// was needed to trigger recovery (the ClosedCB did it).
	data, _ := json.Marshal(map[string]string{"test": "recovery"})
	_, err = client.JS().Publish("notifications.github.test.recovery", data)
	if err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}

	waitFor(t, 5*time.Second, "message delivered after recovery", func() bool {
		return received.Load() >= 1
	})
}

// TestRecovery_AtMostOneSubscription verifies that after recovery, only one
// subscription callback is active (no duplicate message delivery).
func TestRecovery_AtMostOneSubscription(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var received atomic.Int32
	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Durable("dedup-sub-test"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Trigger recovery by closing the underlying connection.
	client.Conn.Close()
	waitFor(t, 30*time.Second, "first recovery", client.SubOK)

	// Trigger recovery again.
	client.Conn.Close()
	waitFor(t, 30*time.Second, "second recovery", client.SubOK)

	// Reset counter and publish a single message.
	received.Store(0)
	data, _ := json.Marshal(map[string]string{"test": "dedup"})
	_, err = client.JS().Publish("notifications.github.test.dedup", data)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for delivery.
	waitFor(t, 5*time.Second, "message delivered", func() bool {
		return received.Load() >= 1
	})

	// Give extra time to detect any duplicate delivery from stale subscriptions.
	time.Sleep(2 * time.Second)
	if count := received.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery (at-most-one subscription), got %d", count)
	}
}

// TestRecovery_ConcurrentRecoverySerializes verifies that multiple concurrent
// calls to recovery (e.g., ClosedCB firing while recovery is already running)
// do not spawn competing retry goroutines. The atomic recovering flag ensures
// at most one recovery goroutine runs at a time.
func TestRecovery_ConcurrentRecoverySerializes(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var received atomic.Int32
	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Durable("concurrent-test"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Trigger multiple recovery cycles in rapid succession. Each cycle closes
	// the connection, waits for recovery, then immediately triggers another.
	// This verifies recovery is resilient to repeated CLOSED transitions and
	// doesn't accumulate stale subscriptions or leak goroutines.
	for range 3 {
		client.Conn.Close()
		waitFor(t, 30*time.Second, "recovery between rapid closes", client.SubOK)
	}

	// After all recovery cycles, publish and verify single delivery.
	received.Store(0)
	data, _ := json.Marshal(map[string]string{"test": "concurrent"})
	_, err = client.JS().Publish("notifications.github.test.concurrent", data)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 5*time.Second, "delivery after recovery cycles", func() bool {
		return received.Load() >= 1
	})
	time.Sleep(2 * time.Second)
	if count := received.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery after recovery cycles, got %d", count)
	}
}

func TestStreamSubjectsIncludesDispatchEvents(t *testing.T) {
	for _, subject := range bus.StreamSubjects() {
		if subject == "notifications.dispatch.>" {
			return
		}
	}
	t.Fatal("dispatch notification subject is not retained by the stream")
}

func TestConnectedReportsLiveConnection(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if !client.Connected() {
		t.Fatal("connected client reported unhealthy")
	}
}

func TestReconnectHookRunsAfterReconnect(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	hookRan := make(chan *natsgo.Conn, 1)
	client.AddReconnectHook(func(conn *natsgo.Conn) error {
		hookRan <- conn
		return nil
	})
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("force reconnect: %v", err)
	}
	select {
	case conn := <-hookRan:
		if conn != client.Conn {
			t.Fatal("reconnect hook received a stale connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect hook did not run")
	}
}

func TestJetStreamPublishDeduplicatesAStableEnvelopeDestination(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	item := contracts.Envelope{
		EventID:        "dispatch-42",
		Source:         "dispatch",
		SourceEventID:  "42",
		Topic:          "notifications.dispatch.issue.TEST-1.ask.answered",
		DedupeKey:      "dispatch-42",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "TEST-1 ask answered",
		TraceID:        "dispatch-42",
	}
	if err := client.Publish(item); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if err := client.Publish(item); err != nil {
		t.Fatalf("retry publish: %v", err)
	}

	info, err := client.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("retained messages = %d, want one after a retry with the same destination", info.State.Msgs)
	}
}

// The Go Legion daemon publishes its controller notices on notifications.legion.<project>.controller,
// one name of the notifications.legion.<project>.<name> family, through the listener's publish route. That route publishes to JetStream, so the stream must carry
// the subject; outside every stream subject JetStream has no responder and the route answers 500.
// The retained copy is not a replay for a pane: a pane subscribes over core NATS and gets only what
// is published while it is subscribed.
func TestJetStreamPublishRetainsLegionIssueNotices(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	item := contracts.Envelope{
		EventID:        "notice-42",
		Source:         "agent",
		SourceEventID:  "notice-42",
		Topic:          "notifications.legion.omp.LEGION-208",
		DedupeKey:      "legion-outbox:42",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "pr-blocked on LEGION-208",
		TraceID:        "notice-42",
	}
	if err := client.Publish(item); err != nil {
		t.Fatalf("publish Legion notice: %v", err)
	}
	retained, err := client.JS().GetLastMsg(bus.Stream, item.Topic)
	if err != nil {
		t.Fatalf("read retained Legion notice: %v", err)
	}
	var got contracts.Envelope
	if err := json.Unmarshal(retained.Data, &got); err != nil {
		t.Fatalf("decode retained Legion notice: %v", err)
	}
	if got.EventID != item.EventID {
		t.Fatalf("retained notice event = %q, want %q", got.EventID, item.EventID)
	}
}
