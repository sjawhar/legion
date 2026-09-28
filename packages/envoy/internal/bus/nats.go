package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ConnectOption configures the bus client.
type ConnectOption func(*connectOpts)

type connectOpts struct {
	replicas                    int
	publishAcknowledgementClock AcknowledgementClock
}

// AcknowledgementClock supplies deadline contexts for JetStream publish acknowledgements.
type AcknowledgementClock interface {
	WithTimeout(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

type wallClock struct{}

func (wallClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// WithPublishAcknowledgementClock overrides the clock that bounds JetStream publish acknowledgements.
func WithPublishAcknowledgementClock(clock AcknowledgementClock) ConnectOption {
	return func(o *connectOpts) { o.publishAcknowledgementClock = clock }
}

// WithReplicas overrides the replica count of the stream ConnectOwningStream ensures (default 1).
func WithReplicas(n int) ConnectOption {
	return func(o *connectOpts) { o.replicas = n }
}

type subscriptionTransport uint8

const (
	jetStreamSubscription subscriptionTransport = iota
	coreSubscription
	subscriptionCount
)

type recoverableSubscription struct {
	transport subscriptionTransport
	subject   string
	queue     string
	handler   nats.MsgHandler
	opts      []nats.SubOpt
	active    *nats.Subscription
}

type Client struct {
	Conn                        *nats.Conn
	js                          nats.JetStreamContext
	urls                        []string
	credential                  nats.Option
	publishAcknowledgementClock AcknowledgementClock
	mu                          sync.Mutex

	subscriptionsMu sync.Mutex
	subscriptions   [subscriptionCount]recoverableSubscription

	reconnectHooksMu sync.Mutex
	reconnectHooks   []func(*nats.Conn) error

	// recovery state
	recovering int32
	stopCh     chan struct{}
	closeOnce  sync.Once
}

// options starts from nats.GetDefaultOptions: Connect fills in only some zero fields, and the
// defaults are what turn on reconnecting, the client's pings and the drain and flusher timeouts. A
// lost server is reconnected in place, forever, so JetStream handles taken from the connection
// keep working; ReconnectedCB re-subscribes and runs the reconnect hooks. ClosedCB fires on Close
// or Drain, and also when nats.go gives up on the connection itself: the same authorization error
// twice in a row, or a server -ERR it does not classify. Those two are why onClosed still starts a
// recovery that dials a replacement. The reconnect buffer is off, so nothing is held and sent after its caller
// saw an error: a publish while reconnecting waits for the reconnect instead
// (ensureConnWithContext), and a publish that fails was not sent.
func options(name string, urls []string, reconnectCB func(*nats.Conn), closedCB func()) nats.Options {
	opts := nats.GetDefaultOptions()
	opts.Servers = urls
	opts.Name = name
	opts.NoRandomize = true
	opts.Timeout = 5 * time.Second
	opts.MaxReconnect = -1
	// A publish while reconnecting waits for the reconnect (ensureConnWithContext), so the wait
	// between attempts is how long it waits after NATS is back.
	opts.ReconnectWait = time.Second
	opts.ReconnectBufSize = -1
	opts.DisconnectedErrCB = func(_ *nats.Conn, err error) {
		if err != nil {
			slog.Info("envoy nats disconnected", slog.String("error", err.Error()))
			return
		}
		slog.Info("envoy nats disconnected")
	}
	opts.ReconnectedCB = func(nc *nats.Conn) {
		slog.Info("envoy nats reconnected", slog.String("url", nc.ConnectedUrl()))
		if reconnectCB != nil {
			reconnectCB(nc)
		}
	}
	opts.ClosedCB = func(_ *nats.Conn) {
		slog.Info("envoy nats connection closed")
		if closedCB != nil {
			closedCB()
		}
	}
	opts.AsyncErrorCB = func(_ *nats.Conn, sub *nats.Subscription, err error) {
		level := slog.LevelError
		switch {
		case err == nats.ErrConsumerNotFound:
			// A drain ends each subscription whose consumer nats.go created, then deletes that
			// consumer. At shutdown the listener leaves a KV watcher the last reconnect has not yet
			// replaced for the drain to end, and that watcher's ordered consumer can already be gone:
			// a NATS restart loses it outright (nats.go creates it with memory storage), and a
			// disconnect longer than its inactive threshold lets the server delete it. So the delete
			// finds it gone: the state the delete was for. At the pinned nats.go (v1.50.0) that
			// delete, in checkDrained, is the only report of the bare sentinel, which
			// DeleteConsumer returns; re-check that on a nats.go bump. The one other report of this
			// error, an ordered consumer nats.go failed to recreate, wraps it and stays an ERROR.
			level = slog.LevelWarn
		case errors.Is(err, nats.ErrConsumerNotActive):
			// Every consumer Envoy runs with idle heartbeats is a KV watcher's ordered consumer,
			// which reports missed heartbeats only while the connection is not connected; once it
			// is connected again, nats.go resets the consumer instead. The report restates the
			// disconnect logged above, once per watcher. The listener's durable carries no heartbeat
			// as long as cmd/listener's startListenerSubscription bound it, since that refuses one that
			// does; a replaced connection binds the same durable again without that check.
			level = slog.LevelWarn
		}
		if sub != nil {
			slog.Log(context.Background(), level, "envoy nats async error", slog.String("subject", sub.Subject), slog.String("error", err.Error()))
			return
		}
		slog.Log(context.Background(), level, "envoy nats async error", slog.String("error", err.Error()))
	}
	return opts
}

func connect(name string, urls []string, credential nats.Option, reconnectCB func(*nats.Conn), closedCB func()) (*nats.Conn, error) {
	return connectWithContext(context.Background(), name, urls, credential, reconnectCB, closedCB)
}

// connectWithContext dials urls with envoy's options and, when credential is not nil, as the NATS
// user it names (nkeyCredential).
func connectWithContext(ctx context.Context, name string, urls []string, credential nats.Option, reconnectCB func(*nats.Conn), closedCB func()) (*nats.Conn, error) {
	var lastErr error
	for range 10 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next := options(name, urls, reconnectCB, closedCB)
		if credential != nil {
			if err := credential(&next); err != nil {
				return nil, err
			}
		}
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// Not ctx.Err(): the deadline can pass before the context's timer marks it done,
				// and a nil error here would hand the caller a nil connection to use.
				return nil, context.DeadlineExceeded
			}
			if remaining < next.Timeout {
				next.Timeout = remaining
			}
		}
		type connectionResult struct {
			conn *nats.Conn
			err  error
		}
		result := make(chan connectionResult, 1)
		go func() {
			conn, err := next.Connect()
			result <- connectionResult{conn: conn, err: err}
		}()
		select {
		case connected := <-result:
			if connected.err == nil {
				return connected.conn, nil
			}
			lastErr = connected.err
		case <-ctx.Done():
			go func() {
				connected := <-result
				if connected.conn != nil {
					connected.conn.Close()
				}
			}()
			return nil, ctx.Err()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

// Dial opens a tuned core NATS connection using envoy's standard options (5s connect timeout,
// infinite reconnect every second, retry-loop for the initial 10 attempts), as the NATS user the
// environment names (nkeyCredential). A caller that needs core pub/sub on a connection of its own
// uses it: nats.go re-subscribes core subscriptions on reconnect by itself, so it needs none of
// Client's recovery, and it touches no stream. It refuses a NATS server that is not this machine's
// on the same terms as Connect.
//
// For JetStream-backed publishing or a durable consumer, use Connect; to reconcile the stream
// this codebase owns, ConnectOwningStream.
func Dial(name string, urls []string) (*nats.Conn, error) {
	if err := refuseRemoteNATS(urls); err != nil {
		return nil, err
	}
	credential, err := nkeyCredential(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	return connect(name, urls, credential, nil, nil)
}

// Connect opens a client that publishes and subscribes without touching the shared
// ENVOY_NOTIFICATIONS stream. Every caller that only publishes or only tails uses it -- natstail,
// the MCP bridge and envoy-dispatch's operator commands own nothing on the server they reach, and
// a stream ensure from one of them rewrites a resource several deployments share (LEGION-249).
//
// Every client, from Connect or ConnectOwningStream, connects as the NATS user the environment
// names: NATS_NKEY_SEED_FILE or NATS_NKEY_SEED (nkeyCredential). An unusable seed is an error
// before any dial; neither variable set connects without a credential. Every connection the client
// dials later, to recover, is the same user's.
//
// For the JetStream stream this codebase owns, use ConnectOwningStream.
func Connect(urls []string, options ...ConnectOption) (*Client, error) {
	if err := refuseRemoteNATS(urls); err != nil {
		return nil, err
	}
	return newClient(urls, false, options)
}

// ConnectOwningStream opens a client that also reconciles ENVOY_NOTIFICATIONS against this
// binary's subjects, retention and duplicate window (ensureStreamWithConfig), creating the stream
// when the server has none. Only the deployed services call it: envoy-listener and
// envoy-dispatch's server.
func ConnectOwningStream(urls []string, options ...ConnectOption) (*Client, error) {
	if err := refuseRemoteNATS(urls); err != nil {
		return nil, err
	}
	return newClient(urls, true, options)
}

func newClient(urls []string, ownsStream bool, options []ConnectOption) (*Client, error) {
	opts := connectOpts{replicas: 1, publishAcknowledgementClock: wallClock{}}
	for _, o := range options {
		o(&opts)
	}
	credential, err := nkeyCredential(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	c := &Client{
		urls:                        urls,
		credential:                  credential,
		publishAcknowledgementClock: opts.publishAcknowledgementClock,
		stopCh:                      make(chan struct{}),
	}
	nc, err := connect("envoy", urls, credential, c.onReconnect, c.onClosed)
	if err != nil {
		return nil, err
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		nc.Close()
		return nil, err
	}
	if ownsStream {
		cfg := *streamCfg
		cfg.Replicas = opts.replicas
		if err := ensureStreamWithConfig(js, &cfg); err != nil {
			nc.Close()
			return nil, err
		}
	}
	c.Conn = nc
	c.js = js
	return c, nil
}

func (c *Client) JS() nats.JetStreamContext {
	return c.js
}

// Connected reports whether the client currently has a live NATS connection.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn != nil && c.Conn.Status() == nats.CONNECTED
}

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
	// from it keeps working, so neither field is reassigned here.
	if err := c.restoreSubscriptions(); err != nil {
		if errors.Is(err, errStopped) {
			// Stopped between the check above and the re-subscribe: same as that branch.
			nc.Close()
			return
		}
		slog.Error("envoy nats resubscribe failed", slog.String("error", err.Error()))
		go c.recover()
		return
	}
	// Drain stops the client before it takes the subscriptions to drain them, so a stop can land
	// while restoreSubscriptions runs. The drain then owns the subscriptions and the connection, and
	// a hook would only rewatch state the drain is about to close, as recover's attempt skips them.
	if c.stopped() {
		return
	}
	if err := c.runReconnectHooks(nc); err != nil {
		// A failure after the stop is the stop: a shutdown that began while a hook ran closed what
		// the hook was reading through. recover reports that case the same way, with the error kept
		// on the line in case a real failure coincided with the stop.
		if c.stopped() {
			slog.Info("envoy nats reconnect hooks cancelled", slog.String("error", err.Error()))
			return
		}
		slog.Error("envoy nats reconnect hook failed", slog.String("error", err.Error()))
	}
}

// AddReconnectHook registers recovery for state that is attached to NATS but
// not represented by Client subscriptions, such as KV watchers.
func (c *Client) AddReconnectHook(hook func(*nats.Conn) error) {
	if hook == nil {
		return
	}
	c.reconnectHooksMu.Lock()
	c.reconnectHooks = append(c.reconnectHooks, hook)
	c.reconnectHooksMu.Unlock()
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

// Subscribe creates the listener's recoverable JetStream subscription. The client keeps one
// JetStream subscription: a later call unsubscribes the handle an earlier one returned.
func (c *Client) Subscribe(subject string, handler nats.MsgHandler, opts ...nats.SubOpt) (*nats.Subscription, error) {
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	return c.registerSubscription(recoverableSubscription{
		transport: jetStreamSubscription,
		subject:   subject,
		handler:   handler,
		opts:      opts,
	}, conn, js)
}

// SubscribeCore creates a recoverable core NATS subscription. The optional queue
// identifies the stable queue group that shares role-lane delivery between
// overlapping listeners. The client keeps one core subscription: a later call
// unsubscribes the handle an earlier one returned.
func (c *Client) SubscribeCore(subject string, handler nats.MsgHandler, queues ...string) (*nats.Subscription, error) {
	if err := c.ensureConn(); err != nil {
		return nil, err
	}
	queue := ""
	if len(queues) > 0 {
		queue = queues[0]
	}
	c.mu.Lock()
	conn, js := c.Conn, c.js
	c.mu.Unlock()
	return c.registerSubscription(recoverableSubscription{
		transport: coreSubscription,
		subject:   subject,
		queue:     queue,
		handler:   handler,
	}, conn, js)
}

func (c *Client) registerSubscription(next recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext) (*nats.Subscription, error) {
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()
	subscription := &c.subscriptions[next.transport]
	// Registering replaces the transport's subscription, so the one it replaces stops delivering:
	// the listener's self-health rebuild re-registers after its durable consumer was lost, and the
	// old handle, bound to that consumer's deliver inbox, would otherwise stay subscribed for good.
	// For a consumer the library created, Unsubscribe also deletes it, as it always does.
	if subscription.active != nil {
		_ = subscription.active.Unsubscribe()
	}
	*subscription = next
	if err := restoreSubscription(subscription, conn, js); err != nil {
		return nil, err
	}
	return subscription.active, nil
}

func restoreSubscription(subscription *recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext) error {
	var (
		sub *nats.Subscription
		err error
	)
	switch subscription.transport {
	case jetStreamSubscription:
		sub, err = js.Subscribe(subscription.subject, subscription.handler, subscription.opts...)
	case coreSubscription:
		if subscription.queue == "" {
			sub, err = conn.Subscribe(subscription.subject, subscription.handler)
		} else {
			sub, err = conn.QueueSubscribe(subscription.subject, subscription.queue, subscription.handler)
		}
	}
	if err != nil {
		return err
	}
	subscription.active = sub
	return nil
}

// SubOK reports whether the durable listener subscription is active on a live connection.
func (c *Client) SubOK() bool {
	c.mu.Lock()
	connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
	c.mu.Unlock()
	c.subscriptionsMu.Lock()
	subscription := c.subscriptions[jetStreamSubscription]
	subOK := subscription.active != nil && subscription.active.IsValid()
	c.subscriptionsMu.Unlock()
	return connOK && subOK
}

// errStopped is what a client refuses once Drain or Close stopped it: dialling or installing a
// connection, or re-subscribing.
var errStopped = errors.New("bus: client is stopped")

// stop stops the client: recovery ends, and nothing dials, installs a connection or re-subscribes.
func (c *Client) stop() {
	c.closeOnce.Do(func() { close(c.stopCh) })
}

func (c *Client) stopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// Close stops any recovery goroutine and closes the underlying NATS connection.
func (c *Client) Close() {
	c.stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Conn != nil {
		c.Conn.Close()
	}
}

// Drain stops the client and drains it, letting the deliveries already in their handlers finish,
// and the connection is closed when Drain returns, at the latest once timeout passes, so a blocked
// drain cannot keep a process alive. It stops the client first, so neither the drain's close nor a
// recovery already under way reconnects or re-subscribes a process that is shutting down. It then
// drains the delivery subscriptions while the connection still accepts new ones: a handler
// finishing its delivery may subscribe (RequestCoreTo's receipt inbox), which a draining
// connection refuses. Only then does it drain the connection, whose Drain only starts the drain,
// and wait for it to close itself. A connection that is reconnecting is closed at once: nothing it
// sends reaches the server, so no subscription would drain and a reconnect would re-send them.
func (c *Client) Drain(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	c.stop()
	c.subscriptionsMu.Lock()
	var delivering []*nats.Subscription
	for _, subscription := range c.subscriptions {
		if subscription.active != nil && subscription.active.IsValid() {
			delivering = append(delivering, subscription.active)
		}
	}
	c.subscriptionsMu.Unlock()
	c.mu.Lock()
	conn := c.Conn
	c.mu.Unlock()
	defer conn.Close()
	if conn.IsReconnecting() {
		return fmt.Errorf("drain NATS: %w", nats.ErrConnectionReconnecting)
	}
	waitUntil := func(done func() bool) error {
		for !done() {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("drain NATS: %w", context.DeadlineExceeded)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	for _, subscription := range delivering {
		if err := subscription.Drain(); err != nil {
			return err
		}
	}
	for _, subscription := range delivering {
		if err := waitUntil(func() bool { return !subscription.IsValid() }); err != nil {
			return err
		}
	}
	if err := conn.Drain(); err != nil {
		return err
	}
	return waitUntil(conn.IsClosed)
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
		if c.subscriptionsHealthy() {
			slog.Info("envoy nats recovery: already healthy")
			return
		}
		slog.Info("envoy nats recovery attempt", slog.Int("attempt", attempt))
		failure := "envoy nats recovery reconnect failed"
		err := c.ensureConnWithContext(ctx)
		if err == nil {
			failure = "envoy nats recovery resubscribe failed"
			err = c.restoreSubscriptions()
		}
		if err == nil && !c.stopped() {
			c.mu.Lock()
			conn := c.Conn
			c.mu.Unlock()
			err = c.runReconnectHooks(conn)
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
			return err
		}
	}
	return nil
}

func (c *Client) ensureConn() error {
	return c.ensureConnWithContext(context.Background())
}

// ensureConnWithContext returns once the client has a usable connection. A connection nats.go is
// reconnecting is waited for, until it reconnects, ctx ends or the client stops: with the
// reconnect buffer off, a publish while reconnecting would otherwise fail at once, and a webhook
// that fails answers 503 to a sender that does not redeliver. A closed connection is replaced by
// dialling a new one, unless the client is stopped.
func (c *Client) ensureConnWithContext(ctx context.Context) error {
	for {
		c.mu.Lock()
		conn := c.Conn
		c.mu.Unlock()
		// A live connection serves even a stopped client: deliveries still in their handlers during
		// Drain publish their receipts and exceptions through it.
		if conn != nil && (conn.IsConnected() || conn.IsDraining()) {
			return nil
		}
		if c.stopped() {
			return errStopped
		}
		if conn == nil || conn.IsClosed() {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bus: waiting for NATS to reconnect: %w", ctx.Err())
		case <-c.stopCh:
			return errStopped
		case <-time.After(10 * time.Millisecond):
		}
	}

	nc, err := connectWithContext(ctx, "envoy", c.urls, c.credential, c.onReconnect, c.onClosed)
	if err != nil {
		return err
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		nc.Close()
		return err
	}

	c.mu.Lock()
	// Drain and Close read c.Conn under c.mu after stopping the client, so checking here, under the
	// same lock, means a connection dialled while they ran is either theirs to close or never
	// installed.
	if c.stopped() {
		c.mu.Unlock()
		nc.Close()
		return errStopped
	}
	c.Conn = nc
	c.js = js
	c.mu.Unlock()
	return nil
}

func usesCoreTransport(topic string) bool {
	return strings.HasPrefix(topic, contracts.RoleTopicPrefix) ||
		strings.HasPrefix(topic, "notifications.envoy.exceptions."+contracts.RoleTopicPrefix)
}

// ErrRefused is returned for an envelope that is refused the same way however often it is
// published, so a caller answers it as a refusal rather than a failure to retry. It is ErrTooLarge
// or ErrInvalidSubject.
var ErrRefused = errors.New("refused")

// refusal is a kind of ErrRefused. Its text names only the kind, since every line and answer that
// reports a refusal already says it is one (`github publish refused: too large to publish whole`).
type refusal string

func (r refusal) Error() string { return string(r) }

func (r refusal) Is(target error) bool { return target == ErrRefused }

// ErrTooLarge is the ErrRefused of an envelope too large to publish whole: a message past the NATS
// server's max payload, which nats.go refuses before sending it, a subject past maxSubjectBytes, a
// KV key whose subject would be, or a CI record or settlement past the bound its store keeps below
// those. Its error names the size and the bound.
var ErrTooLarge error = refusal("too large to publish whole")

// ErrInvalidSubject is the ErrRefused of a subject NATS does not accept: an empty one or one holding
// whitespace, which nats.go refuses, or one holding an empty token (`a..b`, or a leading or trailing
// dot), which no stream's subjects match, so a JetStream publish waits out its deadline for an
// answer that never comes and a core publish is dropped. Its error names the subject, or the KV key
// that would have made it.
var ErrInvalidSubject error = refusal("not a subject NATS accepts")

// maxSubjectBytes bounds a subject the bus publishes on, or a KV call builds from a key. The server
// closes a connection whose protocol line runs past its max control line (4 KiB by default) and
// nats.go does not check it, so a longer subject would close the connection every subscription and
// watcher of the client runs on, a core publish reporting success first. Besides the subject, the
// longest line nats.go sends here, `HPUB <subject> <reply> <header size> <total size>`, holds a
// 38-byte reply inbox (`_INBOX.<nuid>.<token>`), two sizes of at most seven digits (the 1 MiB max
// payload), its verb, spaces and line ending: 62 bytes, which 64 covers.
const maxSubjectBytes = 4<<10 - 64

// checkSubject refuses, before anything is sent, a subject the server would close the connection
// over (ErrTooLarge) and one NATS does not accept (ErrInvalidSubject).
func checkSubject(subject string) error {
	if len(subject) > maxSubjectBytes {
		return fmt.Errorf("%w: a subject of %d bytes, past %d", ErrTooLarge, len(subject), maxSubjectBytes)
	}
	if !validSubject(subject) {
		return fmt.Errorf("%w: %q", ErrInvalidSubject, subject)
	}
	return nil
}

// validSubject reports whether NATS accepts subject: it is not empty, holds no whitespace, and has
// no empty token.
func validSubject(subject string) bool {
	return subject != "" && subject[0] != '.' && subject[len(subject)-1] != '.' &&
		!strings.Contains(subject, "..") && !strings.ContainsAny(subject, " \t\r\n")
}

// refused names nats.go's refusal of a message past the server's max payload as the ErrTooLarge it
// is, naming its envelope of size bytes. checkSubject has already refused every subject nats.go
// would.
func (c *Client) refused(err error, size int) error {
	if errors.Is(err, nats.ErrMaxPayload) {
		return fmt.Errorf("%w: an envelope of %d bytes against the server's max payload of %d bytes",
			ErrTooLarge, size, c.Conn.MaxPayload())
	}
	return err
}

// Publish routes role lanes and their delivery-exception lanes through core
// NATS; every other notification retains JetStream durability. Its signature is
// an interface method in cistore, outbox and webhook, so a caller that wants
// JetStream's duplicate verdict calls PublishReportingDuplicate instead.
func (c *Client) Publish(item contracts.Envelope) error {
	_, err := c.PublishReportingDuplicate(item)
	return err
}

// PublishReportingDuplicate publishes as Publish does and reports whether
// JetStream recognised the message as one the stream already holds. That is only
// ever possible for an envelope whose dedupe key names the upstream event
// (contracts.DedupeKeyNamesTheUpstreamEvent), the only one published under a
// MsgId, and only within the stream's duplicate window; a core-transport topic
// has no acknowledgement at all and always reports false.
//
// True means "the stream already held this MsgId", not "this call published
// nothing new": the reconnect retry below republishes after a failure that may
// have landed, so the very publish that put the message on the subject can come
// back a duplicate.
func (c *Client) PublishReportingDuplicate(item contracts.Envelope) (bool, error) {
	if usesCoreTransport(item.Topic) {
		return false, c.PublishCore(item)
	}
	return c.publishJetStream(item)
}

// publishJetStream publishes item to the notification stream. The MsgId, where the envelope earns
// one, is the dedupe key and the topic together, because one upstream event can be published on
// several topics under one key - a GitHub comment that mentions the trigger is published on its
// mention topic beside its comment topic - and each of those must be retained.
func (c *Client) publishJetStream(item contracts.Envelope) (bool, error) {
	if err := checkSubject(item.Topic); err != nil {
		return false, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return false, err
	}
	ctx, cancel := c.publishAcknowledgementClock.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return false, err
	}
	options := []nats.PubOpt{nats.Context(ctx)}
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		options = append(options, nats.MsgId(item.DedupeKey+":"+item.Topic))
	}
	ack, err := c.js.Publish(item.Topic, data, options...)
	if err != nil && errors.Is(err, nats.ErrConnectionClosed) {
		if err := c.ensureConnWithContext(ctx); err != nil {
			return false, err
		}
		ack, err = c.js.Publish(item.Topic, data, options...)
	}
	if err != nil {
		return false, c.refused(err, len(data))
	}
	return ack.Duplicate, nil
}

// PublishCore publishes directly to the envelope's NATS subject without
// waiting for a JetStream acknowledgment.
func (c *Client) PublishCore(item contracts.Envelope) error {
	return c.PublishCoreTo(item.Topic, item)
}

// PublishCoreTo publishes item directly to subject and flushes, so a nil error means the server
// accepted it: a publish NATS denies (a permissions violation of a per-client grant) or a flush
// that fails returns an error, and so does any other error the server reports before the flush
// answers (confirmPublished). The subject can differ from item.Topic when an authoritative router
// forwards an envelope while retaining its original topic for the recipient.
func (c *Client) PublishCoreTo(subject string, item contracts.Envelope) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return err
	}
	conn := c.Conn
	before := observe(conn)
	if err := conn.Publish(subject, data); err != nil {
		return c.refused(err, len(data))
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("bus: publish window elapsed before the publish to %q was flushed", subject)
	}
	if err := conn.FlushTimeout(remaining); err != nil {
		return fmt.Errorf("bus: flush publish to %q: %w", subject, err)
	}
	return confirmPublished(conn, subject, before)
}

// ErrPublishDenied is returned, wrapping nats.ErrPermissionViolation and the server's own text,
// for a core publish the server refused under its connection's publish permissions.
var ErrPublishDenied = errors.New("NATS denied the publish")

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
// between the two observations. nats.go exposes no error counter to tell that from no change, and
// the only subscriptions on Dispatch's outbox connection are the reply inboxes of its own
// JetStream requests (its agent stream dials a separate connection), which are not busy enough to
// be a slow consumer; so it is accepted.
func confirmPublished(conn *nats.Conn, subject string, before connState) error {
	after := observe(conn)
	if !after.connected || after.reconnects != before.reconnects {
		return fmt.Errorf("bus: NATS disconnected during the publish to %q, so it is not known to have been accepted", subject)
	}
	if sameError(after.lastError, before.lastError) {
		return nil
	}
	if errors.Is(after.lastError, nats.ErrPermissionViolation) &&
		strings.Contains(after.lastError.Error(), fmt.Sprintf("Publish to %q", subject)) {
		return fmt.Errorf("bus: %w to %q: %w", ErrPublishDenied, subject, after.lastError)
	}
	return fmt.Errorf("bus: NATS reported an error during the publish to %q, so it is not known to have been accepted: %w",
		subject, after.lastError)
}

// sameError reports whether a and b are the same error value. nats.go records each permissions
// violation as a new error (processErr), so a denial never equals the error it replaced; a shared
// nats.go value recorded again (confirmPublished names the ones that matter) does. A value of a
// type that cannot be compared counts as different rather than panicking.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	kind := reflect.TypeOf(a)
	return kind == reflect.TypeOf(b) && kind.Comparable() && a == b
}

