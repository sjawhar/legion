package bus

import (
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

const Stream = "ENVOY_NOTIFICATIONS"

var streamCfg = &nats.StreamConfig{
	Name:      Stream,
	Subjects:  []string{"notifications.>"},
	Retention: nats.LimitsPolicy,
	MaxAge:    time.Hour,
	Storage:   nats.FileStorage,
	Replicas:  1,
}

// ConnectOption configures the bus client.
type ConnectOption func(*connectOpts)

type connectOpts struct {
	replicas int
}

// WithReplicas overrides the stream replica count (default 1).
func WithReplicas(n int) ConnectOption {
	return func(o *connectOpts) { o.replicas = n }
}

type Client struct {
	Conn *nats.Conn
	js   nats.JetStreamContext
	urls []string
	mu   sync.Mutex

	// subscriber state for auto-resubscribe
	subMu      sync.Mutex
	subSubject string
	subHandler nats.MsgHandler
	subOpts    []nats.SubOpt
	subActive  *nats.Subscription

	// recovery state
	recovering int32
	stopCh     chan struct{}
	closeOnce  sync.Once
}

func options(urls []string, reconnectCB func(*nats.Conn), closedCB func()) nats.Options {
	return nats.Options{
		Servers:       urls,
		Name:          "envoy",
		MaxReconnect:  -1,
		ReconnectWait: 2 * nats.DefaultReconnectWait,
		DisconnectedErrCB: func(_ *nats.Conn, err error) {
			if err != nil {
				log.Printf("envoy nats disconnected: %v", err)
				return
			}
			log.Printf("envoy nats disconnected")
		},
		ReconnectedCB: func(nc *nats.Conn) {
			log.Printf("envoy nats reconnected: %s", nc.ConnectedUrl())
			if reconnectCB != nil {
				reconnectCB(nc)
			}
		},
		ClosedCB: func(_ *nats.Conn) {
			log.Printf("envoy nats connection closed")
			if closedCB != nil {
				closedCB()
			}
		},
		AsyncErrorCB: func(_ *nats.Conn, sub *nats.Subscription, err error) {
			if sub != nil {
				log.Printf("envoy nats async error subject=%s: %v", sub.Subject, err)
				return
			}
			log.Printf("envoy nats async error: %v", err)
		},
	}
}

func connect(urls []string, reconnectCB func(*nats.Conn), closedCB func()) (*nats.Conn, error) {
	var nc *nats.Conn
	var err error
	for range 10 {
		next := options(urls, reconnectCB, closedCB)
		nc, err = next.Connect()
		if err == nil {
			return nc, nil
		}
		time.Sleep(time.Second)
	}
	return nil, err
}

func Connect(urls []string, options ...ConnectOption) (*Client, error) {
	opts := connectOpts{replicas: 1}
	for _, o := range options {
		o(&opts)
	}
	c := &Client{urls: urls, stopCh: make(chan struct{})}
	nc, err := connect(urls, c.onReconnect, c.onClosed)
	if err != nil {
		return nil, err
	}
	js, err := nc.JetStream()
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

func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig) error {
	_, err := js.StreamInfo(Stream)
	if err == nil {
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
	js, err := nc.JetStream()
	if err != nil {
		c.mu.Unlock()
		log.Printf("envoy nats resubscribe failed (jetstream): %v", err)
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
	log.Printf("envoy nats resubscribing to %s", c.subSubject)
	sub, err := c.js.Subscribe(c.subSubject, c.subHandler, c.subOpts...)
	if err != nil {
		log.Printf("envoy nats resubscribe failed: %v", err)
		go c.recover()
		return
	}
	c.subActive = sub
	log.Printf("envoy nats resubscribed to %s", c.subSubject)
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
			log.Printf("envoy nats recovery cancelled")
			return
		default:
		}

		// Check if already recovered (e.g., by onReconnect succeeding).
		c.mu.Lock()
		connOK := c.Conn != nil && c.Conn.Status() != nats.CLOSED
		c.mu.Unlock()
		c.subMu.Lock()
		subOK := c.subActive != nil && c.subActive.IsValid()
		needsSub := c.subSubject != "" && c.subHandler != nil
		c.subMu.Unlock()
		if connOK && (!needsSub || subOK) {
			log.Printf("envoy nats recovery: already healthy")
			return
		}

		log.Printf("envoy nats recovery attempt %d", attempt)

		// Step 1: Ensure connection.
		if err := c.ensureConn(); err != nil {
			log.Printf("envoy nats recovery reconnect failed (attempt %d): %v", attempt, err)
			select {
			case <-c.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Step 2: Re-subscribe if needed.
		c.subMu.Lock()
		if c.subSubject == "" || c.subHandler == nil {
			c.subMu.Unlock()
			log.Printf("envoy nats recovery: connection restored, no subscription to restore")
			return
		}

		// Unsubscribe old to prevent duplicate message delivery.
		if c.subActive != nil {
			_ = c.subActive.Unsubscribe()
			c.subActive = nil
		}

		log.Printf("envoy nats recovery resubscribing to %s (attempt %d)", c.subSubject, attempt)
		sub, err := c.js.Subscribe(c.subSubject, c.subHandler, c.subOpts...)
		if err != nil {
			c.subMu.Unlock()
			log.Printf("envoy nats recovery resubscribe failed (attempt %d): %v", attempt, err)
			select {
			case <-c.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		c.subActive = sub
		c.subMu.Unlock()
		log.Printf("envoy nats recovery successful (attempt %d)", attempt)
		return
	}
}

func (c *Client) ensureConn() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Conn != nil && c.Conn.Status() != nats.CLOSED {
		return nil
	}
	nc, err := connect(c.urls, c.onReconnect, c.onClosed)
	if err != nil {
		return err
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return err
	}
	c.Conn = nc
	c.js = js
	return nil
}

func (c *Client) Publish(item contracts.Envelope) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if err := c.ensureConn(); err != nil {
		return err
	}
	_, err = c.js.Publish(item.Topic, data)
	if err != nil && errors.Is(err, nats.ErrConnectionClosed) {
		if err := c.ensureConn(); err != nil {
			return err
		}
		_, err = c.js.Publish(item.Topic, data)
	}
	return err
}
