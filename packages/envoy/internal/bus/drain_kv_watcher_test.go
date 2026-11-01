package bus_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A drain ends the KV watchers, deleting the ordered consumers nats.go created for them, before it
// drains the connection, as the listener's shutdown does (kvwatch.Watcher.End from a drain hook).
// Left to the connection's drain, nats.go removes a watcher's subscription and only then sends the
// delete (checkDrained), so the connection's drain, which waits only for its subscriptions, can
// reach its publishes first and refuse the delete as "connection draining", an ERROR, whether or
// not the consumer still exists. Ended first, the delete is a request the drain waits for. A
// consumer still there is deleted with nothing logged; one already gone (a NATS restart loses an
// ordered consumer, which nats.go keeps in memory) is a warning, since the delete was for that
// state. Either way nats.go reports nothing asynchronously and nothing is an ERROR, which
// TestListenerFollowsTheInterestRegistryAcrossNATSRestarts refuses (LEGION-278).
func TestADrainEndsTheKVWatchersBeforeTheConnection(t *testing.T) {
	for _, test := range []struct {
		name         string
		consumerGone bool
		want         string
	}{
		{name: "consumer present", want: ""},
		{name: "consumer already gone", consumerGone: true, want: "WARN drain-test watcher's consumer was already gone at its end"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			logs := captureBusLogs(t)

			name := t.Name()
			client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithName(name))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			kv, err := bus.EnsureKeyValue(client.Conn, client.JS(), &natsgo.KeyValueConfig{Bucket: "drain_kv_watcher"})
			if err != nil {
				t.Fatalf("create bucket: %v", err)
			}
			watcher := kvwatch.New("drain-test", kv, discardingCache{})
			watcher.Start()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := watcher.WaitReady(ctx); err != nil {
				t.Fatalf("wait for the watcher: %v", err)
			}
			client.AddDrainHook(watcher.End)

			admin := testnats.Connect(t, uri)
			adminJS, err := admin.JetStream()
			if err != nil {
				t.Fatalf("admin JetStream: %v", err)
			}
			if consumers := consumerNames(adminJS, "KV_drain_kv_watcher"); len(consumers) != 1 {
				t.Fatalf("consumers before the drain = %v, want the watcher's one", consumers)
			}
			if test.consumerGone {
				// Delete the watcher's consumer from a second connection, as the server does when it
				// loses it.
				for _, name := range consumerNames(adminJS, "KV_drain_kv_watcher") {
					if err := adminJS.DeleteConsumer("KV_drain_kv_watcher", name); err != nil {
						t.Fatalf("delete the watcher's consumer %s: %v", name, err)
					}
				}
			}

			watcher.Stop()
			if err := client.Drain(10 * time.Second); err != nil {
				t.Fatalf("drain: %v", err)
			}
			// nats.go runs an async error report after the drain's close only if it queued it before
			// closing its callback queue; the wait gives one queued in time the moment to log.
			time.Sleep(200 * time.Millisecond)
			if got := strings.Join(warnOrErrorRecords(t, logs, name), ", "); got != test.want {
				t.Fatalf("the drain logged %q, want %q", got, test.want)
			}
			if consumers := consumerNames(adminJS, "KV_drain_kv_watcher"); len(consumers) != 0 {
				t.Fatalf("consumers after the drain = %v, want none", consumers)
			}
		})
	}
}

// A drain runs its hooks once the delivery subscriptions have drained and before the connection's
// own drain, within the drain's deadline, so a hook's requests still reach the server.
func TestDrainRunsItsHooksWhileTheConnectionCarriesRequests(t *testing.T) {
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{})
	var delivered atomic.Bool
	if _, err := client.SubscribeCore("notifications.role.drain-hook", func(*natsgo.Msg) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		delivered.Store(true)
	}, "drain-hook"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-hook", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached its handler")
	}
	type observed struct {
		delivered, draining, deadline bool
		flush                         error
	}
	ran := make(chan observed, 2)
	client.AddDrainHook(func(ctx context.Context) {
		_, hasDeadline := ctx.Deadline()
		ran <- observed{
			delivered: delivered.Load(),
			draining:  client.Conn.IsDraining(),
			deadline:  hasDeadline,
			flush:     client.Conn.FlushWithContext(ctx),
		}
	})
	if err := client.Drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	close(ran)
	var runs []observed
	for run := range ran {
		runs = append(runs, run)
	}
	if len(runs) != 1 {
		t.Fatalf("the drain ran its hook %d times, want once", len(runs))
	}
	run := runs[0]
	if !run.delivered || run.draining || run.flush != nil || !run.deadline {
		t.Fatalf("the hook ran with the delivery finished %v, the connection draining %v, a flush answering %v and a deadline %v; want the delivery finished, the connection not draining, the flush answered and the drain's deadline",
			run.delivered, run.draining, run.flush, run.deadline)
	}
}

// warnOrErrorRecords takes the client under test from the moment it is connected, though that
// client may have logged nothing under its name yet: a healthy client of these tests first names
// itself in its close's records (`envoy nats disconnected`, `envoy nats connection closed`, `envoy
// nats recovery cancelled`), which nats.go reports from its own callbacks after the drain has
// returned. In 20 measured drains of
// TestAStuckDeliveryStillEndsTheKVWatchers' shape, 4 had no such record when Drain returned, the
// first landing 1.6 to 2.3 ms later, and that test reads the records with no wait of its own, so
// a helper that refuses its caller until such a record lands makes the test race nats.go's
// callback.
func TestWarnOrErrorRecordsTakesAClientThatHasLoggedNothing(t *testing.T) {
	_, uri := testnats.Start(t)
	logs := captureBusLogs(t)

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	if got := warnOrErrorRecords(t, logs, t.Name()); len(got) != 0 {
		t.Fatalf("the records of a client that has logged nothing = %q, want none", got)
	}
}

// consumerNames lists the stream's consumers.
func consumerNames(js natsgo.JetStreamContext, stream string) []string {
	var names []string
	for name := range js.ConsumerNames(stream) {
		names = append(names, name)
	}
	return names
}

// warnOrErrorRecords is every WARN or ERROR record the client named name logged so far, each as
// its level and message, with the error for an async error report. name is the name that client
// was connected with (bus.WithName). The caller passes it, rather than the helper waiting for a
// record that names the client under test, which a healthy client of these tests may not have
// logged yet when the assertion reads (TestWarnOrErrorRecordsTakesAClientThatHasLoggedNothing).
// The capture (captureBusLogs) is the process's default logger, so every client of every test
// running in this binary writes into it, and a record naming another client is dropped: without
// that, a client an earlier test left connected to a cluster it has stopped fails this test with
// its reconnect warning. A caller that names no client is refused, since the filter would then
// drop every record that names a client, the client under test's among them. A record naming no
// client is kept, since the records these tests require are a watcher's and a reconcile's, which
// name none.
func warnOrErrorRecords(t *testing.T, logs *busLogs, name string) []string {
	t.Helper()
	if name == "" {
		t.Fatal("warnOrErrorRecords needs the name the client under test was connected with (bus.WithName)")
	}
	var records []string
	for _, line := range logs.lines() {
		if line == "" {
			continue
		}
		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Error string `json:"error"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode the log record %q: %v", line, err)
		}
		if record.Level != "WARN" && record.Level != "ERROR" {
			continue
		}
		if record.Name != "" && record.Name != name {
			continue
		}
		if record.Msg == "envoy nats async error" {
			records = append(records, record.Level+" "+record.Msg+": "+record.Error)
			continue
		}
		records = append(records, record.Level+" "+record.Msg)
	}
	return records
}
