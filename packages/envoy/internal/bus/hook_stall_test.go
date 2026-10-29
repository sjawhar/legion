package bus_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A run of the reconnect hooks that fails is run again by a recovery, and the retry that succeeds
// leaves nothing for an operator to do. The retry itself is not what this test holds: it is on main
// already (TestAFailedRunOfTheReconnectHooksRunsAgainWithoutAnotherConnectionEvent). What it holds
// is the report of it. A failure the process repairs seconds later used to page as an ERROR, which
// is what a failure nothing repairs must read as, so this run reports at WARN, logs no ERROR at
// all, and leaves HookStall with no stall and no error once the retry has covered the reconnect.
func TestRewatchRetryAfterAReconnectLogsNoError(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	var runs atomic.Int32
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		defer func() { hooked <- struct{}{} }()
		if runs.Add(1) == 1 {
			// The first run times out on a server that is still coming back, as a watcher's
			// rewatch does when its ordered consumer is not created in time.
			return context.DeadlineExceeded
		}
		return nil
	})

	logs := captureBusLogs(t)
	srv.Stop(t)
	srv.Start(t)
	for run := 1; run <= 2; run++ {
		select {
		case <-hooked:
		case <-time.After(30 * time.Second):
			t.Fatalf("the reconnect hooks ran %d time(s); want the failed run and the retry", run-1)
		}
	}
	// The retry clears the stall as it records the events it covered, just after the hook returned.
	waitFor(t, 15*time.Second, "the retry that covered the reconnect", func() bool {
		stall, hookErr := client.HookStall()
		return stall == 0 && hookErr == nil
	})
	if reconnects := client.Conn.Stats().Reconnects; reconnects != 1 {
		t.Fatalf("the connection reconnected %d time(s); want the one restart, so the retry ran with no further connection event", reconnects)
	}
	if line := logs.lineAt("WARN", "envoy nats reconnect hook failed; it is retried"); line == "" {
		t.Fatalf("the failed run was not reported at WARN: %s", logs.String())
	}
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a failed run the retry repaired logged an error: %s", line)
	}
}

// HookStall is what the listener's /healthz reads to answer 503 while the reconnect hooks are
// stuck: a rewatch that keeps failing ends no watcher, so nothing else tells the listener its
// caches are off their buckets. It reports the time since the connection event the hooks have not
// covered, on the client's own clock, and the error of the last run that failed, so the report
// names what is broken and for how long.
func TestHookStallReportsTheStallAndTheLastHookError(t *testing.T) {
	_, uri := testnats.Start(t)
	clock := &reconnectErrClock{now: time.Now()}
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReconnectErrClock(clock.Now))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	errRewatch := errors.New("rewatch interest cache: nats: no responders available for request")
	client.AddReconnectHook(func(*natsgo.Conn) error { return errRewatch })

	captureBusLogs(t)
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	waitFor(t, 30*time.Second, "the failed run of the hooks to be recorded", func() bool {
		_, hookErr := client.HookStall()
		return hookErr != nil
	})

	// The clock has not moved since the reconnect it timed, so the stall is none yet: the recovery
	// is retrying, and the error alone is no reason for /healthz to refuse.
	stall, hookErr := client.HookStall()
	if stall != 0 || !errors.Is(hookErr, errRewatch) {
		t.Fatalf("HookStall() = (%v, %v) before the clock moved; want (0, %v)", stall, hookErr, errRewatch)
	}

	clock.Advance(bus.RepairGrace)
	stall, hookErr = client.HookStall()
	if stall < bus.RepairGrace || !errors.Is(hookErr, errRewatch) {
		t.Fatalf("HookStall() = (%v, %v) a grace after the reconnect; want at least %v with %v", stall, hookErr, bus.RepairGrace, errRewatch)
	}
}
