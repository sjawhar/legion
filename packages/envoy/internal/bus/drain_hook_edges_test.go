package bus_test

// Every test here captures the default slog handler (captureBusLogs, restart_test.go), a
// process-wide swap, so none of them is parallel (dispatch://INFRA-1075).

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// watchedBucket opens a bucket on client's connection and returns a watcher on it that has
// finished its first scan, with the server's own view of the bucket's consumers through admin.
func watchedBucket(t *testing.T, client *bus.Client, uri, bucket string) (*kvwatch.Watcher, func() []string) {
	t.Helper()
	kv, err := bus.EnsureKeyValue(client.JS(), &natsgo.KeyValueConfig{Bucket: bucket})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	watcher := kvwatch.New("drain-test", kv, func(natsgo.KeyValueEntry) {}, func() {})
	watcher.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := watcher.WaitReady(ctx); err != nil {
		t.Fatalf("wait for the watcher: %v", err)
	}
	admin := testnats.Connect(t, uri)
	adminJS, err := admin.JetStream()
	if err != nil {
		t.Fatalf("admin JetStream: %v", err)
	}
	consumers := func() []string { return consumerNames(adminJS, "KV_"+bucket) }
	if got := consumers(); len(got) != 1 {
		t.Fatalf("consumers before the drain = %v, want the watcher's one", got)
	}
	return watcher, consumers
}

// heldDelivery is a core-NATS handler a test holds inside its delivery: it reports that the
// delivery reached it, then waits until the test ends. A drain that must find a delivery in its
// handler finds one for as long as the drain takes, rather than for a span of time a sleep
// guessed at, and the handler is released before the client is closed (the cleanups run in
// reverse, and the caller registers the client's close first).
func heldDelivery(t *testing.T) (func(*natsgo.Msg), <-chan struct{}) {
	t.Helper()
	begun := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(*natsgo.Msg) {
		close(begun)
		<-release
	}, begun
}

// A connection lost while the drain hooks run leaves each watcher's consumer delete unsent, or
// sent with no answer coming: nats.go refuses a publish while reconnecting with the reconnect
// buffer off ("outbound buffer limit exceeded"), and a request already sent waits out its context.
// Neither is a failure of the watcher: the server deletes an ordered consumer whose interest is
// gone on its own (its inactive threshold, 5 s for an ephemeral), so the end is a WARN that says
// the delete was left to the server, never an ERROR.
func TestADrainHookLosingTheConnectionWarns(t *testing.T) {
	for _, test := range []struct {
		name string
		// lose cuts the link at the point the case names and returns once nats.go reconnects.
		lose func(t *testing.T, proxy *relayProxy, client *bus.Client) func()
	}{
		{
			name: "the delete is sent while reconnecting",
			lose: func(t *testing.T, proxy *relayProxy, client *bus.Client) func() {
				return func() {
					proxy.refuse.Store(true)
					proxy.drop()
					waitFor(t, 5*time.Second, "the client to start reconnecting", client.Conn.IsReconnecting)
				}
			},
		},
		{
			name: "the link drops with the delete in flight",
			lose: func(t *testing.T, proxy *relayProxy, client *bus.Client) func() {
				return func() {
					proxy.arm("$JS.API.CONSUMER.DELETE.")
					go func() {
						select {
						case <-proxy.swallowed:
						case <-time.After(5 * time.Second):
							return
						}
						proxy.refuse.Store(true)
						proxy.drop()
					}()
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			proxy := startRelayProxy(t, uri)
			logs := captureBusLogs(t)
			name := t.Name()
			client, err := bus.ConnectOwningStream([]string{proxy.url()}, bus.WithReplicas(1), bus.WithName(name))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			watcher, consumers := watchedBucket(t, client, uri, "drain_hook_lost")
			lose := test.lose(t, proxy, client)
			client.AddDrainHook(func(ctx context.Context) {
				lose()
				watcher.End(ctx)
			})
			watcher.Stop()
			_ = client.Drain(3 * time.Second)
			time.Sleep(200 * time.Millisecond)
			records := warnOrErrorRecords(t, logs, name)
			for _, record := range records {
				if strings.HasPrefix(record, "ERROR") {
					t.Fatalf("a connection lost during the drain hooks logged %q", records)
				}
			}
			if got, want := strings.Join(records, ", "), "WARN drain-test watcher's consumer was left to the server: the connection was lost before its delete"; got != want {
				t.Fatalf("the drain logged %q, want %q", got, want)
			}
			waitFor(t, 15*time.Second, "the server to delete the consumer it lost interest in", func() bool { return len(consumers()) == 0 })
		})
	}
}

// A delivery that finishes just before the drain's deadline leaves the hooks their share of it:
// the delivery subscriptions drain within all but the drain's last 3 s, or its last 30% when that
// is less, so the hooks and the connection's drain always keep it, however long a delivery takes.
// Of the 300 ms that share is of a 1 s drain, the test asks for half, which leaves the scheduler
// the other half.
func TestASlowDeliveryLeavesTheDrainHooksTheirShare(t *testing.T) {
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	hold, started := heldDelivery(t)
	if _, err := client.SubscribeCore("notifications.role.drain-slow", hold, "drain-slow"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-slow", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached its handler")
	}
	left := make(chan time.Duration, 1)
	client.AddDrainHook(func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		left <- time.Until(deadline)
	})
	_ = client.Drain(time.Second)
	select {
	case remaining := <-left:
		if remaining < 150*time.Millisecond {
			t.Fatalf("the drain hooks ran with %s of the drain's 1s left, want at least 150ms of the 300ms share they keep", remaining)
		}
	default:
		t.Fatal("the drain never ran its hooks")
	}
}

