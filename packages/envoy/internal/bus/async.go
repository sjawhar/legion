package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// publishAcknowledgementTimeout bounds one publish: the wait for a connection and the server's
// answer to it, a JetStream acknowledgement or a core publish's flush.
const publishAcknowledgementTimeout = 5 * time.Second

// ErrUnavailable is wrapped by the outcome of a PublishAsync that sent nothing because the client
// had no connection to NATS: none came back within the publish window, or the one it had was lost
// as it sent. The server never saw the envelope, and an envelope sent after it meets the same
// connection.
var ErrUnavailable = errors.New("NATS is unreachable")

// PublishAsync sends item on the transport Publish would (usesCoreTransport), under the same MsgId,
// and returns once item is written to the connection, without waiting for the server's answer. The
// returned channel receives the outcome exactly once: nil once the server accepted item, or why it
// did not, as Publish would return it. An outcome decided before anything was sent is in the
// channel when PublishAsync returns: a refusal (ErrRefused), or ErrUnavailable.
//
// Envelopes one goroutine sends one after another are written to the connection in that order, and
// a NATS server takes one connection's messages in the order they arrive, so the stream stores them
// and a core subscriber receives them in send order; an envelope whose send fails is the only gap.
// The server's answer is waited for within publishAcknowledgementTimeout: the JetStream context's
// PublishAsyncTimeout for an acknowledgement, and for a core publish its own flush, confirmed as
// PublishCoreTo confirms one (confirmFlushed). A core publish another goroutine's denied publish
// overlaps is not confirmed and comes back unconfirmed, as confirmPublished explains.
func (c *Client) PublishAsync(item contracts.Envelope) <-chan error {
	outcome := make(chan error, 1)
	answer, err := c.send(item)
	if err != nil {
		outcome <- err
		return outcome
	}
	go func() { outcome <- answer() }()
	return outcome
}

// send writes item to the connection and returns what waits for the server's answer to it.
func (c *Client) send(item contracts.Envelope) (func() error, error) {
	if err := checkSubject(item.Topic); err != nil {
		return nil, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishAcknowledgementTimeout)
	defer cancel()
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
	}
	if usesCoreTransport(item.Topic) {
		deadline, _ := ctx.Deadline()
		return c.sendCore(item.Topic, data, deadline)
	}
	return c.sendJetStream(ctx, item, data)
}

// sendCore publishes data to subject over core NATS and returns what confirms, by deadline, that
// the server accepted it.
func (c *Client) sendCore(subject string, data []byte, deadline time.Time) (func() error, error) {
	c.mu.Lock()
	conn := c.Conn
	c.mu.Unlock()
	before := observe(conn)
	if err := conn.PublishMsg(&nats.Msg{Subject: subject, Data: data}); err != nil {
		return nil, c.unsent(err, len(data))
	}
	return func() error { return confirmFlushed(conn, subject, before, deadline) }, nil
}

// sendJetStream publishes data, item's encoding, to the stream and returns what waits for its
// acknowledgement. A connection that closed between the check and the send is replaced and the
// send made once more on its replacement, as publishJetStream does.
func (c *Client) sendJetStream(ctx context.Context, item contracts.Envelope, data []byte) (func() error, error) {
	var options []nats.PubOpt
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		options = append(options, nats.MsgId(item.DedupeKey+":"+item.Topic))
	}
	future, err := c.JS().PublishMsgAsync(&nats.Msg{Subject: item.Topic, Data: data}, options...)
	if errors.Is(err, nats.ErrConnectionClosed) {
		if err := c.ensureConnWithContext(ctx); err != nil {
			return nil, fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
		}
		future, err = c.JS().PublishMsgAsync(&nats.Msg{Subject: item.Topic, Data: data}, options...)
	}
	if err != nil {
		return nil, c.unsent(err, len(data))
	}
	return func() error {
		select {
		case <-future.Ok():
			return nil
		case err := <-future.Err():
			return err
		}
	}, nil
}

// unsent names nats.go's refusal to send a message of size bytes: ErrUnavailable when the
// connection is closed, draining or reconnecting (the client keeps no reconnect buffer, so nats.go
// sends nothing then), and otherwise as refused names it.
func (c *Client) unsent(err error, size int) error {
	if errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrConnectionDraining) ||
		errors.Is(err, nats.ErrConnectionReconnecting) || errors.Is(err, nats.ErrReconnectBufExceeded) {
		return fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
	}
	return c.refused(err, size)
}
