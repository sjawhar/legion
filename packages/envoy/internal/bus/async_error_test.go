package bus

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

// A KV watcher's ordered consumer reports nats.ErrConsumerNotActive only while its connection is
// not connected - connected, nats.go resets the consumer instead - and every consumer Envoy runs
// with idle heartbeats is such a watcher. The report restates a disconnect the bus has already
// logged, once per watcher: six ERROR lines in one NATS gap of a deployment pin rehearsal. It is a
// WARN. Every other async error stays an ERROR (LEGION-278), "consumer not found" among them,
// bare or wrapped: a shutdown ends each KV watcher and deletes its consumer itself, before the
// connection's drain (kvwatch.Watcher.End), so a delete nats.go's drain reports belongs to a
// watcher nothing ended, and an ordered consumer nats.go failed to recreate wraps the error.
func TestAConsumerNotActiveReportIsAWarning(t *testing.T) {
	logs := captureLogs(t)

	report := options("async-error-test", nil, nil, nil, nil, nil).AsyncErrorCB
	report(nil, &nats.Subscription{Subject: "_INBOX.watcher"}, nats.ErrConsumerNotActive)
	report(nil, &nats.Subscription{Subject: "_INBOX.other"}, nats.ErrSlowConsumer)
	report(nil, &nats.Subscription{Subject: "_INBOX.recreate"}, fmt.Errorf("%w: recreating ordered consumer", nats.ErrConsumerNotFound))
	report(nil, &nats.Subscription{Subject: "_INBOX.drained"}, nats.ErrConsumerNotFound)

	// Only this client's records: a client an earlier test closed can still log through the same
	// default.
	var levels []string
	for _, record := range logs.records(t) {
		if record["name"] == "async-error-test" {
			levels = append(levels, fmt.Sprint(record["subject"], "=", record["level"]))
		}
	}
	if got, want := strings.Join(levels, " "), "_INBOX.watcher=WARN _INBOX.other=ERROR _INBOX.recreate=ERROR _INBOX.drained=ERROR"; got != want {
		t.Fatalf("async error levels = %s, want %s", got, want)
	}
}

// Only the server's refusal of the client's core subscription, on its subject and in its queue
// group, reported by the connection the client holds, marks it refused (CoreSubOK). A refusal of a
// publish, of another subscription, of the same subject in another queue group or outside one, and
// one a connection the client has replaced reports late, leave it as it is. The errors are built as
// nats.go builds the one it hands the async error callback for a server's -ERR (processErr).
func TestOnlyTheServersRefusalOfTheCoreSubscriptionMarksItRefused(t *testing.T) {
	violation := func(text string) error { return fmt.Errorf("%w: %s", nats.ErrPermissionViolation, text) }
	own := violation(`Permissions Violation for Subscription to "notifications.>" using queue "live"`)
	current := &nats.Conn{}
	for _, tc := range []struct {
		name string
		conn *nats.Conn
		err  error
		want bool
	}{
		{name: "its own refusal", conn: current, err: own, want: true},
		{name: "a publish refusal", conn: current, err: violation(`Permissions Violation for Publish to "notifications.>"`)},
		{name: "another subject", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.role.>" using queue "live"`)},
		{name: "another queue group", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.>" using queue "other"`)},
		{name: "no queue group", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.>"`)},
		{name: "a replaced connection", conn: &nats.Conn{}, err: own},
		{name: "not a permissions violation", conn: current, err: errors.New(own.Error())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{Conn: current}
			client.subscriptions[coreSubscription] = recoverableSubscription{
				transport: coreSubscription,
				subject:   "notifications.>",
				queue:     "live",
				handler:   func(*nats.Msg) {},
			}
			client.onAsyncError(tc.conn, tc.err)
			if got := client.subscriptions[coreSubscription].refused; got != tc.want {
				t.Fatalf("refused = %t after %v, want %t", got, tc.err, tc.want)
			}
		})
	}
}
