package bus_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The listener opens its interest registry, session registry and CI store from client.Conn once,
// at start. A NATS server restart must leave that state working: the connection reconnects in
// place rather than closing, so a JetStream handle taken from it before the restart is still bound
// to a live connection afterwards.
func TestAServerRestartKeepsJetStreamStateTakenFromTheConnection(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "server-restart"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	testnats.Stop(t, ctr)
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	waitFor(t, 30*time.Second, "the client to reconnect", client.Connected)
	var putErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, putErr = kv.Put("key", []byte("value")); putErr == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("a KV handle taken before the restart still fails after the client reconnected: %v", putErr)
}

// A publish while the server is away fails, and a publish that failed is never sent later: the
// callers treat an error as not delivered (a role-lane forward as delivery_failed, a webhook as a
// 500 for the sender to retry), so a copy held and sent after reconnecting would arrive twice.
func TestAPublishThatFailedWhileReconnectingIsNeverSent(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	testnats.Stop(t, ctr)
	waitFor(t, 15*time.Second, "the client to notice the server is gone", func() bool { return !client.Connected() })
	item := contracts.Envelope{
		EventID:       "evt-publish-while-reconnecting",
		Source:        "agent",
		SourceEventID: "source-publish-while-reconnecting",
		Topic:         "notifications.role.publish-while-reconnecting",
		DedupeKey:     "publish-while-reconnecting",
		IssuedAt:      contracts.NowMillis(),
	}
	if err := client.PublishCore(item); err == nil {
		t.Fatal("a core publish while the server was away reported success")
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	observer := testnats.Connect(t, uri)
	t.Cleanup(observer.Close)
	received := make(chan *natsgo.Msg, 1)
	if _, err := observer.ChanSubscribe(item.Topic, received); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := observer.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}
	waitFor(t, 30*time.Second, "the client to reconnect", client.Connected)
	select {
	case message := <-received:
		t.Fatalf("the publish that failed was sent after the reconnect: %s", message.Data)
	case <-time.After(2 * time.Second):
	}
}

// A publish while the connection is reconnecting waits for the reconnect, within its own deadline,
// rather than failing at once: a webhook that gets an error answers 503, and the sender does not
// redeliver, so a failure fast enough to beat a one-second NATS restart loses the event.
func TestAPublishWhileReconnectingWaitsForTheReconnect(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	testnats.Stop(t, ctr)
	waitFor(t, 15*time.Second, "the client to start reconnecting", func() bool { return client.Conn.IsReconnecting() })
	restarted := make(chan error, 1)
	go func() {
		time.Sleep(time.Second)
		restarted <- ctr.Start(context.Background())
	}()
	item := contracts.Envelope{
		EventID:        "evt-publish-across-restart",
		Source:         "github",
		SourceEventID:  "source-publish-across-restart",
		Topic:          "notifications.github.acme.widgets.push.branch.main",
		DedupeKey:      "publish-across-restart",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "publish across a NATS restart",
		TraceID:        "trace-publish-across-restart",
	}
	publishErr := client.Publish(item)
	if err := <-restarted; err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	if publishErr != nil {
		t.Fatalf("a JetStream publish during a one-second NATS restart failed: %v", publishErr)
	}
}

// A connection that reconnects while Drain is still draining belongs to a process that is shutting
// down: the reconnect closes it, quietly, instead of re-subscribing or logging a failure.
func TestAReconnectDuringDrainClosesQuietly(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{}, 1)
	if _, err := client.SubscribeCore("notifications.role.reconnect-during-drain", func(*natsgo.Msg) {
		started <- struct{}{}
		time.Sleep(20 * time.Second)
	}, "reconnect-during-drain"); err != nil {
		t.Fatalf("role subscribe: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.reconnect-during-drain", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached its handler")
	}

	logs := captureBusLogs(t)
	drained := make(chan error, 1)
	go func() { drained <- client.Drain(15 * time.Second) }()
	time.Sleep(200 * time.Millisecond)
	testnats.Stop(t, ctr)
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	select {
	case <-drained:
	case <-time.After(14 * time.Second):
		t.Fatal("the reconnect during the drain did not end it before its deadline")
	}
	if !client.Conn.IsClosed() {
		t.Fatal("the drain ended with the reconnected connection still open")
	}
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a reconnect during the drain logged an error: %s", line)
	}
}

