package bus

import (
	"cmp"
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
// sent nothing), or the connection was lost or reconnected after the envelope was sent and before
// NATS answered it, so whether it landed is unknown. A core publish whose own flush failed or ran
// out of time is one too (answered). NATS giving no answer on a connection that stayed current is
// ErrNotAcknowledged instead. Sending it again is safe on a JetStream subject, where its MsgId
// drops a repeat, and on a role lane, whose receivers drop a repeat by its dedupe key.
var ErrUnavailable = errors.New("NATS is unreachable")

// ErrNotAcknowledged is wrapped by the outcome of a window's send that NATS gave no
// acknowledgement for on a connection that stayed current: no JetStream acknowledgement within
// publishAcknowledgementTimeout, no responders for the stream's subject, or no answer to a Request
// within its timeout. The envelope may or may not be stored, so the caller backs it off and counts
// an attempt, rather than resending it on the next connection as it does an ErrUnavailable.
var ErrNotAcknowledged = errors.New("NATS did not acknowledge the publish")

// Sender sends envelopes without waiting for NATS's answers (Client.Window).
type Sender interface {
	// PublishAsync sends item and returns a channel that receives the outcome exactly once: nil
	// once NATS accepted item, or why it did not.
	PublishAsync(item contracts.Envelope) <-chan error
	// Request sends item over core NATS to subject, which can differ from item.Topic, with a reply
	// inbox, and returns a channel that receives the outcome exactly once: nil once its receiver
	// answered it, or why it did not.
	Request(subject string, item contracts.Envelope, opts RequestOptions) <-chan error
	// Probe flushes the sender's connection within timeout: nil once the server answered on a
	// connection that is still current, and otherwise an error wrapping ErrUnavailable.
	Probe(timeout time.Duration) error
}

// RequestOptions shapes one Request.
type RequestOptions struct {
	// MsgID, when not empty, is sent as the request's Nats-Msg-Id header (ForwardMsgID), so a
	// stream that captures the subject stores one copy of the envelope however often it is sent.
	MsgID string
	// AcceptStreamAck counts the stream's acknowledgement of its stored copy as the answer, a
	// duplicate's included: a receiver delivered from the stored copy never answers the request
	// itself, so for it that acknowledgement is the receipt.
	AcceptStreamAck bool
	// Timeout bounds the wait for the answer once the request is sent; zero is
	// publishAcknowledgementTimeout.
	Timeout time.Duration
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
// flush, confirmed as PublishCoreTo confirms one (confirmFlushed). A JetStream publish NATS does
// not acknowledge, or answers with no responders, is ErrNotAcknowledged while the window's
// connection stays current and ErrUnavailable once it does not. A core publish that its own flush
// cannot confirm because another publish on the connection changed its last error in the meantime
// is published again alone on the window's connection, which answers for it: once every core flush
// of the window has returned, and one such republish at a time, so that another of the window's
// denials cannot land during it and leave it unconfirmed too. A republish costs a round trip of its
// own, so a batch in which one denial left many sends unconfirmed takes that many round trips more.
//
// A Request is a send of the window too, pinned and refused by the same rules, and a Probe pins
// the window when nothing has yet (window.Request, window.Probe). The window's methods are safe to
// call from several goroutines at once.
func (c *Client) Window() Sender {
	w := &window{client: c}
	w.settled = sync.NewCond(&w.mu)
	return w
}

type window struct {
	client *Client

	// pinning guards conn, js and reconnects while the first send pins them (pin); they never
	// change after it.
	pinning    sync.Mutex
	conn       *nats.Conn
	js         nats.JetStreamContext
	reconnects uint64

	// mu guards flushing, the window's core sends and requests whose flush has not returned, and
	// is held across a republish. settled is signalled when flushing reaches zero.
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
	if err := w.pin(publishAcknowledgementTimeout); err != nil {
		return nil, err
	}
	if usesCoreTransport(item.Topic) {
		return w.sendCore(item, data)
	}
	return w.sendJetStream(item, data)
}

// pin pins the window to the client's connection at its first send, waiting up to wait for the
// client to have one, and at every later one answers ErrUnavailable unless that connection is
// still connected and has not reconnected.
func (w *window) pin(wait time.Duration) error {
	w.pinning.Lock()
	defer w.pinning.Unlock()
	if w.conn == nil {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
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
	return w.answered(w.client.publishConfirmed(w.conn, msg, time.Now().Add(publishAcknowledgementTimeout), w.conn.PublishMsg))
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

// flushed records that one of the window's core sends or requests has its flush's answer.
func (w *window) flushed() {
	w.mu.Lock()
	w.flushing--
	if w.flushing == 0 {
		w.settled.Broadcast()
	}
	w.mu.Unlock()
}

// sendJetStream publishes data, item's encoding, to the stream and returns what waits for its
// acknowledgement. A failure is ErrUnavailable once the window's connection is no longer current
// or nats.go reports the disconnect, and ErrNotAcknowledged when the acknowledgement ran out of
// time or the server answered with no responders on a connection that stayed current.
func (w *window) sendJetStream(item contracts.Envelope, data []byte) (func() error, error) {
	var options []nats.PubOpt
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		options = append(options, nats.MsgId(ForwardMsgID(item.DedupeKey, item.Topic)))
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
			if !w.current() || errors.Is(err, nats.ErrDisconnected) {
				return fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
			}
			if errors.Is(err, nats.ErrAsyncPublishTimeout) || errors.Is(err, nats.ErrNoResponders) {
				return fmt.Errorf("bus: %w: %w", ErrNotAcknowledged, err)
			}
			return err
		}
	}, nil
}

// requestReplies is how many replies to one Request wait to be read: it expects at most an empty
// receipt and the stream's acknowledgement, or the server's no-responders answer, and nats.go
// drops a reply that finds the channel full.
const requestReplies = 4

// Request publishes item's encoding to subject over core NATS on the window's connection, with a
// reply inbox it subscribes first and, when opts.MsgID is not empty, a Nats-Msg-Id header of it. It
// is pinned and refused as a send of the window is: a subject NATS would not accept answers
// ErrRefused, and a window whose connection was lost or reconnected answers ErrUnavailable, both in
// the channel when Request returns, before anything is sent. Otherwise the channel receives:
//
//   - nil on the first empty receipt, a reply with no data and no header field, as RequestCoreTo
//     reads one; or, when opts.AcceptStreamAck, on the stream's acknowledgement of its stored copy
//     without an error, a duplicate's included (answers). Any other reply is passed over and the
//     wait goes on: a stream's acknowledgement without AcceptStreamAck or with an error, and the
//     server's no-responders answer;
//   - ErrPublishDenied when the request's flush shows the server denied the publish
//     (confirmFlushed). A denial another publish on the connection hid in the meantime leaves the
//     request unconfirmed instead, and it ends as one with no answer;
//   - ErrUnavailable once the window's connection is lost or reconnects before an answer;
//   - when no answer came within opts.Timeout on a connection that stayed current, an error
//     wrapping ErrNotAcknowledged and ErrReceiptTimeout, and the flush's own failure when the flush
//     failed or could not confirm the publish.
//
// The request counts in the window's flushing until its flush returns, as a core send does, so a
// republish neither runs while its flush is out nor has it published during one. Its inbox is
// unsubscribed once the outcome is decided.
func (w *window) Request(subject string, item contracts.Envelope, opts RequestOptions) <-chan error {
	outcome := make(chan error, 1)
	answer, err := w.request(subject, item, opts)
	if err != nil {
		outcome <- err
		return outcome
	}
	go func() { outcome <- answer() }()
	return outcome
}

// request sends item as Request describes and returns what waits for its answer.
func (w *window) request(subject string, item contracts.Envelope, opts RequestOptions) (func() error, error) {
	if err := checkSubject(subject); err != nil {
		return nil, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	// Taken before the pin reads the connection, so every connection event from here on wakes the
	// wait for the answer.
	changed := w.client.ConnectionChanged()
	if err := w.pin(publishAcknowledgementTimeout); err != nil {
		return nil, err
	}
	inbox := nats.NewInbox()
	replies := make(chan *nats.Msg, requestReplies)
	receipt, err := w.conn.ChanSubscribe(inbox, replies)
	if err != nil {
		return nil, w.unsent(err, len(data))
	}
	msg := &nats.Msg{Subject: subject, Reply: inbox, Data: data}
	if opts.MsgID != "" {
		msg.Header = nats.Header{nats.MsgIdHdr: []string{opts.MsgID}}
	}
	w.mu.Lock()
	w.flushing++
	w.mu.Unlock()
	deadline := time.Now().Add(cmp.Or(opts.Timeout, publishAcknowledgementTimeout))
	before := observe(w.conn)
	if err := w.conn.PublishMsg(msg); err != nil {
		w.flushed()
		_ = receipt.Unsubscribe()
		return nil, w.unsent(err, len(data))
	}
	return func() error {
		defer receipt.Unsubscribe()
		unconfirmed := confirmFlushed(w.conn, subject, before, deadline)
		w.flushed()
		if errors.Is(unconfirmed, ErrPublishDenied) {
			return unconfirmed
		}
		// answered reads the replies already in, so an answer that arrived with the connection's
		// loss or the deadline still counts.
		answered := func() bool {
			for len(replies) > 0 {
				if answers(<-replies, opts.AcceptStreamAck) {
					return true
				}
			}
			return false
		}
		lost := func() error {
			return fmt.Errorf("bus: %w: the window's connection was lost or reconnected before the request to %q was answered", ErrUnavailable, subject)
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		for {
			select {
			case reply := <-replies:
				if answers(reply, opts.AcceptStreamAck) {
					return nil
				}
			case <-changed:
				if answered() {
					return nil
				}
				if !w.current() {
					return lost()
				}
				changed = w.client.ConnectionChanged()
			case <-timer.C:
				if answered() {
					return nil
				}
				if !w.current() {
					return lost()
				}
				if unconfirmed != nil {
					return fmt.Errorf("bus: request to %q: %w: %w: %w", subject, ErrNotAcknowledged, ErrReceiptTimeout, unconfirmed)
				}
				return fmt.Errorf("bus: request to %q: %w: %w", subject, ErrNotAcknowledged, ErrReceiptTimeout)
			}
		}
	}, nil
}

// answers reports whether reply answers a Request: an empty receipt, a reply with no data and no
// header field (a reply to a request that carries headers can come with a header block that holds
// none), or, when acceptStreamAck, the stream's acknowledgement of its stored copy without an
// error, a duplicate's included.
func answers(reply *nats.Msg, acceptStreamAck bool) bool {
	if len(reply.Data) == 0 && len(reply.Header) == 0 {
		return true
	}
	if !acceptStreamAck {
		return false
	}
	var ack struct {
		nats.PubAck
		Error *nats.APIError `json:"error"`
	}
	return json.Unmarshal(reply.Data, &ack) == nil && ack.Error == nil && ack.Stream != "" && ack.Sequence > 0
}

// Probe pins the window to the client's connection when nothing has yet, waiting up to timeout
// for one, and flushes that connection within timeout. A flush that fails or runs out of time, and
// a connection lost or reconnected since the window pinned it, answer an error wrapping
// ErrUnavailable. After a window whose sends NATS did not acknowledge, it tells a connection that
// is gone or silent from one whose server still answers.
func (w *window) Probe(timeout time.Duration) error {
	if err := w.pin(timeout); err != nil {
		return err
	}
	if err := w.conn.FlushTimeout(timeout); err != nil {
		return fmt.Errorf("bus: %w: the flush probe failed: %w", ErrUnavailable, err)
	}
	if !w.current() {
		return fmt.Errorf("bus: %w: the window's connection was lost or reconnected since its first send", ErrUnavailable)
	}
	return nil
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
