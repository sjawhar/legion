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
	// A reconnect after the client stopped belongs to a process that is shutting down: closeStopped
	// closes the connection, or leaves it to the drain when the reconnect came before Drain stopped
	// the client.
	if c.stopped() {
		c.closeStopped(nc)
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
	pending, restoreErr := c.restoreSubscriptions()
	if errors.Is(restoreErr, errStopped) {
		// Stopped between the check above and the re-subscribe: same as that branch.
		c.closeStopped(nc)
		return
	}
	if restoreErr != nil {
		slog.Error("envoy nats resubscribe failed", slog.String("name", c.name), slog.String("error", restoreErr.Error()))
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
	hookErr := c.rewatch()
	if hookErr != nil {
		// A failure after the stop is the stop: a shutdown that began while a hook ran closed what
		// the hook was reading through. recover reports that case the same way, with the error kept
		// on the line in case a real failure coincided with the stop.
		if c.stopped() {
			slog.Info("envoy nats reconnect hooks cancelled", slog.String("name", c.name), slog.String("error", hookErr.Error()))
			return
		}
		slog.Error("envoy nats reconnect hook failed", slog.String("name", c.name), slog.String("error", hookErr.Error()))
	}
	// The recovery binds what the restore left unbound, a registration it left to its bind in flight
	// should that bind fail, and runs the hooks again after a failed run; until the next connection
	// event nothing else does either.
	if pending || restoreErr != nil || hookErr != nil {
		go c.recover()
	}
}

// closeStopped closes nc, the connection a reconnect callback reports once the client has stopped:
// nats.go has re-sent the subscriptions a drain is draining, and nothing will drain them now, so
// the connection is closed rather than hand deliveries to a process that is exiting. A reconnect
// that came before Drain stopped the client is the drain's: nats.go can run its callback only once
// the drain is under way, and by then the drain itself is draining what nats.go re-sent, so closing
// there would cut off the deliveries still in their handlers.
func (c *Client) closeStopped(nc *nats.Conn) {
	c.mu.Lock()
	// A callback reports at least one reconnect, and a client stopped by Close alone leaves the
	// count at 0, so only a reconnect Drain counted at its stop matches.
	reconnectedBeforeTheDrain := nc == c.Conn && nc.Stats().Reconnects == c.drainedReconnects
	c.mu.Unlock()
	if !reconnectedBeforeTheDrain {
		nc.Close()
	}
}

// AddReconnectHook registers recovery for state that is attached to NATS but not represented by
// Client subscriptions, such as KV watchers. After each connection event, a reconnect in place or a
// connection the client dials to replace a closed one, a run of the hooks on the client's connection
// begins that covers it, whatever restoring the subscriptions returned. A run covers every event
// counted before it began, so an event that lands between runs gets a run of its own and a burst of
// events that land before one begins gets one. Runs take turns, never two at once, and a run that
// fails is run again by a recovery, the only way an event gets a second run.
func (c *Client) AddReconnectHook(hook func(*nats.Conn) error) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.reconnectHooks = append(c.reconnectHooks, hook)
	c.reconnectHooksMu.Unlock()
}

// AddDisconnectHook registers what state attached to NATS, such as a KV watcher's cache, does when
// the client's connection is lost. nats.go reconnects forever and keeps a watcher's subscription
// open through the outage, so the watcher never ends and nothing else says its cache has stopped
// following the bucket. The hooks run on every disconnect of the client's connection, in the order
// they were added, on nats.go's callback goroutine and so before the reconnect hooks of the
// reconnect that follows. A hook returns at once: it runs inside that callback.
func (c *Client) AddDisconnectHook(hook func()) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.disconnectHooks = append(c.disconnectHooks, hook)
	c.reconnectHooksMu.Unlock()
}

// onDisconnect is wired as the DisconnectedErrCB callback, after its log line: it runs the
// disconnect hooks.
func (c *Client) onDisconnect() {
	c.reconnectHooksMu.Lock()
	hooks := slices.Clone(c.disconnectHooks)
	c.reconnectHooksMu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

// AddDrainHook registers an end Drain gives to state attached to NATS that the client does not
// subscribe for itself, such as KV watchers, while the connection still carries requests. Drain
// runs each hook once, in the order they were added, after the delivery subscriptions have drained
// or run out their share of the timeout, and before the connection's own drain, with ctx ending at
// Drain's deadline, or as soon as the connection is lost, with ErrConnectionLost as its cause. A
// drain of a connection that is reconnecting closes it at once and runs no hook, since nothing it
// sent would reach the server.
//
// The connection's own drain cannot end a KV watcher cleanly. nats.go deletes the ordered
// consumer it created for the watcher once it has removed the watcher's subscription
// (checkDrained), so the connection's drain, which waits only for its subscriptions, reaches its
// publishes, refuses the delete as "connection draining" or closes under it, depending on the
// scheduler. A hook that stops the watcher sends that delete itself, as a request it waits for.
func (c *Client) AddDrainHook(hook func(ctx context.Context)) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.drainHooks = append(c.drainHooks, hook)
	c.reconnectHooksMu.Unlock()
}

// ErrConnectionLost is the cause of a drain hook's context (context.Cause) when the connection
// was lost while the hooks ran. nats.go then refuses a request with the reconnect buffer off
// ("outbound buffer limit exceeded"), and a request already sent gets no answer, so what a hook
// had still to ask the server is left to the server: an ordered consumer whose interest is gone is
// deleted by its inactive threshold.
var ErrConnectionLost = errors.New("bus: the connection was lost during the drain")

