package bus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

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

	logs := captureBusLogs(t)
	const reconnectFailed = "envoy nats reconnect attempt failed"

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
	if got := logs.count(name, reconnectFailed); got != 0 {
		t.Fatalf("a connected client logged %d reconnect failures, want 0", got)
	}

	awaitWarnings := func(want int) {
		t.Helper()
		waitFor(t, 30*time.Second, fmt.Sprintf("%d reconnect failures of %s", want, name), func() bool {
			return logs.count(name, reconnectFailed) >= want
		})
		if got := logs.count(name, reconnectFailed); got != want {
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
			if got := logs.count(name, reconnectFailed); got != 2+i {
				t.Fatalf("%d reconnect failures logged while the clock stood still, want %d", got, 2+i)
			}
		}
	}

	// slog's JSON handler writes the record's attributes flat beside its level and message, so
	// one decode per line reads everything the assertions below need.
	type reconnectWarning struct {
		Level   string
		Msg     string
		Name    string
		Servers string
		Error   string
	}
	var warnings []reconnectWarning
	for _, line := range logs.lines() {
		if line == "" {
			continue
		}
		var record reconnectWarning
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode the bus log record %q: %v", line, err)
		}
		if record.Name == name && record.Msg == reconnectFailed {
			warnings = append(warnings, record)
		}
	}
	if len(warnings) != 11 {
		t.Fatalf("%d reconnect failures logged, want 11", len(warnings))
	}
	for i, record := range warnings {
		if record.Level != "WARN" {
			t.Errorf("reconnect failure %d logged at %v, want WARN", i, record.Level)
		}
		if record.Servers != uri {
			t.Errorf("reconnect failure %d names servers %q, want %q", i, record.Servers, uri)
		}
		if record.Error == "" {
			t.Errorf("reconnect failure %d carries no error", i)
		}
	}
	if bus.HoldsSeed(logs.String()) {
		t.Error("a captured log line holds an nkey seed")
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start the server again: %v", err)
	}
	gate.Open()
	waitFor(t, 60*time.Second, "the client to reconnect", func() bool { return client.Conn.IsConnected() })
	if got := logs.count(name, reconnectFailed); got != 11 {
		t.Fatalf("%d reconnect failures logged across a successful reconnect, want 11", got)
	}

	// The reset on ReconnectedCB: the new outage reports itself at once, without a minute of the
	// fake clock, which never passes again. The outage is made the same way the first one was,
	// and in the same order: the gate's address goes first, so no reconnect reaches a listening
	// socket, and stopping the server then drops the live connection the gate's own close would
	// have left joined, so the client's next dial is refused rather than accepted.
	gate.Close()
	testnats.Stop(t, ctr)
	awaitWarnings(12)
}
