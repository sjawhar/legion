package bus

import (
	"encoding/json"
	"errors"
	"log"
	"sync"
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
	subMu       sync.Mutex
	subSubject  string
	subHandler  nats.MsgHandler
	subOpts     []nats.SubOpt
	subActive   *nats.Subscription
}

func options(urls []string, reconnectCB func(*nats.Conn)) nats.Options {
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

func connect(urls []string, reconnectCB func(*nats.Conn)) (*nats.Conn, error) {
	var nc *nats.Conn
	var err error
	for range 10 {
		next := options(urls, reconnectCB)
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
	c := &Client{urls: urls}
	nc, err := connect(urls, c.onReconnect)
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

func (c *Client) onReconnect(nc *nats.Conn) {
	c.mu.Lock()
	c.Conn = nc
	js, err := nc.JetStream()
	if err != nil {
		c.mu.Unlock()
		log.Printf("envoy nats resubscribe failed (jetstream): %v", err)
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

func (c *Client) ensureConn() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Conn != nil && c.Conn.Status() != nats.CLOSED {
		return nil
	}
	nc, err := connect(c.urls, c.onReconnect)
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
