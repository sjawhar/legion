package daemon

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeConnStatus is reconnectWatchdog's test double for *nats.Conn: a test sets connected and err
// directly, without a real connection or a NATS server.
type fakeConnStatus struct {
	mu        sync.Mutex
	connected bool
	err       error
}

func (c *fakeConnStatus) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *fakeConnStatus) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *fakeConnStatus) set(connected bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected, c.err = connected, err
}

// A NATS reconnect that keeps failing a non-auth handshake — a server that accepts the TCP
// connection but never completes the protocol handshake, say — retries forever under
// natsauth.ReconnectForever behind one "NATS connection lost" line, stuck in RECONNECTING, since
// LogEvents' DisconnectedErrCB and ReconnectedCB each fire once per disconnect/reconnect pair and
// say nothing while stuck between them. reconnectWatchdog is what gives an operator a periodic
// signal instead: a warn every reconnectWarnEvery naming the downtime so far and the connection's
// own LastError, and none at all while connected.
func TestReconnectWatchdogWarnsWhileDisconnectedNotWhileConnected(t *testing.T) {
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	conn := &fakeConnStatus{connected: true}
	w := &workflowRuntime{log: log, reconnectPollInterval: 5 * time.Millisecond, reconnectWarnEvery: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.reconnectWatchdogWith(ctx, conn); close(done) }()

	time.Sleep(40 * time.Millisecond)
	if strings.Contains(logBuf.String(), "NATS has not reconnected") {
		t.Fatal("the watchdog warned while connected")
	}

	conn.set(false, errors.New("nats: protocol exception, INFO not received"))
	testwait.Eventually(t, "the watchdog warns while disconnected", func() bool {
		return strings.Contains(logBuf.String(), "NATS has not reconnected")
	})
	cancel()
	<-done

	msg := logBuf.String()
	if !strings.Contains(msg, "downtime=") {
		t.Fatalf("warn log = %q, want it to name the downtime so far", msg)
	}
	if !strings.Contains(msg, "protocol exception, INFO not received") {
		t.Fatalf("warn log = %q, want it to name conn.LastError()", msg)
	}
}

// Once NATS reconnects, the watchdog's downtime clock and its own warn throttle both reset: a
// later disconnect waits out its own fresh warnEvery before its first warn, rather than reusing
// a downtime or a last-warned time left over from the previous disconnect. The connected interval
// here outlasts warnEvery itself, so a broken reset's leftover clocks would already be overdue the
// moment the second disconnect begins, and would warn within one poll tick of it — which the
// no-warn check below would catch.
func TestReconnectWatchdogResetsAfterReconnecting(t *testing.T) {
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	conn := &fakeConnStatus{connected: false, err: errors.New("first outage")}
	warnEvery := 1200 * time.Millisecond
	w := &workflowRuntime{log: log, reconnectPollInterval: 5 * time.Millisecond, reconnectWarnEvery: warnEvery}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.reconnectWatchdogWith(ctx, conn); close(done) }()

	testwait.Eventually(t, "the watchdog warns about the first outage", func() bool {
		return strings.Contains(logBuf.String(), "first outage")
	})
	conn.set(true, nil)
	time.Sleep(3 * warnEvery / 2)
	logBuf.Reset()
	conn.set(false, errors.New("second outage"))

	// With a broken reset, disconnectedSince and warnedAt would still be the first outage's —
	// already more than warnEvery stale by now — so the very next poll tick would warn. A quarter
	// of warnEvery is many poll ticks (poll interval is 5ms here), long enough to catch that.
	time.Sleep(warnEvery / 4)
	if strings.Contains(logBuf.String(), "second outage") {
		t.Fatalf("warned about the second outage before its own fresh warnEvery elapsed (the reset is broken): %q", logBuf.String())
	}

	testwait.Eventually(t, "the watchdog warns about the second outage once its own warnEvery elapses", func() bool {
		return strings.Contains(logBuf.String(), "second outage")
	})
	msg := logBuf.String()
	match := reconnectDowntimeRe.FindStringSubmatch(msg)
	if match == nil {
		t.Fatalf("warn log = %q, want a parseable downtime=", msg)
	}
	downtime, err := time.ParseDuration(match[1])
	if err != nil {
		t.Fatalf("parse downtime %q: %v", match[1], err)
	}
	// Counted from the second disconnect alone, downtime is close to warnEvery. Counted
	// cumulatively from the first disconnect (connected interval included, as a broken reset
	// would), it would be well over twice that.
	if downtime >= 2*warnEvery {
		t.Fatalf("second outage's downtime = %v, want close to warnEvery (%v), not cumulative since the first outage began", downtime, warnEvery)
	}
	cancel()
	<-done
}

var reconnectDowntimeRe = regexp.MustCompile(`downtime=(\S+)`)

// A watchdog that warned on the very first poll after a disconnect — downtime=0s, rounded down
// from whatever the poll interval happened to be — would be close to as noisy as nats.go's own
// default 2s reconnect wait: the deep review measured about one ordinary reconnect in five
// tripping it. The first warn must wait for the downtime itself to reach warnEvery, exactly as
// pollHoldReleaseWith holds its own first warn for holdWarnAfter, then warn every warnEvery after.
func TestReconnectWatchdogWarnsOnlyOnceDowntimeReachesWarnEvery(t *testing.T) {
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	conn := &fakeConnStatus{connected: true}
	warnEvery := 1200 * time.Millisecond
	w := &workflowRuntime{log: log, reconnectPollInterval: 50 * time.Millisecond, reconnectWarnEvery: warnEvery}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.reconnectWatchdogWith(ctx, conn); close(done) }()

	conn.set(false, errors.New("handshake failure"))
	time.Sleep(warnEvery / 2)
	if strings.Contains(logBuf.String(), "NATS has not reconnected") {
		t.Fatalf("warned before downtime reached warnEvery: %q", logBuf.String())
	}

	testwait.Eventually(t, "the watchdog warns once downtime reaches warnEvery", func() bool {
		return strings.Contains(logBuf.String(), "NATS has not reconnected")
	})
	cancel()
	<-done

	msg := logBuf.String()
	match := reconnectDowntimeRe.FindStringSubmatch(msg)
	if match == nil {
		t.Fatalf("warn log = %q, want a parseable downtime=", msg)
	}
	downtime, err := time.ParseDuration(match[1])
	if err != nil {
		t.Fatalf("parse downtime %q: %v", match[1], err)
	}
	if downtime < time.Second {
		t.Fatalf("first warn's downtime = %v (logged %q), want >= warnEvery (%v) — a downtime this low means it warned near the first poll, not once downtime reached warnEvery", downtime, match[1], warnEvery)
	}
}