// The listener drains for 13 s, and its deliveries keep the 10 s they had before the drain hooks:
// a delivery that takes 9.5 s finishes, the drain reports no error, and the hooks still have their
// 3 s once it has (bus's defaultDrainHookReserve). The same division runs here on a drain of two
// seconds, which the client's reserve is set a quarter of: the deliveries keep 1.5 s of it, the
// delivery takes 1 s, and the hooks run with the reserve and the half second the delivery left.
// The delivery's half-second margin inside its share is the one the 9.5 s delivery had of its
// 10 s, so a loaded machine is no likelier to miss it here.
func TestADeliveryKeepsTenSecondsOfTheListenersDrain(t *testing.T) {
	const reserve = 500 * time.Millisecond
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithDrainHookReserve(reserve))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{})
	var delivered atomic.Bool
	if _, err := client.SubscribeCore("notifications.role.drain-window", func(*natsgo.Msg) {
		close(started)
		time.Sleep(time.Second)
		delivered.Store(true)
	}, "drain-window"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-window", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached its handler")
	}
	type observed struct {
		delivered bool
		left      time.Duration
	}
	ran := make(chan observed, 1)
	client.AddDrainHook(func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		ran <- observed{delivered: delivered.Load(), left: time.Until(deadline)}
	})
	if err := client.Drain(4 * reserve); err != nil {
		t.Fatalf("a 2 s drain of a 1 s delivery returned %v, want none", err)
	}
	select {
	case run := <-ran:
		if !run.delivered || run.left < 400*time.Millisecond {
			t.Fatalf("the hooks ran with the delivery finished %v and %s of the drain left, want it finished and about a second left", run.delivered, run.left)
		}
	default:
		t.Fatal("the drain never ran its hooks")
	}
}

// A delivery that outlasts the drain's share for deliveries still lets the hooks end the KV
// watchers, within what is left of the deadline, before the connection closes: closing it with
// the watchers still subscribed would leave their consumers for the server to time out. The
// client's reserve is a tenth of the deployed one, so the drain divides a second rather than ten.
func TestAStuckDeliveryStillEndsTheKVWatchers(t *testing.T) {
	const reserve = 300 * time.Millisecond
	_, uri := testnats.Start(t)
	logs := captureBusLogs(t)
	name := t.Name()
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithName(name), bus.WithDrainHookReserve(reserve))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	watcher, consumers := watchedBucket(t, client, uri, "drain_hook_stuck")
	client.AddDrainHook(watcher.End)
	hold, started := heldDelivery(t)
	if _, err := client.SubscribeCore("notifications.role.drain-stuck", hold, "drain-stuck"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-stuck", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached its handler")
	}
	watcher.Stop()
	began := time.Now()
	err = client.Drain(time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("Drain returned after %s, want about its 1s deadline", elapsed)
	}
	if got := consumers(); len(got) != 0 {
		t.Fatalf("consumers right after a drain a stuck delivery held = %v, want none", got)
	}
	if got := strings.Join(warnOrErrorRecords(t, logs, name), ", "); got != "" {
		t.Fatalf("the drain logged %q, want nothing", got)
	}
}
