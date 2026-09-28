package bus

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// ErrPublishDenied is returned, wrapping nats.ErrPermissionViolation and the server's own text,
// for a core publish the server refused under its connection's publish permissions.
var ErrPublishDenied = errors.New("NATS denied the publish")

// errPublishUnconfirmed is wrapped, with the reason, by a core publish confirmPublished cannot
// confirm: not known to be denied, and not known to be accepted either.
var errPublishUnconfirmed = errors.New("publish not known to have been accepted")

// publishConfirmed publishes data to subject on conn, with reply as its reply subject when it is not
// empty, flushes within deadline, and confirms the server accepted it (confirmPublished). A nil
// error means the server accepted the publish; otherwise the error is nats.go's refusal before
// sending (refused), a flush that failed or ran out of the window, ErrPublishDenied, or one
// wrapping errPublishUnconfirmed.
func (c *Client) publishConfirmed(conn *nats.Conn, subject, reply string, data []byte, deadline time.Time) error {
	before := observe(conn)
	var err error
	if reply == "" {
		err = conn.Publish(subject, data)
	} else {
		err = conn.PublishRequest(subject, reply, data)
	}
	if err != nil {
		return c.refused(err, len(data))
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("bus: window elapsed before the publish to %q was flushed", subject)
	}
	if err := conn.FlushTimeout(remaining); err != nil {
		return fmt.Errorf("bus: flush publish to %q: %w", subject, err)
	}
	return confirmPublished(conn, subject, before)
}

// connState is what confirmPublished compares across a core publish and its flush.
type connState struct {
	lastError  error
	connected  bool
	reconnects uint64
}

// observe reads the last error, then whether the connection is connected, then its reconnect
// count, in that order, so that a reconnect's clearing of the last error is always seen with one of
// the other two (confirmPublished).
func observe(conn *nats.Conn) connState {
	lastError := conn.LastError()
	connected := conn.IsConnected()
	return connState{lastError: lastError, connected: connected, reconnects: conn.Stats().Reconnects}
}

// confirmPublished reports whether a core publish to subject, made after before was observed and
// followed by a flush that succeeded, may count as accepted by the server.
//
// Core NATS acknowledges nothing: the server answers a publish its connection may not make with
// `-ERR 'Permissions Violation for Publish to "<subject>"'` and drops it, and nats.go reports that
// only to the async error callback. It does record it first, synchronously: the one read loop
// parses the -ERR (processErr) and sets the connection's last error (processTransientError, under
// the connection's lock) before it parses anything after it. The server queues that -ERR on the
// connection's outbound buffer while it processes the PUB, before it processes the flush's PING and
// queues the PONG behind it, so a flush that returns has already seen the -ERR recorded.
//
// nats.go keeps only the last error, one per connection, so a violation cannot be attributed to
// one publish when other goroutines publish on the same connection: another error recorded after
// ours would overwrite it. So any change of the last error across the publish and its flush fails
// the publish, named as ErrPublishDenied when the error is the violation of this very subject, and
// as unconfirmed otherwise; the cost is that a concurrent failure elsewhere on the connection fails
// this publish too, and its caller retries it.
//
// A reconnect clears the last error (doReconnect), so a connection that drops right behind the
// flush's PONG would read as unchanged. The clear happens only after the drop has marked the
// connection reconnecting (processOpErr), and a reconnect counts itself before it marks the
// connection connected again, both under the connection's lock; so reading the error, then the
// status, then the count, a cleared error comes with a status that is not connected or a count
// that moved, and either fails the publish as unconfirmed. Not connected is IsConnected, which
// counts a draining connection as connected, so a publish during Drain still confirms, and a
// closed one as not.
//
// One change goes unseen: the last error set to a shared nats.go value (ErrSlowConsumer,
// ErrMaxSubscriptionsExceeded), then this publish's violation, then that same value again, all
// between the two observations. nats.go exposes no error counter to tell that from no change. Both
// connections that publish on core subjects carry subscriptions - Dispatch's its JetStream reply
// inboxes and the webhook sweeper's KV watch, the listener's its notification stream consumer and
// the role-lane subscription whose handler forwards - but Envoy sets no pending limits, and at
// nats.go's defaults (500,000 messages, 64 MiB per subscription) a subscription that falls far
// enough behind to report a slow consumer, twice around one publish, is rare. So it is accepted.
func confirmPublished(conn *nats.Conn, subject string, before connState) error {
	after := observe(conn)
	if !after.connected || after.reconnects != before.reconnects {
		return fmt.Errorf("bus: %w: NATS disconnected during the publish to %q", errPublishUnconfirmed, subject)
	}
	if sameError(after.lastError, before.lastError) {
		return nil
	}
	if errors.Is(after.lastError, nats.ErrPermissionViolation) &&
		strings.Contains(after.lastError.Error(), fmt.Sprintf("Publish to %q", subject)) {
		return fmt.Errorf("bus: %w to %q: %w", ErrPublishDenied, subject, after.lastError)
	}
	return fmt.Errorf("bus: %w: NATS reported an error during the publish to %q: %w",
		errPublishUnconfirmed, subject, after.lastError)
}

// sameError reports whether a and b are the same error value. nats.go records each permissions
// violation as a new error (processErr), so a denial never equals the error it replaced; a shared
// nats.go value recorded again (confirmPublished names the one that matters) does. A value that
// cannot be compared - a struct holding a slice in an interface field, say - counts as different
// rather than panicking.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return reflect.ValueOf(a).Comparable() && a == b
}
