package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
)

// The channels the delivery hub listens on. delivery_changes carries no payload and is notified
// as a delivery_changes row commits (migration 0100); delivery_freshness carries the settings
// row's freshness columns as JSON (0100); dispatch_events carries no payload and is notified as an
// event commits (0107).
const (
	deliveryChangesChannel   = "delivery_changes"
	deliveryFreshnessChannel = "delivery_freshness"
	dispatchEventsChannel    = "dispatch_events"
)

// deliveryListenStatement listens on all three channels in one round trip: with no arguments
// pgx sends it over the simple protocol, which runs every statement in it.
const deliveryListenStatement = "listen " + deliveryChangesChannel + "; listen " + deliveryFreshnessChannel + "; listen " + dispatchEventsChannel

const (
	// deliveryListenIdle is how long the listener waits for a notification before it pings its
	// connection. A connection that went half-open (a dropped NAT entry, a failover that left the
	// socket up) delivers nothing and reports no error; only a round trip finds it.
	deliveryListenIdle = 30 * time.Second
	// deliveryPingTimeout bounds that ping, the LISTEN statement on a new connection, and the
	// close of a connection the listener gives up.
	deliveryPingTimeout = 5 * time.Second
	// The default backoff's first wait and its ceiling.
	deliveryBackoffFirst = time.Second
	deliveryBackoffMax   = 30 * time.Second
)

// deliveryListener holds one LISTEN connection of its own for the delivery hub (LEGION-690): it
// dials, listens on delivery_changes, delivery_freshness and dispatch_events, reports each
// notification, pings the connection when it is idle, and redials on any error.
type deliveryListener struct {
	dial        func(ctx context.Context) (*pgx.Conn, error) // the hub passes Store.Pool.Listener; a test passes a proxy's
	listenIdle  time.Duration                                // 30s when zero (deliveryListenIdle); a test sets it lower
	pingTimeout time.Duration                                // 5s when zero (deliveryPingTimeout); a test sets it lower
	onConnected func()                                       // after all three LISTENs succeed on a new connection; the hub catches up both logs here
	onNotify    func(channel, payload string)                // one call per notification, on the listener's goroutine
	backoff     func(attempt int) time.Duration              // nil: 1s doubling to 30s, plus jitter
}

// run blocks until ctx ends, then closes its connection and returns. Each time a connection
// fails, or a dial or its LISTEN does, it closes that connection, logs why, waits the backoff for
// the number of failures in a row (a connection that got as far as its LISTEN starts the count
// again), and dials again. A notification sent while it had no connection is not reported; the
// hub's catch-up on the next onConnected covers that window.
func (l *deliveryListener) run(ctx context.Context) {
	failures := 0
	for {
		listened, err := l.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if listened {
			failures = 0
		}
		failures++
		wait := l.backoffFor(failures)
		slog.Warn("dispatch: delivery live listener has no connection; redialing",
			"error", err, "attempt", failures, "wait_ms", wait.Milliseconds())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// listen dials one connection, listens on the three channels, and reports what arrives on it
// until ctx ends or the connection fails, then closes it. listened is whether its LISTEN
// succeeded; err is why it stopped.
func (l *deliveryListener) listen(ctx context.Context) (listened bool, err error) {
	conn, err := l.dial(ctx)
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer l.close(conn)
	listenCtx, cancel := context.WithTimeout(ctx, l.pingWait())
	_, err = conn.Exec(listenCtx, deliveryListenStatement)
	cancel()
	if err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	l.onConnected()
	for {
		waitCtx, cancel := context.WithTimeout(ctx, l.idle())
		notification, err := conn.WaitForNotification(waitCtx)
		idle := errors.Is(waitCtx.Err(), context.DeadlineExceeded)
		cancel()
		if notification != nil {
			l.onNotify(notification.Channel, notification.Payload)
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if !idle {
			return true, fmt.Errorf("wait for a notification: %w", err)
		}
		// pgx keeps the connection open when a wait ends at its deadline, so the same connection
		// answers the ping; one that has gone half-open fails it at pingTimeout.
		pingCtx, cancel := context.WithTimeout(ctx, l.pingWait())
		err = conn.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return true, fmt.Errorf("ping an idle connection: %w", err)
		}
	}
}

// close closes conn within pingTimeout, so a half-open connection's Terminate cannot hold the
// listener; closing a connection pgx has already closed does nothing.
func (l *deliveryListener) close(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), l.pingWait())
	defer cancel()
	_ = conn.Close(ctx)
}

func (l *deliveryListener) idle() time.Duration {
	if l.listenIdle == 0 {
		return deliveryListenIdle
	}
	return l.listenIdle
}

func (l *deliveryListener) pingWait() time.Duration {
	if l.pingTimeout == 0 {
		return deliveryPingTimeout
	}
	return l.pingTimeout
}

func (l *deliveryListener) backoffFor(attempt int) time.Duration {
	if l.backoff == nil {
		return deliveryListenerBackoff(attempt)
	}
	return l.backoff(attempt)
}

// deliveryListenerBackoff is the default wait before the attempt-th redial in a row (from 1): a
// second, doubling to thirty, plus up to half of that again, so the tasks of a rolling deploy that
// lost one database together do not redial it in step.
func deliveryListenerBackoff(attempt int) time.Duration {
	wait := deliveryBackoffFirst
	for n := 1; n < attempt && wait < deliveryBackoffMax; n++ {
		wait *= 2
	}
	wait = min(wait, deliveryBackoffMax)
	return wait + rand.N(wait/2+1)
}
