package bus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

const Stream = "ENVOY_NOTIFICATIONS"

var streamSubjects = []string{
	"notifications.agent.>",
	"notifications.github.>",
	"notifications.slack.>",
	"notifications.ghostwispr.>",
	"notifications.whatsapp.>",
	"notifications.envoy.exceptions.notifications.agent.>",
}

// StreamSubjects reports the durable notification subjects, excluding role lanes.
func StreamSubjects() []string {
	return slices.Clone(streamSubjects)
}

var streamCfg = &nats.StreamConfig{
	Name:      Stream,
	Subjects:  streamSubjects,
	Retention: nats.LimitsPolicy,
	MaxAge:    72 * time.Hour,
	Storage:   nats.FileStorage,
	Replicas:  1,
}

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

// WithReplicas overrides the stream replica count (default 1).
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
	publishAcknowledgementClock AcknowledgementClock
	mu                          sync.Mutex

	subscriptionsMu sync.Mutex
	subscriptions   [subscriptionCount]recoverableSubscription

	// recovery state
	recovering int32
	stopCh     chan struct{}
	closeOnce  sync.Once
}

func options(name string, urls []string, reconnectCB func(*nats.Conn), closedCB func()) nats.Options {
	return nats.Options{
		Servers:       urls,
		Name:          name,
		NoRandomize:   true,
		Timeout:       5 * time.Second,
		MaxReconnect:  -1,
		ReconnectWait: 2 * nats.DefaultReconnectWait,
		DisconnectedErrCB: func(_ *nats.Conn, err error) {
			if err != nil {
				slog.Info("envoy nats disconnected", slog.String("error", err.Error()))
				return
			}
			slog.Info("envoy nats disconnected")
		},
		ReconnectedCB: func(nc *nats.Conn) {
			slog.Info("envoy nats reconnected", slog.String("url", nc.ConnectedUrl()))
			if reconnectCB != nil {
				reconnectCB(nc)
			}
		},
		ClosedCB: func(_ *nats.Conn) {
			slog.Info("envoy nats connection closed")
			if closedCB != nil {
				closedCB()
			}
		},
		AsyncErrorCB: func(_ *nats.Conn, sub *nats.Subscription, err error) {
			if sub != nil {
				slog.Error("envoy nats async error", slog.String("subject", sub.Subject), slog.String("error", err.Error()))
				return
			}
			slog.Error("envoy nats async error", slog.String("error", err.Error()))
		},
	}
}

func connect(name string, urls []string, reconnectCB func(*nats.Conn), closedCB func()) (*nats.Conn, error) {
	return connectWithContext(context.Background(), name, urls, reconnectCB, closedCB)
}

