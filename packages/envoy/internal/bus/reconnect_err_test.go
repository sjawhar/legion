package bus_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// reconnectErrRecords collects the records the package logs while a test runs, so the test can
// count the warnings a client's failed reconnect attempts made. slog calls a handler from
// whichever goroutine logs, nats.go's reconnect loop here, so every field is read and written
// under the mutex.
type reconnectErrRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *reconnectErrRecords) Enabled(context.Context, slog.Level) bool { return true }

func (r *reconnectErrRecords) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record.Clone())
	return nil
}

func (r *reconnectErrRecords) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *reconnectErrRecords) WithGroup(string) slog.Handler { return r }

// warnings returns the reconnect-failure records of the client named name, so nothing another
// part of the run logs can satisfy or break the count.
func (r *reconnectErrRecords) warnings(name string) []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []slog.Record
	for _, record := range r.records {
		if record.Message != "envoy nats reconnect attempt failed" {
			continue
		}
		if attrValue(record, "name") == name {
			found = append(found, record)
		}
	}
	return found
}

// text is every message and every attribute value captured so far, for the credential check.
func (r *reconnectErrRecords) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all string
	for _, record := range r.records {
		all += record.Message + "\n"
		record.Attrs(func(a slog.Attr) bool {
			all += a.Value.String() + "\n"
			return true
		})
	}
	return all
}

// attrValue is the value of the record's attribute key, formatted, or "" when it has none.
func attrValue(record slog.Record, key string) string {
	value := ""
	record.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			value = a.Value.String()
			return false
		}
		return true
	})
	return value
}

// reconnectErrClock is a clock a test moves itself, so a minute of the throttle's clock passes
// without the test waiting one. nats.go's reconnect loop reads it while the test advances it.
type reconnectErrClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *reconnectErrClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *reconnectErrClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestAFailedReconnectAttemptLogsOneWarnAMinute holds that a broker that is down is never silent
// and never floods: the first failed reconnect attempt of an outage logs one WARN at once, naming
// the servers the client was configured with and the attempt's error, and the attempts that follow
// log one more per minute, not one per attempt (nats.go retries about once a second). It also
// holds that a new outage reports itself at once rather than waiting out the previous outage's
// minute, and that no line carries the client's seed. The regression it catches is the one this
// test was written for: no ReconnectErrCB set at all, so every attempt was silent.
//
// nats.go runs ReconnectErrCB only for an attempt whose dial failed, so the outage is made by
// stopping the server and closing the gate its address belongs to: nothing listens there, and
// every reconnect dial is refused rather than accepted and dropped.
func TestAFailedReconnectAttemptLogsOneWarnAMinute(t *testing.T) {
	unsetNkeyEnvironment(t)
	seed, public := testnats.User(t)
	ctr, _ := testnats.StartNkeyAuthorizedContainer(t, public)
	gate := testnats.StartGate(t, ctr)
	uri := gate.URL()
	t.Setenv("NATS_NKEY_SEED", seed)

	records := &reconnectErrRecords{}
	previous := slog.Default()
	slog.SetDefault(slog.New(records))
	t.Cleanup(func() { slog.SetDefault(previous) })

	clock := &reconnectErrClock{now: time.Now()}
	name := "reconnect-err-test-" + t.Name()
	client, err := bus.ConnectOwningStream([]string{uri},
		bus.WithName(name),
		bus.WithReconnectErrClock(clock.Now),
		bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	// A successful connect reports no failed attempt.
	if got := len(records.warnings(name)); got != 0 {
		t.Fatalf("a connected client logged %d reconnect failures, want 0", got)
	}

	awaitWarnings := func(want int) {
		t.Helper()
		waitFor(t, 30*time.Second, fmt.Sprintf("%d reconnect failures of %s", want, name), func() bool {
			return len(records.warnings(name)) >= want
		})
		if got := len(records.warnings(name)); got != want {
			t.Fatalf("%d reconnect failures logged, want %d", got, want)
		}
	}

	gate.Close()
	testnats.Stop(t, ctr)
	awaitWarnings(1)

	for i := range 10 {
		clock.Advance(time.Minute)
		awaitWarnings(2 + i)
		if i == 4 {
			// The throttle: several more attempts (ReconnectWait is a second) add no line while
			// the clock stands still.
			time.Sleep(5 * time.Second)
			if got := len(records.warnings(name)); got != 2+i {
				t.Fatalf("%d reconnect failures logged while the clock stood still, want %d", got, 2+i)
			}
		}
	}

	warnings := records.warnings(name)
	if len(warnings) != 11 {
		t.Fatalf("%d reconnect failures logged, want 11", len(warnings))
	}
	for i, record := range warnings {
		if record.Level != slog.LevelWarn {
			t.Errorf("reconnect failure %d logged at %v, want WARN", i, record.Level)
		}
		if got := attrValue(record, "servers"); got != uri {
			t.Errorf("reconnect failure %d names servers %q, want %q", i, got, uri)
		}
		if attrValue(record, "error") == "" {
			t.Errorf("reconnect failure %d carries no error", i)
		}
	}
	if bus.HoldsSeed(records.text()) {
		t.Error("a captured log line holds an nkey seed")
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start the server again: %v", err)
	}
	gate.Open()
	waitFor(t, 60*time.Second, "the client to reconnect", func() bool { return client.Conn.IsConnected() })
	if got := len(records.warnings(name)); got != 11 {
		t.Fatalf("%d reconnect failures logged across a successful reconnect, want 11", got)
	}

	// The reset on ReconnectedCB: the new outage reports itself at once, without a minute of the
	// fake clock, which never passes again. The server keeps running this time; the gate alone
	// takes its address away.
	gate.Close()
	awaitWarnings(12)
}