// runDrainHooks runs the drain hooks one after another on conn, each bounded by deadline and by
// conn staying connected.
func (c *Client) runDrainHooks(conn *nats.Conn, deadline time.Time) {
	c.reconnectHooksMu.Lock()
	hooks := slices.Clone(c.drainHooks)
	c.reconnectHooksMu.Unlock()
	if len(hooks) == 0 {
		return
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx, cancelDeadline := context.WithDeadline(ctx, deadline)
	defer cancelDeadline()
	lost := conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED)
	defer conn.RemoveStatusListener(lost)
	if !conn.IsConnected() {
		cancel(ErrConnectionLost)
	}
	go func() {
		select {
		case _, ok := <-lost:
			if ok {
				cancel(ErrConnectionLost)
			}
		case <-ctx.Done():
		}
	}()
	for _, hook := range hooks {
		hook(ctx)
	}
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

// recover attempts to restore the NATS connection and every recoverable subscription after a
// CLOSED state, a failed re-subscribe or a failed run of the reconnect hooks. One runs at a time: a
// caller that finds one running starts none, so the running one looks again once it has cleared its
// flag, and a failure that lands as it ends is not left to the next connection event.
func (c *Client) recover() {
	for atomic.CompareAndSwapInt32(&c.recovering, 0, 1) {
		c.recoverUntilHealthy()
		atomic.StoreInt32(&c.recovering, 0)
		if c.stopped() || (c.subscriptionsHealthy() && !c.hooksDue()) {
			return
		}
	}
}

// recoverUntilHealthy retries, with backoff, until the connection, every subscription and the
// reconnect hooks are current, or the client stops.
func (c *Client) recoverUntilHealthy() {
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
			slog.Info("envoy nats recovery cancelled", slog.String("name", c.name))
			return
		}
		if c.subscriptionsHealthy() && !c.hooksDue() {
			slog.Info("envoy nats recovery: already healthy", slog.String("name", c.name))
			return
		}
		slog.Info("envoy nats recovery attempt", slog.String("name", c.name), slog.Int("attempt", attempt))
		failure := "envoy nats recovery reconnect failed"
		pending := false
		err := c.ensureConnWithContext(ctx)
		if err == nil {
			failure = "envoy nats recovery resubscribe failed"
			pending, err = c.restoreSubscriptions()
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
				slog.Info("envoy nats recovery cancelled", slog.String("name", c.name), slog.String("error", err.Error()))
			} else {
				slog.Info("envoy nats recovery cancelled", slog.String("name", c.name))
			}
			return
		}
		if err == nil && !pending {
			slog.Info("envoy nats recovery successful", slog.String("name", c.name), slog.Int("attempt", attempt))
			return
		}
		// A registration left to its bind in flight is no failure: the restore logged it at INFO,
		// and the next attempt finds it bound or binds it.
		if err != nil {
			slog.Error(failure, slog.String("name", c.name), slog.Int("attempt", attempt), slog.String("error", err.Error()))
		}
		select {
		case <-c.stopCh:
			slog.Info("envoy nats recovery cancelled", slog.String("name", c.name))
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

// restoreSubscriptions binds again every registered subscription the connection lost, and reports
// apart from its error whether it left one pending: a registration whose bind is in flight is that
// bind's, which installs it when it ends. Nothing failed there, so it is logged at INFO, and the
// caller's recovery binds the registration should that bind fail.
func (c *Client) restoreSubscriptions() (pending bool, err error) {
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	// Drain collects the subscriptions to drain under subscriptionsMu after stopping the client, so
	// a recovery that reaches here after it re-subscribes nothing.
	if c.stopped() {
		c.subscriptionsMu.Unlock()
		return false, errStopped
	}
	// Every subscription is attempted, so one that cannot be restored leaves the others delivering: a
	// listener waiting out the task that holds its durable has an unbound JetStream subscription
	// beside a core role lane that is already a member of its machine's queue group.
	var errs []error
	var restoring []*recoverableSubscription
	var pendingSubjects []string
	for index := range c.subscriptions {
		subscription := &c.subscriptions[index]
		// A reconnect in place leaves a subscription valid: nats.go has already re-sent its SUB, so
		// it delivers as it did. Unsubscribing it to bind again would race the server's release of
		// the consumer's push binding, which refuses the new bind while it still sees the old one.
		// One on a replaced connection is invalid and is bound again.
		if subscription.handler == nil || subscription.active.IsValid() {
			continue
		}
		// A registration whose bind is in flight (the listener's bind attempt, whose lookup a
		// reconnect can lose, to be waited out for JetStream's 10 s) is that bind's: a second bind
		// beside it could leave the registration bound twice.
		if subscription.binding != nil {
			pendingSubjects = append(pendingSubjects, subscription.subject)
			continue
		}
		subscription.binding = make(chan struct{})
		restoring = append(restoring, subscription)
	}
	registrations := make([]recoverableSubscription, len(restoring))
	for index, subscription := range restoring {
		registrations[index] = *subscription
	}
	c.subscriptionsMu.Unlock()
	for _, subject := range pendingSubjects {
		slog.Info("envoy nats resubscribe left to the bind in flight", slog.String("name", c.name), slog.String("subject", subject))
	}
	for index, subscription := range restoring {
		slog.Info("envoy nats resubscribing", slog.String("name", c.name), slog.String("subject", registrations[index].subject))
		sub, err := bind(registrations[index], conn, js)
		if err := c.endBind(subscription, sub, err); err != nil {
			errs = append(errs, err)
		}
	}
	return len(pendingSubjects) > 0, errors.Join(errs...)
}