func connectWithContext(ctx context.Context, name string, urls []string, reconnectCB func(*nats.Conn), closedCB func()) (*nats.Conn, error) {
	var lastErr error
	for range 10 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next := options(name, urls, reconnectCB, closedCB)
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, ctx.Err()
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

// Dial opens a tuned core NATS connection using envoy's standard options
// (5s connect timeout, infinite reconnect, 2× default backoff, retry-loop
// for the initial 10 attempts). Callers that only need core pub/sub —
// no JetStream stream creation, no durable consumer — should use this.
// The NATS Go client auto-resubscribes core subscriptions on reconnect,
// so no callbacks are needed for plain subscribers.
//
// For JetStream-backed durable consumers, use Connect instead.
func Dial(name string, urls []string) (*nats.Conn, error) {
	return connect(name, urls, nil, nil)
}

func Connect(urls []string, options ...ConnectOption) (*Client, error) {
	opts := connectOpts{replicas: 1, publishAcknowledgementClock: wallClock{}}
	for _, o := range options {
		o(&opts)
	}
	c := &Client{
		urls:                        urls,
		publishAcknowledgementClock: opts.publishAcknowledgementClock,
		stopCh:                      make(chan struct{}),
	}
	nc, err := connect("envoy", urls, c.onReconnect, c.onClosed)
	if err != nil {
		return nil, err
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		nc.Close()
		return nil, err
	}
	cfg := *streamCfg
	cfg.Replicas = opts.replicas
	if err := ensureStreamWithConfig(js, &cfg); err != nil {
		nc.Close()
		return nil, err
	}
	c.Conn = nc
	c.js = js
	return c, nil
}

func streamSubjectMatches(pattern, subject string) bool {
	patternTokens := strings.Split(pattern, ".")
	subjectTokens := strings.Split(subject, ".")
	for index, patternToken := range patternTokens {
		if patternToken == ">" {
			return index == len(patternTokens)-1
		}
		if index >= len(subjectTokens) {
			return false
		}
		if patternToken != "*" && patternToken != subjectTokens[index] {
			return false
		}
	}
	return len(patternTokens) == len(subjectTokens)
}

func streamCapturesRoleLanes(subjects []string) bool {
	for _, subject := range subjects {
		if streamSubjectMatches(subject, "notifications.role.legion") ||
			streamSubjectMatches(subject, "notifications.envoy.exceptions.notifications.role.legion") {
			return true
		}
	}
	return false
}

func purgeLegacyRoleMessages(js nats.JetStreamContext) error {
	for _, subject := range []string{
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
	} {
		if err := js.PurgeStream(Stream, &nats.StreamPurgeRequest{Subject: subject}); err != nil {
			return err
		}
	}
	return nil
}

func migrateLegacyConsumerFilters(js nats.JetStreamContext, subjects []string) error {
	for name := range js.ConsumerNames(Stream) {
		info, err := js.ConsumerInfo(Stream, name)
		if err != nil {
			return err
		}
		if info.Config.FilterSubject != "notifications.>" {
			continue
		}
		config := info.Config
		config.FilterSubject = ""
		config.FilterSubjects = slices.Clone(subjects)
		if _, err := js.UpdateConsumer(Stream, &config); err != nil {
			return err
		}
	}
	return nil
}

func migrateRoleLanesOffStream(js nats.JetStreamContext, oldConfig, newConfig *nats.StreamConfig) error {
	if !streamCapturesRoleLanes(oldConfig.Subjects) || streamCapturesRoleLanes(newConfig.Subjects) {
		return nil
	}
	if err := migrateLegacyConsumerFilters(js, newConfig.Subjects); err != nil {
		return err
	}
	return purgeLegacyRoleMessages(js)
}

func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig) error {
	info, err := js.StreamInfo(Stream)
	if err == nil {
		if info.Config.MaxAge == cfg.MaxAge && slices.Equal(info.Config.Subjects, cfg.Subjects) {
			return nil
		}
		migratingRoleLanes := streamCapturesRoleLanes(info.Config.Subjects) && !streamCapturesRoleLanes(cfg.Subjects)
		if err := migrateRoleLanesOffStream(js, &info.Config, cfg); err != nil {
			return err
		}
		// Updating here reconciles the deployed stream on the next listener start.
		if _, err = js.UpdateStream(cfg); err != nil {
			return err
		}
		if migratingRoleLanes {
			return purgeLegacyRoleMessages(js)
		}
		return nil
	}
	if !errors.Is(err, nats.ErrStreamNotFound) {
		return err
	}
	_, err = js.AddStream(cfg)
	return err
}

func (c *Client) JS() nats.JetStreamContext {
	return c.js
}

// onClosed is wired as the ClosedCB callback. It launches recovery in a
// background goroutine so the NATS library callback returns immediately.
func (c *Client) onClosed() {
	go c.recover()
}

func (c *Client) onReconnect(nc *nats.Conn) {
	c.mu.Lock()
	c.Conn = nc
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		c.mu.Unlock()
		slog.Error("envoy nats resubscribe failed (jetstream)", slog.String("error", err.Error()))
		go c.recover()
		return
	}
	c.js = js
	c.mu.Unlock()

	if err := c.restoreSubscriptions(); err != nil {
		slog.Error("envoy nats resubscribe failed", slog.String("error", err.Error()))
		go c.recover()
	}
}

// Subscribe creates the listener's recoverable JetStream subscription.
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
// overlapping listeners.
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
	*subscription = next
	if err := restoreSubscription(subscription, conn, js); err != nil {
		subscription.active = nil
		return nil, err
	}
	return subscription.active, nil
}

func restoreSubscription(subscription *recoverableSubscription, conn *nats.Conn, js nats.JetStreamContext) error {
	if subscription.active != nil {
		_ = subscription.active.Unsubscribe()
		subscription.active = nil
	}
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

// Close stops any recovery goroutine and closes the underlying NATS connection.
func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.stopCh) })
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Conn != nil {
		c.Conn.Close()
	}
}

