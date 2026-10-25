package startup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

const stampedCommit = "0123456789abcdef0123456789abcdef01234567"

// startedAt is the clock every test starts Dispatch at, whole seconds as Envelope keeps them, and
// startedEventID the event id Envelope builds from it (its unix time, 1792411200).
var startedAt = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

const startedEventID = "dispatch-server-started-" + stampedCommit + "-1792411200"

func clock(at time.Time) func() time.Time { return func() time.Time { return at } }

// logBuffer collects a test's log lines, written by the publish on its goroutine and read by the
// test on its own.
type logBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// logger is a logger into logs.
func logger(logs *logBuffer) *slog.Logger { return slog.New(slog.NewTextHandler(logs, nil)) }

// TestAnUnstampedBuildPublishesNoStart: an empty commit publishes nothing, and says so at INFO.
func TestAnUnstampedBuildPublishesNoStart(t *testing.T) {
	client := connect(t, testnats.URL(t))
	var logs logBuffer
	startup.PublishStarted(t.Context(), client, "", clock(startedAt), logger(&logs))
	if got := countStarts(t, client); got != 0 {
		t.Fatalf("the stream holds %d start(s) for an unstamped build, want none", got)
	}
	if !strings.Contains(logs.String(), "dispatch publishes no start: no build commit") {
		t.Fatalf("logs = %q, want the INFO line naming the missing commit", logs.String())
	}
}

// TestAStampedBuildPublishesItsStartOnce: a stamped commit publishes one envelope the stream holds,
// in the shape the Legion daemon decodes: every field its envelope check requires, source dispatch,
// the subject, the dedupe key naming the start, and the payload {commit, started_at}. The same
// start published again, as a retry whose first acknowledgement was lost would, is a duplicate the
// stream stores once.
func TestAStampedBuildPublishesItsStartOnce(t *testing.T) {
	client := connect(t, testnats.URL(t))
	var logs logBuffer
	log := logger(&logs)
	startup.PublishStarted(t.Context(), client, stampedCommit, clock(startedAt), log)
	if got := countStarts(t, client); got != 1 {
		t.Fatalf("the stream holds %d start(s), want 1", got)
	}
	if !strings.Contains(logs.String(), "dispatch published its start") || !strings.Contains(logs.String(), stampedCommit) {
		t.Fatalf("logs = %q, want the success line naming the commit", logs.String())
	}

	item := lastStart(t, client)
	if err := item.Validate(); err != nil {
		t.Fatalf("the published envelope does not validate: %v", err)
	}
	if item.EventID != startedEventID || item.SourceEventID != startedEventID || item.DedupeKey != startedEventID {
		t.Fatalf("event id, source event id, dedupe key = %q, %q, %q; want all %q", item.EventID, item.SourceEventID, item.DedupeKey, startedEventID)
	}
	if item.Source != "dispatch" || item.Topic != "notifications.dispatch.server.started" {
		t.Fatalf("source/topic = (%q, %q), want dispatch on notifications.dispatch.server.started", item.Source, item.Topic)
	}
	if item.IssuedAt != startedAt.UnixMilli() || item.TraceID == "" {
		t.Fatalf("issued_at %d, trace_id %q; want %d and a trace id", item.IssuedAt, item.TraceID, startedAt.UnixMilli())
	}
	if item.PayloadSummary != "dispatch started on 0123456" {
		t.Fatalf("payload summary = %q, want the short commit", item.PayloadSummary)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(item.Payload), &fields); err != nil {
		t.Fatalf("the payload is not a JSON object: %v: %s", err, item.Payload)
	}
	if len(fields) != 2 || string(fields["commit"]) != `"`+stampedCommit+`"` || string(fields["started_at"]) != `"2026-10-19T12:00:00Z"` {
		t.Fatalf("payload = %s, want exactly {commit, started_at} naming the commit and the start", item.Payload)
	}

	startup.PublishStarted(t.Context(), client, stampedCommit, clock(startedAt), log)
	if got := countStarts(t, client); got != 1 {
		t.Fatalf("the stream holds %d start(s) after the same start was published again, want 1", got)
	}
}

// TestTheStartIsRetriedUntilTheStreamAcknowledgesIt: with the server away, the publish fails and is
// retried; once the server is back, the retry publishes, and the stream holds the one start.
func TestTheStartIsRetriedUntilTheStreamAcknowledgesIt(t *testing.T) {
	srv, uri := testnats.Start(t)
	client := connect(t, uri)
	srv.Stop(t)
	waitFor(t, 15*time.Second, "the client to notice the server is gone", func() bool { return !client.Connected() })

	var logs logBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		startup.PublishStarted(t.Context(), client, stampedCommit, clock(startedAt), logger(&logs))
	}()
	// The first attempts fail while the server is away: the publish is still running once it has
	// said so.
	waitFor(t, 15*time.Second, "a failed attempt to be logged", func() bool {
		select {
		case <-done:
			t.Fatal("the start was published with the server away")
		default:
		}
		return strings.Contains(logs.String(), "dispatch could not publish its start")
	})

	srv.Start(t)
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the start was not published within a minute of the server's return")
	}
	if !strings.Contains(logs.String(), "dispatch published its start") {
		t.Fatalf("logs = %q, want the success line after the retries", logs.String())
	}
	waitFor(t, 15*time.Second, "the client to reconnect", client.Connected)
	if got := countStarts(t, client); got != 1 {
		t.Fatalf("the stream holds %d start(s) after the retry, want 1", got)
	}
	if item := lastStart(t, client); item.EventID != startedEventID {
		t.Fatalf("the retried start is %q, want the envelope built once at the start", item.EventID)
	}
}

// TestAStoppedProcessGivesUpItsStart: a context that ends while the server is away ends the
// retries without a publish.
func TestAStoppedProcessGivesUpItsStart(t *testing.T) {
	srv, uri := testnats.Start(t)
	client := connect(t, uri)
	srv.Stop(t)
	waitFor(t, 15*time.Second, "the client to notice the server is gone", func() bool { return !client.Connected() })

	ctx, cancel := context.WithCancel(t.Context())
	var logs logBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		startup.PublishStarted(ctx, client, stampedCommit, clock(startedAt), logger(&logs))
	}()
	waitFor(t, 15*time.Second, "a failed attempt to be logged", func() bool {
		return strings.Contains(logs.String(), "dispatch could not publish its start")
	})
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the publish did not end with its context")
	}
	if !strings.Contains(logs.String(), "dispatch stopped before it published its start") {
		t.Fatalf("logs = %q, want the line saying the process stopped first", logs.String())
	}
}

// connect opens a client that owns the stream on uri and closes it with the test.
func connect(t *testing.T, uri string) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// countStarts is how many messages the stream holds on the start subject.
func countStarts(t *testing.T, client *bus.Client) uint64 {
	t.Helper()
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: startup.Subject})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	return info.State.Subjects[startup.Subject]
}

// lastStart is the last envelope the stream holds on the start subject.
func lastStart(t *testing.T, client *bus.Client) contracts.Envelope {
	t.Helper()
	message, err := client.JS().GetLastMsg(bus.Stream, startup.Subject)
	if err != nil {
		t.Fatalf("read the last start: %v", err)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(message.Data, &item); err != nil {
		t.Fatalf("decode the start envelope: %v", err)
	}
	return item
}

// waitFor polls condition until it holds, and fails the test once timeout has passed.
func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
