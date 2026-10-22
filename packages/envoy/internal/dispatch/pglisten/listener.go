// Package pglisten holds one Postgres LISTEN connection that redials: the loop every Dispatch
// feature that learns of another process's commits from the database runs. Two envoy-dispatch
// processes serve one Postgres (INFRA-66), so what one process commits reaches the other only
// through the database, and a connection waiting for a notification must outlive every failure
// between it and the server. Its three users are the delivery hub and the event stream relay
// (internal/dispatch/api) and the document rooms' stale-room evictor (internal/dispatch/docs).
package pglisten

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// DefaultListenIdle is how long a listener waits for a notification before it pings its
	// connection. A connection that went half-open (a dropped NAT entry, a failover that left the
	// socket up) delivers nothing and reports no error; only a round trip finds it.
	DefaultListenIdle = 30 * time.Second
	// DefaultPingTimeout bounds that ping, the LISTEN statement on a new connection, and the
	// close of a connection the listener gives up.
	DefaultPingTimeout = 5 * time.Second
	// The default backoff's first wait and its ceiling.
	backoffFirst = time.Second
	backoffMax   = 30 * time.Second
)

// Listener holds one LISTEN connection of its own (LEGION-690): it dials, listens on Channels,
// reports each notification, pings the connection when it is idle, wakes a caller that asked to
// be woken, and redials on any error. Run is the whole of it, and nothing is opened until it is
// called.
type Listener struct {
	// Dial opens the connection the listener hijacks for itself: a caller passes
	// store.Pool.Listener, a test a proxy's dial.
	Dial func(ctx context.Context) (*pgx.Conn, error)
	// Channels are the channels one LISTEN statement names, spliced into it as written: they are
	// the caller's own package constants, never a value read from the database or a request.
	Channels []string
	// ListenIdle and PingTimeout are DefaultListenIdle and DefaultPingTimeout when zero; a test
	// sets them lower.
	ListenIdle  time.Duration
	PingTimeout time.Duration
	// OnConnected runs after the LISTEN succeeds on a new connection, which is where a caller
	// catches up what it missed while it had none. Nil for a caller with nothing to catch up.
	OnConnected func()
	// OnNotify is called once per notification, on the listener's goroutine, so a caller that
	// does slow work in it delays the notifications behind it.
	OnNotify func(channel, payload string)
	// Backoff is the wait before the attempt-th redial in a row; nil is RedialBackoff.
	Backoff func(attempt int) time.Duration
	// NextWake and OnWake are a clock for a caller whose own work has to run on this listener's
	// goroutine: the evictor of stale document rooms decides everything inside OnNotify, so it
	// cannot put the work it defers on a time.AfterFunc, which would run it on a goroutine of its
	// own, beside a notification being decided. NextWake is asked, each time round the wait loop,
	// how long until that caller next wants waking; zero - or a nil field - is never. When that is
	// sooner than ListenIdle the wait ends there and OnWake is called on this goroutine instead of
	// the idle ping, which is for a connection that has been idle for ListenIdle and so says
	// nothing about one that has not. A notification that arrives first is reported as any other
	// is, and the wake is asked again on the next round. A caller that leaves both nil - the
	// delivery hub and the event stream relay - waits exactly as it did before.
	NextWake func() time.Duration
	OnWake   func()
}

// Run blocks until ctx ends, then closes its connection and returns. Each time a connection
// fails, or a dial or its LISTEN does, it closes that connection, logs why, waits the backoff for
// the number of failures in a row (a connection that got as far as its LISTEN starts the count
// again), and dials again. A notification sent while it had no connection is not reported: a
// caller that cannot afford that window catches up in OnConnected, which runs after every
// successful LISTEN, the first one included.
func (l *Listener) Run(ctx context.Context) {
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
		slog.Warn("dispatch: a Postgres listener has no connection; redialing",
			"channels", strings.Join(l.Channels, ","), "error", err, "attempt", failures, "wait_ms", wait.Milliseconds())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// listen dials one connection, listens on every channel, and reports what arrives on it until ctx
// ends or the connection fails, then closes it. listened is whether its LISTEN succeeded; err is
// why it stopped.
func (l *Listener) listen(ctx context.Context) (listened bool, err error) {
	conn, err := l.Dial(ctx)
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer l.close(conn)
	listenCtx, cancel := context.WithTimeout(ctx, l.pingWait())
	_, err = conn.Exec(listenCtx, l.statement())
	cancel()
	if err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	if l.OnConnected != nil {
		l.OnConnected()
	}
	for {
		wait, waking := l.waitFor()
		waitCtx, cancel := context.WithTimeout(ctx, wait)
		notification, err := conn.WaitForNotification(waitCtx)
		reached := errors.Is(waitCtx.Err(), context.DeadlineExceeded)
		cancel()
		if notification != nil {
			l.OnNotify(notification.Channel, notification.Payload)
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if !reached {
			return true, fmt.Errorf("wait for a notification: %w", err)
		}
		if waking {
			// The caller's wake, not ListenIdle: this connection has been waiting for as long as
			// the caller asked for and no longer, so there is nothing for the ping to find.
			l.OnWake()
			continue
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

// statement listens on every channel in one round trip: with no arguments pgx sends it over the
// simple protocol, which runs every statement in it.
func (l *Listener) statement() string {
	if len(l.Channels) == 0 {
		return ""
	}
	return "listen " + strings.Join(l.Channels, "; listen ")
}

// close closes conn within the ping timeout, so a half-open connection's Terminate cannot hold
// the listener; closing a connection pgx has already closed does nothing.
func (l *Listener) close(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), l.pingWait())
	defer cancel()
	_ = conn.Close(ctx)
}

func (l *Listener) idle() time.Duration {
	if l.ListenIdle == 0 {
		return DefaultListenIdle
	}
	return l.ListenIdle
}

// waitFor is how long the next wait for a notification runs, and whether its deadline is the
// caller's wake rather than the idle period: the sooner of the two, so a caller is woken on time
// however long ListenIdle is, and the ping stays with the wait that really was idle for all of it.
func (l *Listener) waitFor() (time.Duration, bool) {
	idle := l.idle()
	if l.NextWake == nil {
		return idle, false
	}
	wake := l.NextWake()
	if wake <= 0 || wake >= idle {
		return idle, false
	}
	return wake, true
}

func (l *Listener) pingWait() time.Duration {
	if l.PingTimeout == 0 {
		return DefaultPingTimeout
	}
	return l.PingTimeout
}

func (l *Listener) backoffFor(attempt int) time.Duration {
	if l.Backoff == nil {
		return RedialBackoff(attempt)
	}
	return l.Backoff(attempt)
}

// RedialBackoff is the default wait before the attempt-th redial in a row (from 1): a second,
// doubling to thirty, plus up to half of that again, so the tasks of a rolling deploy that lost
// one database together do not redial it in step. A caller that retries a read of its own on the
// same terms uses it too (the delivery hub's catch-up).
func RedialBackoff(attempt int) time.Duration {
	wait := backoffFirst
	for n := 1; n < attempt && wait < backoffMax; n++ {
		wait *= 2
	}
	wait = min(wait, backoffMax)
	return wait + rand.N(wait/2+1)
}
