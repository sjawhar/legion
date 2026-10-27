package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// publishAcknowledgementTimeout bounds one publish: the wait for a connection and the server's
// answer to it, a JetStream acknowledgement or a core publish's flush.
const publishAcknowledgementTimeout = 5 * time.Second

// ErrUnavailable is wrapped by the outcome of a window's send that NATS did not take because of
// the connection rather than the envelope: the client had no connection within the publish window,
// the window's connection had been lost or had reconnected since its first send (so the window
// sent nothing), or the connection was lost, or NATS gave no answer, after the envelope was sent,
// so whether it landed is unknown. Sending it again is safe on a JetStream subject, where its
// MsgId drops a repeat, and on a role lane, whose receivers drop a repeat by its dedupe key.
var ErrUnavailable = errors.New("NATS is unreachable")

// Sender sends envelopes without waiting for NATS's answers (Client.Window).
type Sender interface {
	// PublishAsync sends item and returns a channel that receives the outcome exactly once: nil
	// once NATS accepted item, or why it did not.
	PublishAsync(item contracts.Envelope) <-chan error
}

// Window returns a Sender whose sends all go out on one connection, in the order they are made,
// or not at all. Its first send waits, within publishAcknowledgementTimeout, for the client to have
// a connection, and pins the window to it. Every later send goes out on that connection only while
// it is connected and has not reconnected since; otherwise it answers ErrUnavailable at once,
// without sending. A NATS server takes one connection's messages in the order they arrive, so the
// stream stores, and a core subscriber receives, a window's envelopes in send order, and what of
// them reached NATS is always a prefix of what was sent: once one is lost to the connection, none
// sent after it can land after it on a connection that came back.
//
// Each send goes on the transport Publish would use (usesCoreTransport), under the same MsgId. An
// outcome decided before anything was sent is in the channel when PublishAsync returns: a refusal
// (ErrRefused), or ErrUnavailable. Otherwise the answer comes within publishAcknowledgementTimeout:
// a JetStream acknowledgement (the JetStream context's PublishAsyncTimeout), or a core publish's own
// flush, confirmed as PublishCoreTo confirms one (confirmFlushed). A core publish that its own
// flush cannot confirm because another publish on the connection changed its last error in the
// meantime is published again alone on the window's connection, which answers for it: once every
// core flush of the window has returned, and one such republish at a time, so that another of the
// window's denials cannot land during it and leave it unconfirmed too. A republish costs a round
// trip of its own, so a batch in which one denial left many sends unconfirmed takes that many
// round trips more.
func (c *Client) Window() Sender {
	w := &window{client: c}
	w.settled = sync.NewCond(&w.mu)
	return w
}

type window struct {
	client     *Client
	conn       *nats.Conn
	js         nats.JetStreamContext
	reconnects uint64

	// mu guards flushing, the window's core sends whose flush has not returned, and is held
	// across a republish. settled is signalled when flushing reaches zero.
	mu       sync.Mutex
	flushing int
	settled  *sync.Cond
}

func (w *window) PublishAsync(item contracts.Envelope) <-chan error {
	outcome := make(chan error, 1)
	answer, err := w.send(item)
	if err != nil {
		outcome <- err
		return outcome
	}
	go func() { outcome <- answer() }()
	return outcome
}

