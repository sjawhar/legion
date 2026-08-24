package bus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

const Stream = "ENVOY_NOTIFICATIONS"

// coreRoleQueueGroup elects one listener to arbitrate every live role event.
const coreRoleQueueGroup = "envoy-role-delivery"

var streamCfg = &nats.StreamConfig{
	Name: Stream,
	Subjects: []string{
		"notifications.agent.>",
		"notifications.github.>",
		"notifications.slack.>",
		"notifications.ghostwispr.>",
		"notifications.whatsapp.>",
		"notifications.envoy.exceptions.notifications.agent.>",
	},
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

type Client struct {
	Conn                        *nats.Conn
	js                          nats.JetStreamContext
	urls                        []string
	publishAcknowledgementClock AcknowledgementClock
	mu                          sync.Mutex

	// subscriber state for auto-resubscribe
	subMu      sync.Mutex
	subSubject string
	subHandler nats.MsgHandler
	subOpts    []nats.SubOpt
	subActive  *nats.Subscription

	// core subscriber state for role lanes that must not be durably replayed
	coreSubMu      sync.Mutex
	coreSubSubject string
	coreSubHandler nats.MsgHandler
	coreSubActive  *nats.Subscription

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
	var nc *nats.Conn
	var err error
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
		nc, err = next.Connect()
		if err == nil {
			return nc, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, err
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

func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig) error {
	info, err := js.StreamInfo(Stream)
	if err == nil {
		if info.Config.MaxAge == cfg.MaxAge && slices.Equal(info.Config.Subjects, cfg.Subjects) {
			return nil
		}
		if err := purgeLegacyRoleMessages(js); err != nil {
			return err
		}
		// Updating here migrates the deployed stream on the next listener start.
		_, err = js.UpdateStream(cfg)
		return err
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

	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.subSubject == "" || c.subHandler == nil {
		return
	}
	slog.Info("envoy nats resubscribing", slog.String("subject", c.subSubject))
	sub, err := c.js.Subscribe(c.subSubject, c.subHandler, c.subOpts...)
	if err != nil {
		slog.Error("envoy nats resubscribe failed", slog.String("error", err.Error()))
		go c.recover()
		return
	}
	c.subActive = sub
	slog.Info("envoy nats resubscribed", slog.String("subject", c.subSubject))
}

// Subscribe creates a JetStream subscription that auto-resubscribes on reconnect.
// Only one subscription per client is supported (the listener's main consumer).
func (c *Client) Subscribe(subject string, handler nats.MsgHandler, opts ...nats.SubOpt) (*nats.Subscription, error) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	sub, err := c.js.Subscribe(subject, handler, opts...)
	if err != nil {
		return nil, err
	}
	c.subSubject = subject
	c.subHandler = handler
	c.subOpts = opts
	c.subActive = sub
	return sub, nil
}

// SubscribeCore creates a core NATS subscription that is restored if recovery
// replaces the underlying connection. Only one core subscription per client is
// supported (the listener's role lane).
func (c *Client) SubscribeCore(subject string, handler nats.MsgHandler) (*nats.Subscription, error) {
	if err := c.ensureConn(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	conn := c.Conn
	c.mu.Unlock()
	c.coreSubMu.Lock()
	defer c.coreSubMu.Unlock()
	sub, err := conn.QueueSubscribe(subject, coreRoleQueueGroup, handler)
	if err != nil {
		return nil, err
	}
	c.coreSubSubject = subject
	c.coreSubHandler = handler
	c.coreSubActive = sub
	return sub, nil
}

// SubOK reports whether the client has an active subscription on a live connection.
func (c *Client) SubOK() bool {
	c.mu.Lock()
	connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
	c.mu.Unlock()

	c.subMu.Lock()
	subOK := c.subActive != nil && c.subActive.IsValid()
	c.subMu.Unlock()

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

// recover attempts to restore the NATS connection and subscription after a
// CLOSED state or failed re-subscribe. It is serialized via an atomic flag so
// concurrent callers (ClosedCB, onReconnect failure) do not race. The method
// retries with exponential backoff (1s → 2s → 4s → … → 30s cap) until the
// connection and subscription are restored or the client is closed.
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
		if err := c.ensureConn(); err != nil {
			slog.Error("envoy nats recovery reconnect failed", slog.Int("attempt", attempt), slog.String("error", err.Error()))
			select {
			case <-c.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		if err := c.restoreJetStreamSubscription(); err != nil {
			slog.Error("envoy nats recovery JetStream resubscribe failed", slog.Int("attempt", attempt), slog.String("error", err.Error()))
			select {
			case <-c.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		if err := c.restoreCoreSubscription(); err != nil {
			slog.Error("envoy nats recovery core resubscribe failed", slog.Int("attempt", attempt), slog.String("error", err.Error()))
			select {
			case <-c.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		slog.Info("envoy nats recovery successful", slog.Int("attempt", attempt))
		return
	}
}

func (c *Client) subscriptionsHealthy() bool {
	c.mu.Lock()
	connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
	c.mu.Unlock()
	c.subMu.Lock()
	subOK := c.subActive != nil && c.subActive.IsValid()
	needsSub := c.subSubject != "" && c.subHandler != nil
	c.subMu.Unlock()
	c.coreSubMu.Lock()
	coreSubOK := c.coreSubActive != nil && c.coreSubActive.IsValid()
	needsCoreSub := c.coreSubSubject != "" && c.coreSubHandler != nil
	c.coreSubMu.Unlock()
	return connOK && (!needsSub || subOK) && (!needsCoreSub || coreSubOK)
}

func (c *Client) restoreJetStreamSubscription() error {
	c.mu.Lock()
	js := c.js
	c.mu.Unlock()
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.subSubject == "" || c.subHandler == nil {
		return nil
	}
	if c.subActive != nil {
		_ = c.subActive.Unsubscribe()
		c.subActive = nil
	}
	slog.Info("envoy nats recovery resubscribing", slog.String("subject", c.subSubject))
	sub, err := js.Subscribe(c.subSubject, c.subHandler, c.subOpts...)
	if err != nil {
		return err
	}
	c.subActive = sub
	return nil
}

func (c *Client) restoreCoreSubscription() error {
	c.mu.Lock()
	conn := c.Conn
	c.mu.Unlock()
	c.coreSubMu.Lock()
	defer c.coreSubMu.Unlock()
	if c.coreSubSubject == "" || c.coreSubHandler == nil {
		return nil
	}
	if c.coreSubActive != nil {
		_ = c.coreSubActive.Unsubscribe()
		c.coreSubActive = nil
	}
	slog.Info("envoy nats recovery core resubscribing", slog.String("subject", c.coreSubSubject))
	sub, err := conn.QueueSubscribe(c.coreSubSubject, coreRoleQueueGroup, c.coreSubHandler)
	if err != nil {
		return err
	}
	c.coreSubActive = sub
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

func (c *Client) Publish(item contracts.Envelope) error {
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
