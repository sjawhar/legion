package bus

import (
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

type Client struct {
	Conn *nats.Conn
}

func Connect(urls []string) (Client, error) {
	opts := nats.Options{
		Servers:       urls,
		Name:          "envoy",
		ReconnectWait: 2 * nats.DefaultReconnectWait,
	}
	var nc *nats.Conn
	var err error
	for range 10 {
		nc, err = opts.Connect()
		if err == nil {
			return Client{Conn: nc}, nil
		}
		time.Sleep(time.Second)
	}
	return Client{}, err
}

func (c Client) Publish(item contracts.Envelope) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if err := c.Conn.Publish(item.Topic, data); err != nil {
		return err
	}
	return c.Conn.Flush()
}