// ErrReceiptTimeout is returned by RequestCoreTo only when the publish and the
// flush both succeeded, the server reported nothing and the connection stayed
// up through them, and no empty receipt arrived before the deadline: the
// forward is known to have reached the server, and whoever holds the subject
// did not acknowledge it in time. A flush that fails or times out — a
// reconnecting or stalled connection still buffering the forward — is returned
// as the client's own error, never this one, because that forward is not known
// to have left this process; so is a forward the server denied
// (ErrPublishDenied), or one during which the server reported another error or
// the connection dropped, since either may have hidden its denial.
var ErrReceiptTimeout = errors.New("bus: no receipt inside the request window")

// RequestCoreTo delivers item directly to subject and waits for an empty
// receiver receipt. Agent subjects are captured by the notification stream,
// whose non-empty JetStream publish acknowledgement is not a receiver receipt.
// The flush is bounded by the same window as the receipt wait, so the call
// never outlives timeout by the client's default 10 s flush. A forward the
// server denied returns at once; one confirmPublished cannot confirm for
// another reason still waits, since a receipt proves it was delivered, and
// returns that reason instead of ErrReceiptTimeout if none arrives.
func (c *Client) RequestCoreTo(subject string, item contracts.Envelope, timeout time.Duration) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	conn := c.Conn
	inbox := nats.NewInbox()
	receipt, err := conn.SubscribeSync(inbox)
	if err != nil {
		return err
	}
	defer receipt.Unsubscribe()
	before := observe(conn)
	if err := conn.PublishRequest(subject, inbox, data); err != nil {
		return c.refused(err, len(data))
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("bus: request window of %s elapsed before the forward was flushed", timeout)
	}
	if err := conn.FlushTimeout(remaining); err != nil {
		return fmt.Errorf("bus: flush forward: %w", err)
	}
	unconfirmed := confirmPublished(conn, subject, before)
	if errors.Is(unconfirmed, ErrPublishDenied) {
		return unconfirmed
	}
	timedOut := ErrReceiptTimeout
	if unconfirmed != nil {
		timedOut = unconfirmed
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return timedOut
		}
		response, err := receipt.NextMsg(remaining)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				return timedOut
			}
			return err
		}
		if len(response.Data) == 0 && response.Header == nil {
			return nil
		}
	}
}
