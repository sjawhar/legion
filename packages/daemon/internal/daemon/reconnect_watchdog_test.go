package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
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

// Once NATS reconnects, the watchdog's downtime clock and its own throttle reset: a later
// disconnect gets its own first warn promptly rather than waiting out whatever was left of the
// previous disconnect's warnEvery window.
func TestReconnectWatchdogResetsAfterReconnecting(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	conn := &fakeConnStatus{connected: false, err: errors.New("first outage")}
	w := &workflowRuntime{log: log, reconnectPollInterval: 5 * time.Millisecond, reconnectWarnEvery: 500 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.reconnectWatchdogWith(ctx, conn); close(done) }()

	testwait.Eventually(t, "the watchdog warns about the first outage", func() bool {
		return strings.Contains(logBuf.String(), "first outage")
	})
	conn.set(true, nil)
	time.Sleep(20 * time.Millisecond)
	logBuf.Reset()
	conn.set(false, errors.New("second outage"))

	testwait.Eventually(t, "the watchdog warns about the second outage promptly, not after the first outage's own warnEvery", func() bool {
		return strings.Contains(logBuf.String(), "second outage")
	})
	cancel()
	<-done
}