// busLogs collects the default slog logger's JSON records for one test.
type busLogs struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *busLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *busLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

func (l *busLogs) errorLine() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range strings.Split(l.buffer.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			return line
		}
	}
	return ""
}

func captureBusLogs(t *testing.T) *busLogs {
	t.Helper()
	logs := &busLogs{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// A reconnect in place keeps every subscription nats.go restored. After a server restart, nats.go
// re-sends each subscription's SUB on the connection it reconnected, so the subscription Subscribe
// returned still delivers, the durable's from the consumer the server kept on its file store;
// tearing it down to bind the consumer again races the server's own release of the consumer's
// push binding, which refuses the new bind with "consumer is already bound to a subscription" and
// logs a resubscribe failure at ERROR (LEGION-278).
func TestAReconnectInPlaceKeepsTheSubscriptionsNATSRestored(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	var durableDeliveries, roleDeliveries atomic.Int32
	durable, err := subscribeAllNotifications(client, func(message *natsgo.Msg) {
		durableDeliveries.Add(1)
		_ = message.Ack()
	}, natsgo.Durable("in-place-reconnect"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("durable subscribe: %v", err)
	}
	const roleSubject = "notifications.role.in-place-reconnect"
	role, err := client.SubscribeCore(roleSubject, func(*natsgo.Msg) { roleDeliveries.Add(1) }, "in-place-reconnect")
	if err != nil {
		t.Fatalf("role subscribe: %v", err)
	}
	reconnected := make(chan struct{}, 1)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		reconnected <- struct{}{}
		return nil
	})

	logs := captureBusLogs(t)
	testnats.Stop(t, ctr)
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	select {
	case <-reconnected:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect never finished restoring the subscriptions")
	}
	if !durable.IsValid() || !role.IsValid() {
		t.Fatalf("a reconnect in place tore down the subscriptions Subscribe returned (durable valid %t, role valid %t)", durable.IsValid(), role.IsValid())
	}

	// The restarted server answers core NATS before JetStream is ready, so the publish is retried
	// until the stream acknowledges it. The message id makes a retry after a late acknowledgement
	// a duplicate the stream drops, so the stream holds the message once either way.
	var publishErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, publishErr = client.JS().Publish("notifications.github.test.in-place-reconnect", []byte(`{}`), natsgo.MsgId("in-place-reconnect")); publishErr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if publishErr != nil {
		t.Fatalf("publish to the durable after the restart: %v", publishErr)
	}
	if err := client.Conn.Publish(roleSubject, []byte(`{}`)); err != nil {
		t.Fatalf("publish to the role lane: %v", err)
	}
	waitFor(t, 10*time.Second, "both deliveries after the restart", func() bool {
		return durableDeliveries.Load() >= 1 && roleDeliveries.Load() >= 1
	})
	time.Sleep(time.Second)
	if durable, role := durableDeliveries.Load(), roleDeliveries.Load(); durable != 1 || role != 1 {
		t.Fatalf("deliveries after the restart: durable %d, role %d, want one each", durable, role)
	}
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a reconnect in place logged an error: %s", line)
	}
}

// A reconnect hook that fails after the client was stopped fails because of the stop: a SIGTERM
// that lands while the listener's rewatch hook runs closes the connection the hook is reading
// through. recover already reports that case as the stop; the reconnect path must too, at INFO
// with the error kept on the line, never as a hook failure at ERROR.
func TestAReconnectHookFailingAfterTheStopIsTheStop(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	hooked := make(chan struct{}, 1)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		// The shutdown begins while the hook is still running, and the hook's next server request
		// fails on the closed connection.
		client.Close()
		hooked <- struct{}{}
		return errors.New("rewatch interest registry: nats: connection closed")
	})

	logs := captureBusLogs(t)
	testnats.Stop(t, ctr)
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect never ran its hook")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "reconnect hooks cancelled") && logs.errorLine() == "" {
		time.Sleep(50 * time.Millisecond)
	}
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a hook failing after the stop logged an error: %s", line)
	}
	if !strings.Contains(logs.String(), "envoy nats reconnect hooks cancelled") || !strings.Contains(logs.String(), "connection closed") {
		t.Fatalf("the stop was not reported with its error at INFO: %s", logs.String())
	}
}
