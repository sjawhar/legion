// Package pglisten holds one Postgres LISTEN connection that redials: the loop every feature that
// learns of another process's commits from the database runs. Two envoy-dispatch processes serve
// one Postgres (INFRA-66), and the secrets broker now runs two pods over one database of its own,
// so what one process commits reaches the others only through the database, and a connection
// waiting for a notification must outlive every failure between it and the server. Its Dispatch
// users are the delivery hub, the event stream relay and, under DISPATCH_TEST_HOOKS alone, the
// test hooks' fan-out to every process (internal/dispatch/api), and the process's shared listener
// hub (Hub), on which the document rooms' room sync is registered (internal/dispatch/docs) and
// every change-driven background loop of the server takes its wake (cmd/dispatch); the other is
// the broker's cross-pod secret-reread fan-out (internal/broker/reread). Serving two programs is
// why the package sits here rather than under internal/dispatch, as internal/pgmigrate does for
// the two migration runners.
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
	// DefaultListenIdle is how long a listener goes without a proven round trip - the LISTEN that
	// opened its connection, a notification, a ping - before it pings that connection. A
	// connection that went half-open (a dropped NAT entry, a failover that left the socket up)
	// delivers nothing and reports no error; only a round trip finds it.
	DefaultListenIdle = 30 * time.Second
	// DefaultPingTimeout bounds that ping, the LISTEN statement on a new connection, and the
	// close of a connection the listener gives up.
	DefaultPingTimeout = 5 * time.Second
	// The default backoff's first wait and its ceiling.
	backoffFirst = time.Second
	backoffMax   = 30 * time.Second
)

// Listener holds one LISTEN connection of its own (LEGION-690): it dials, listens on Channels,
// reports each notification, pings the connection when it is idle, and redials on any error. Run
// is the whole of it, and nothing is opened until it is called.
type Listener struct {
	// Owner names the subsystem this listener belongs to, and is the prefix of the one line a
	// listener logs ("<owner>: a Postgres listener has no connection; redialing"). It is a field
	// rather than the word baked into that line because the package now serves two programs whose
	// logs an operator reads apart - envoy-dispatch and the secrets broker - and a line that
	// names neither leaves the reader of one of those logs to guess which program lost its
	// database. Empty is "dispatch" (owner), the exact text this line carried while the package
	// was Dispatch's alone, so a deployment alarming on that text keeps matching whatever a new
	// caller forgets to set; the three Dispatch call sites set it all the same, so each reads at
	// its own site which subsystem it is.
	Owner string
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
	// does slow work in it delays the notifications behind it. Nil drops every notification:
	// the listener keeps its connection, waits and pings on it and still runs OnConnected after
	// each LISTEN, and a caller with nowhere to report a notification simply never hears one.
	// Dropping rather than refusing to run such a listener, because the only louder answer
	// available this far down is the panic the unchecked call used to make, and a nil field of
	// one caller's takes every listener in the process with it - the delivery hub's and the
	// event stream relay's included, since they run in it.
	OnNotify func(channel, payload string)
	// Backoff is the wait before the attempt-th redial in a row; nil is RedialBackoff.
	Backoff func(attempt int) time.Duration
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
		slog.Warn(l.owner()+": a Postgres listener has no connection; redialing",
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
	// The last round trip this connection proved it could make: the LISTEN above, a notification
	// it carried, a ping it answered. The ping in the loop below falls due one idle period after
	// this stamp, so each wait runs only what is left of that period.
	proven := time.Now()
	if l.OnConnected != nil {
		l.OnConnected()
	}
	for {
		waitCtx, cancel := context.WithTimeout(ctx, l.idle()-time.Since(proven))
		notification, err := conn.WaitForNotification(waitCtx)
		reached := errors.Is(waitCtx.Err(), context.DeadlineExceeded)
		cancel()
		if notification != nil {
			// The connection carried it, which is a round trip of its own; stamped before
			// OnNotify, whose work is the caller's and proves nothing further.
			proven = time.Now()
			if l.OnNotify != nil {
				l.OnNotify(notification.Channel, notification.Payload)
			}
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
		// The wait ran out the idle period since the last proven round trip. pgx keeps the
		// connection open when a wait ends at its deadline, so the same connection answers the
		// ping; one that has gone half-open fails it at pingTimeout.
		pingCtx, cancel := context.WithTimeout(ctx, l.pingWait())
		err = conn.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return true, fmt.Errorf("ping an idle connection: %w", err)
		}
		proven = time.Now()
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

// owner is the prefix of the one line Run logs: the subsystem Owner names, or dispatch for a
// listener that names none, which is the prefix this line carried for its whole life as
// Dispatch's own (Owner says why that fallback is the old text and not the empty prefix).
func (l *Listener) owner() string {
	if l.Owner == "" {
		return "dispatch"
	}
	return l.Owner
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
