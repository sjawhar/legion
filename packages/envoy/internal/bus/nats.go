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
	Replicas:  3,
}

type Client struct {
	Conn *nats.Conn
	js   nats.JetStreamContext
	urls []string
	mu   sync.Mutex
}

func options(urls []string) nats.Options {
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

func connect(urls []string) (*nats.Conn, error) {
	var nc *nats.Conn
	var err error
	for range 10 {
		next := options(urls)
		nc, err = next.Connect()
		if err == nil {
			return nc, nil
		}
		time.Sleep(time.Second)
	}
	return nil, err
}

func Connect(urls []string) (Client, error) {
	nc, err := connect(urls)
	if err != nil {
		return Client{}, err
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return Client{}, err
	}
	if err := ensureStream(js); err != nil {
		nc.Close()
		return Client{}, err
	}
	return Client{Conn: nc, js: js, urls: urls}, nil
}

func ensureStream(js nats.JetStreamContext) error {
	_, err := js.StreamInfo(Stream)
	if err == nil {
		_, err = js.UpdateStream(streamCfg)
		return err
	}
	if !errors.Is(err, nats.ErrStreamNotFound) {
		return err
	}
	_, err = js.AddStream(streamCfg)
	return err
}

func (c *Client) JS() nats.JetStreamContext {
	return c.js
}

func (c *Client) ensureConn() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Conn != nil && c.Conn.Status() != nats.CLOSED {
		return nil
	}
	nc, err := connect(c.urls)
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
