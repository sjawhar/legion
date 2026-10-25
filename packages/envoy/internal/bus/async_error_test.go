package bus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
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
	var records bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&records, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	report := options("async-error-test", nil, nil, nil, nil).AsyncErrorCB
	report(nil, &nats.Subscription{Subject: "_INBOX.watcher"}, nats.ErrConsumerNotActive)
	report(nil, &nats.Subscription{Subject: "_INBOX.other"}, nats.ErrSlowConsumer)
	report(nil, &nats.Subscription{Subject: "_INBOX.recreate"}, fmt.Errorf("%w: recreating ordered consumer", nats.ErrConsumerNotFound))
	report(nil, &nats.Subscription{Subject: "_INBOX.drained"}, nats.ErrConsumerNotFound)

	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(records.String()), "\n") {
		var record struct {
			Level   string `json:"level"`
			Subject string `json:"subject"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		levels = append(levels, record.Subject+"="+record.Level)
	}
	if got, want := strings.Join(levels, " "), "_INBOX.watcher=WARN _INBOX.other=ERROR _INBOX.recreate=ERROR _INBOX.drained=ERROR"; got != want {
		t.Fatalf("async error levels = %s, want %s", got, want)
	}
}