// send writes item to the window's connection and returns what waits for the server's answer.
func (w *window) send(item contracts.Envelope) (func() error, error) {
	if err := checkSubject(item.Topic); err != nil {
		return nil, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	if err := w.pin(); err != nil {
		return nil, err
	}
	if usesCoreTransport(item.Topic) {
		return w.sendCore(item, data)
	}
	return w.sendJetStream(item, data)
}

// pin pins the window to the client's connection at its first send, and at every later one
// answers ErrUnavailable unless that connection is still connected and has not reconnected.
func (w *window) pin() error {
	if w.conn == nil {
		ctx, cancel := context.WithTimeout(context.Background(), publishAcknowledgementTimeout)
		defer cancel()
		if err := w.client.ensureConnWithContext(ctx); err != nil {
			return fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
		}
		w.client.mu.Lock()
		w.conn, w.js = w.client.Conn, w.client.js
		w.client.mu.Unlock()
		w.reconnects = w.conn.Stats().Reconnects
	}
	if !w.current() {
		return fmt.Errorf("bus: %w: the window's connection was lost or reconnected since its first send", ErrUnavailable)
	}
	return nil
}

// current reports whether the window's connection is connected and has not reconnected since the
// window pinned it.
func (w *window) current() bool {
	return w.conn.IsConnected() && w.conn.Stats().Reconnects == w.reconnects
}

// sendCore publishes data, item's encoding, to item's topic over core NATS and returns what
// confirms that the server accepted it. The send counts itself in flushing before it reads the
// clock, so time it spent waiting behind a republish for the window's lock is not taken from its
// own flush.
func (w *window) sendCore(item contracts.Envelope, data []byte) (func() error, error) {
	w.mu.Lock()
	w.flushing++
	w.mu.Unlock()
	deadline := time.Now().Add(publishAcknowledgementTimeout)
	before := observe(w.conn)
	if err := w.conn.PublishMsg(&nats.Msg{Subject: item.Topic, Data: data}); err != nil {
		w.flushed()
		return nil, w.unsent(err, len(data))
	}
	return func() error {
		err := confirmFlushed(w.conn, item.Topic, before, deadline)
		w.flushed()
		if err == nil || errors.Is(err, ErrPublishDenied) || !errors.Is(err, errPublishUnconfirmed) {
			return w.answered(err)
		}
		return w.republish(item, data)
	}, nil
}

// republish publishes data, item's encoding, again alone on the window's connection, once every
// core flush of the window has returned, holding the window's lock so that one republish runs at
// a time and no send of the window lands during it.
func (w *window) republish(item contracts.Envelope, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.flushing > 0 {
		w.settled.Wait()
	}
	if !w.current() {
		return fmt.Errorf("bus: %w: the window's connection was lost or reconnected before the publish to %q was sent again", ErrUnavailable, item.Topic)
	}
	msg := &nats.Msg{Subject: item.Topic, Data: data}
	ctx, cancel := context.WithTimeout(context.Background(), publishAcknowledgementTimeout)
	defer cancel()
	return w.answered(w.client.publishConfirmed(ctx, w.conn, msg, w.conn.PublishMsg))
}

// answered names a core publish's confirmation: accepted, denied, still unconfirmed, or
// ErrUnavailable when the connection failed or the flush ran out of time, so whether it landed
// is unknown.
func (w *window) answered(err error) error {
	if err == nil || errors.Is(err, ErrPublishDenied) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRefused) {
		return err
	}
	if !w.current() || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrConnectionReconnecting) ||
		errors.Is(err, nats.ErrTimeout) || errors.Is(err, errFlushWindowElapsed) {
		return fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
	}
	return err
}

// flushed records that one of the window's core sends has its flush's answer.
func (w *window) flushed() {
	w.mu.Lock()
	w.flushing--
	if w.flushing == 0 {
		w.settled.Broadcast()
	}
	w.mu.Unlock()
}

// sendJetStream publishes data, item's encoding, to the stream and returns what waits for its
// acknowledgement.
func (w *window) sendJetStream(item contracts.Envelope, data []byte) (func() error, error) {
	var options []nats.PubOpt
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		options = append(options, nats.MsgId(item.DedupeKey+":"+item.Topic))
	}
	future, err := w.js.PublishMsgAsync(&nats.Msg{Subject: item.Topic, Data: data}, options...)
	if err != nil {
		return nil, w.unsent(err, len(data))
	}
	return func() error {
		select {
		case <-future.Ok():
			return nil
		case err := <-future.Err():
			if !w.current() || errors.Is(err, nats.ErrDisconnected) || errors.Is(err, nats.ErrAsyncPublishTimeout) ||
				errors.Is(err, nats.ErrNoResponders) {
				return fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
			}
			return err
		}
	}, nil
}

// unsent names nats.go's refusal to send a message of size bytes: ErrUnavailable when the
// connection is not connected (the client keeps no reconnect buffer, so nats.go sends nothing then)
// or more acknowledgements are pending than nats.go takes, and otherwise as refused names it.
func (w *window) unsent(err error, size int) error {
	if !w.current() || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrConnectionDraining) ||
		errors.Is(err, nats.ErrConnectionReconnecting) || errors.Is(err, nats.ErrReconnectBufExceeded) ||
		errors.Is(err, nats.ErrTooManyStalledMsgs) {
		return fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
	}
	return w.client.refused(err, size)
}