// recover attempts to restore the NATS connection and every recoverable
// subscription after a CLOSED state or failed re-subscribe.
func (c *Client) recover() {
	if !atomic.CompareAndSwapInt32(&c.recovering, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&c.recovering, 0)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for attempt := 1; ; attempt++ {
		select {
		case <-c.stopCh:
			slog.Info("envoy nats recovery cancelled")
			return
		default:
		}
		if c.subscriptionsHealthy() {
			slog.Info("envoy nats recovery: already healthy")
			return
		}
		slog.Info("envoy nats recovery attempt", slog.Int("attempt", attempt))
		if err := c.ensureConn(); err == nil {
			err = c.restoreSubscriptions()
			if err == nil {
				slog.Info("envoy nats recovery successful", slog.Int("attempt", attempt))
				return
			}
			slog.Error("envoy nats recovery resubscribe failed", slog.Int("attempt", attempt), slog.String("error", err.Error()))
		} else {
			slog.Error("envoy nats recovery reconnect failed", slog.Int("attempt", attempt), slog.String("error", err.Error()))
		}
		select {
		case <-c.stopCh:
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
	for index := range c.subscriptions {
		subscription := &c.subscriptions[index]
		if subscription.handler == nil {
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

func (c *Client) ensureConnWithContext(ctx context.Context) error {
	c.mu.Lock()
	if c.Conn != nil && c.Conn.Status() != nats.CLOSED {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	nc, err := connectWithContext(ctx, "envoy", c.urls, c.onReconnect, c.onClosed)
	if err != nil {
		return err
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		nc.Close()
		return err
	}

	c.mu.Lock()
	c.Conn = nc
	c.js = js
	c.mu.Unlock()
	return nil
}

func usesCoreTransport(topic string) bool {
	return strings.HasPrefix(topic, contracts.RoleTopicPrefix) ||
		strings.HasPrefix(topic, "notifications.envoy.exceptions."+contracts.RoleTopicPrefix)
}

// Publish routes role lanes and their delivery-exception lanes through core
// NATS; every other notification retains JetStream durability.
func (c *Client) Publish(item contracts.Envelope) error {
	if usesCoreTransport(item.Topic) {
		return c.PublishCore(item)
	}
	return c.publishJetStream(item)
}

func (c *Client) publishJetStream(item contracts.Envelope) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	ctx, cancel := c.publishAcknowledgementClock.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return err
	}
	_, err = c.js.Publish(item.Topic, data, nats.Context(ctx))
	if err != nil && errors.Is(err, nats.ErrConnectionClosed) {
		if err := c.ensureConnWithContext(ctx); err != nil {
			return err
		}
		_, err = c.js.Publish(item.Topic, data, nats.Context(ctx))
	}
	return err
}

// PublishCore publishes directly to the envelope's NATS subject without
// waiting for a JetStream acknowledgment.
func (c *Client) PublishCore(item contracts.Envelope) error {
	return c.PublishCoreTo(item.Topic, item)
}

// PublishCoreTo publishes item directly to subject. The subject can differ
// from item.Topic when an authoritative router forwards an envelope while
// retaining its original topic for the recipient.
func (c *Client) PublishCoreTo(subject string, item contracts.Envelope) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return err
	}
	return c.Conn.Publish(subject, data)
}

// RequestCoreTo delivers item directly to subject and waits for an empty
// receiver receipt. Agent subjects are captured by the notification stream,
// whose non-empty JetStream publish acknowledgement is not a receiver receipt.
func (c *Client) RequestCoreTo(subject string, item contracts.Envelope, timeout time.Duration) error {
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
	inbox := nats.NewInbox()
	receipt, err := c.Conn.SubscribeSync(inbox)
	if err != nil {
		return err
	}
	defer receipt.Unsubscribe()
	if err := c.Conn.PublishRequest(subject, inbox, data); err != nil {
		return err
	}
	if err := c.Conn.Flush(); err != nil {
		return err
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nats.ErrTimeout
		}
		response, err := receipt.NextMsg(remaining)
		if err != nil {
			return err
		}
		if len(response.Data) == 0 && response.Header == nil {
			return nil
		}
	}
}
