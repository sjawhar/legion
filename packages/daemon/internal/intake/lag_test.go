package intake

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// storedMessage is a delivery JetStream stored at stored, with pending more behind it.
type storedMessage struct {
	jetstream.Msg
	stored  time.Time
	pending uint64
}

func (m storedMessage) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{Timestamp: m.stored, NumPending: m.pending, Sequence: jetstream.SequencePair{Stream: 1013506}}, nil
}

func (storedMessage) Subject() string { return "notifications.github.acme.widgets.push.branch.main" }

// A durable consumer hours behind its stream held phase moves and pull request records back with
// nothing in the log to say so. lagWatch says so, with how far behind and how many messages wait,
// for a message stored more than five minutes before it is handled, at most once a minute however
// many such messages a backlog delivers; a consumer keeping up says nothing.
func TestLagWatchSaysOnceAMinuteThatAConsumerIsBehind(t *testing.T) {
	now := time.Date(2026, 10, 26, 20, 54, 24, 0, time.UTC)
	var logs bytes.Buffer
	watch := &lagWatch{consumer: "legion-capture-github", logger: slog.New(slog.NewTextHandler(&logs, nil)), now: func() time.Time { return now }}
	lines := func() int { return strings.Count(logs.String(), "a durable consumer is behind its stream") }

	watch.observe(storedMessage{stored: now.Add(-time.Minute), pending: 3})
	if got := lines(); got != 0 {
		t.Fatalf("a consumer a minute behind logged %d lines, want none:\n%s", got, logs.String())
	}
	watch.observe(storedMessage{stored: now.Add(-2 * time.Hour), pending: 4102})
	if got := lines(); got != 1 {
		t.Fatalf("a consumer two hours behind logged %d lines, want one:\n%s", got, logs.String())
	}
	for _, want := range []string{"consumer=legion-capture-github", "behindSeconds=7200", "pending=4102", "stream_seq=1013506"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the line does not say %s:\n%s", want, logs.String())
		}
	}
	now = now.Add(30 * time.Second)
	watch.observe(storedMessage{stored: now.Add(-2 * time.Hour), pending: 4101})
	if got := lines(); got != 1 {
		t.Fatalf("a second message within the minute logged %d lines in all, want still one", got)
	}
	now = now.Add(30 * time.Second)
	watch.observe(storedMessage{stored: now.Add(-2 * time.Hour), pending: 4000})
	if got := lines(); got != 2 {
		t.Fatalf("a message a minute after the first line logged %d lines in all, want two", got)
	}
}
