package bus

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// onClosed is wired as the ClosedCB callback. It launches recovery in a
// background goroutine so the NATS library callback returns immediately.
func (c *Client) onClosed() {
	go c.recover()
}

func (c *Client) onReconnect(nc *nats.Conn) {
	// A reconnect while Drain runs belongs to a process that is shutting down: nats.go has re-sent
	// the draining subscriptions, and nothing will drain them now, so close the connection rather
	// than hand deliveries to a process that is exiting.
	if c.stopped() {
		nc.Close()
		return
	}
	// nc is the connection already in c.Conn, reconnected in place, and the JetStream context taken
	// from it keeps working, so neither field is reassigned here. The reconnect is a connection
	// event, so the hooks are due. A callback nats.go delivers late for a connection the client has
	// since replaced is not: the replacement was its own event.
	c.mu.Lock()
	if nc == c.Conn {
		c.connEvents++
	}
	c.mu.Unlock()
	restoreErr := c.restoreSubscriptions()
	if errors.Is(restoreErr, errStopped) {
		// Stopped between the check above and the re-subscribe: same as that branch.
		nc.Close()
		return
	}
	if restoreErr != nil {
		slog.Error("envoy nats resubscribe failed", slog.String("error", restoreErr.Error()))
	}
	// Drain stops the client before it takes the subscriptions to drain them, so a stop can land
	// while restoreSubscriptions runs. The drain then owns the subscriptions and the connection, and
	// a hook would only rewatch state the drain is about to close, as recover's attempt skips them.
	if c.stopped() {
		return
	}
	// The hooks run whatever the restore returned: the state they move holds no subscription, and
	// while a listener waits out the task that holds its durable, the durable's restore is refused
	// by design while /v1 and the role lane already read that state.
	if err := c.rewatch(); err != nil {
		// A failure after the stop is the stop: a shutdown that began while a hook ran closed what
		// the hook was reading through. recover reports that case the same way, with the error kept
		// on the line in case a real failure coincided with the stop.
		if c.stopped() {
			slog.Info("envoy nats reconnect hooks cancelled", slog.String("error", err.Error()))
			return
		}
		slog.Error("envoy nats reconnect hook failed", slog.String("error", err.Error()))
	}
	if restoreErr != nil {
		go c.recover()
	}
}

// AddReconnectHook registers recovery for state that is attached to NATS but not represented by
// Client subscriptions, such as KV watchers. The hooks run on the client's connection once for each
// connection event, a reconnect in place or a connection the client dials to replace a closed one,
// whatever restoring the subscriptions returned. Runs take turns, never two at once, and a recovery
// runs the hooks again only after a run failed.
func (c *Client) AddReconnectHook(hook func(*nats.Conn) error) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.reconnectHooks = append(c.reconnectHooks, hook)
	c.reconnectHooksMu.Unlock()
}

// rewatch runs the reconnect hooks on the current connection while they are due (hooksDue), one
// run at a time: a caller that finds a run in progress waits for it to end, then runs the hooks only
// if they are still due. A run that succeeds covers the connection events counted when it began, so
// an event during the run leaves the hooks due for one more.
func (c *Client) rewatch() error {
	c.rewatchMu.Lock()
	defer c.rewatchMu.Unlock()
	c.mu.Lock()
	conn, events := c.Conn, c.connEvents
	due := events != c.hookedEvents
	c.mu.Unlock()
	if !due {
		return nil
	}
	if err := c.runReconnectHooks(conn); err != nil {
		return err
	}
	c.mu.Lock()
	c.hookedEvents = events
	c.mu.Unlock()
	return nil
}

// hooksDue reports whether a connection event has yet to be covered by a run of the reconnect hooks
// that succeeded. It still reads true while the run covering the event is in progress, so a caller
// that acts on it goes through rewatch, which decides under its lock.
func (c *Client) hooksDue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connEvents != c.hookedEvents
}

func (c *Client) runReconnectHooks(conn *nats.Conn) error {
	c.reconnectHooksMu.Lock()
	hooks := slices.Clone(c.reconnectHooks)
	c.reconnectHooksMu.Unlock()
	for _, hook := range hooks {
		if err := hook(conn); err != nil {
			return err
		}
	}
	return nil
}

// recover attempts to restore the NATS connection and every recoverable
// subscription after a CLOSED state or failed re-subscribe.
func (c *Client) recover() {
	if !atomic.CompareAndSwapInt32(&c.recovering, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&c.recovering, 0)

	// The dial retries a lost server for up to ten attempts; the stop cancels it, so a recovery
	// ends at Close or Drain instead of outliving the client.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-c.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for attempt := 1; ; attempt++ {
		if c.stopped() {
			slog.Info("envoy nats recovery cancelled")
			return
		}
		if c.subscriptionsHealthy() && !c.hooksDue() {
			slog.Info("envoy nats recovery: already healthy")
			return
		}
		slog.Info("envoy nats recovery attempt", slog.Int("attempt", attempt))
		failure := "envoy nats recovery reconnect failed"
		err := c.ensureConnWithContext(ctx)
		if err == nil {
			failure = "envoy nats recovery resubscribe failed"
			err = c.restoreSubscriptions()
			// The hooks move onto a connection they have not reached whatever the restore returned,
			// as onReconnect runs them, and only while they are due: a durable a listener waits out
			// keeps the restore failing, attempt after attempt, while the hooks' state is already
			// current. A run onReconnect has in progress is waited for, never joined by a second.
			if !c.stopped() {
				err = errors.Join(err, c.rewatch())
			}
		}
		// A failure after the stop is the stop: the dial it cancelled, a subscription the drain
		// closed, a connection it refused to install. None of them is a recovery failure, but the
		// error stays on the line in case a real one coincided with the stop.
		if c.stopped() {
			if err != nil {
				slog.Info("envoy nats recovery cancelled", slog.String("error", err.Error()))
			} else {
				slog.Info("envoy nats recovery cancelled")
			}
			return
		}
		if err == nil {
			slog.Info("envoy nats recovery successful", slog.Int("attempt", attempt))
			return
		}
		slog.Error(failure, slog.Int("attempt", attempt), slog.String("error", err.Error()))
		select {
		case <-c.stopCh:
			slog.Info("envoy nats recovery cancelled")
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (c *Client) subscriptionsHealthy() bool {
	c.mu.Lock()
	connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	for _, subscription := range c.subscriptions {
		if subscription.handler != nil && (subscription.active == nil || !subscription.active.IsValid()) {
			return false
		}
	}
	return connOK
}

func (c *Client) restoreSubscriptions() error {
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	// Drain collects the subscriptions to drain under subscriptionsMu after stopping the client, so
	// a recovery that reaches here after it re-subscribes nothing.
	if c.stopped() {
		return errStopped
	}
	// Every subscription is attempted, so one that cannot be restored leaves the others delivering: a
	// listener waiting out the task that holds its durable has an unbound JetStream subscription
	// beside a core role lane that is already a member of its machine's queue group.
	var errs []error
	for index := range c.subscriptions {
		subscription := &c.subscriptions[index]
		// A reconnect in place leaves a subscription valid: nats.go has already re-sent its SUB, so
		// it delivers as it did. Unsubscribing it to bind again would race the server's release of
		// the consumer's push binding, which refuses the new bind while it still sees the old one.
		// One on a replaced connection is invalid and is bound again.
		if subscription.handler == nil || subscription.active.IsValid() {
			continue
		}
		slog.Info("envoy nats resubscribing", slog.String("subject", subscription.subject))
		if err := restoreSubscription(subscription, conn, js); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
