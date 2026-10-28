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

// publishAcknowledgementTimeout bounds each send of a publish window (Client.WindowUntil): its wait
// for a connection, a JetStream send's acknowledgement (the stream context's PublishAsyncTimeout),
// and a Request's answer when its options set no timeout of their own. Publish and PublishCoreTo
// take publishBound instead.
const publishAcknowledgementTimeout = 5 * time.Second

// ErrUnavailable is wrapped by the outcome of a window's send that NATS did not take because of
// the connection rather than the envelope: the client had no connection within the publish window,
// the window's connection had been lost or had reconnected since its first send (so the window
// sent nothing), or the connection was lost or reconnected after the envelope was sent and before
// NATS answered it, so whether it landed is unknown. NATS giving no answer on a connection that
// stayed current is ErrNotAcknowledged instead. Sending it again is safe: a JetStream publish and
// a Request under a MsgId are stored once by the stream, and the receiver of a Request drops a
// repeat by its dedupe key.
var ErrUnavailable = errors.New("NATS is unreachable")

// ErrNotAcknowledged is wrapped by the outcome of a window's send that NATS gave no
// acknowledgement for on a connection that stayed current: no JetStream acknowledgement within
// publishAcknowledgementTimeout, no responders for the stream's subject, or no answer to a Request
// within its timeout. The envelope may or may not be stored, so the caller backs it off and counts
// an attempt, rather than resending it on the next connection as it does an ErrUnavailable.
var ErrNotAcknowledged = errors.New("NATS did not acknowledge the publish")

// ErrNotCaptured is wrapped by the outcome of a window's PublishAsync of a topic the stream does
// not capture (StreamCaptures), answered before anything is sent. The window publishes only to the
// stream, whose acknowledgement answers for each envelope; a core forward goes through Request,
// whose receiver answers it. It is neither ErrRefused nor ErrUnavailable: the caller backs the
// envelope off as a failure of its own.
var ErrNotCaptured = errors.New("the stream does not capture the topic")

// ErrWindowDeadline is the outcome of a send of a window with a deadline (Client.WindowUntil) that
// would write at or past it, answered before anything is sent: the deadline had passed when the
// send began, while its first send waited for a connection, or by the moment it would write. It is
// neither ErrUnavailable nor ErrRefused: the window's owner may no longer send at all, whatever the
// connection does.
var ErrWindowDeadline = errors.New("the window's deadline has passed")

// Sender sends envelopes without waiting for NATS's answers (Client.WindowUntil).
type Sender interface {
	// PublishAsync sends item to the stream and returns a channel that receives the outcome
	// exactly once: nil once the stream acknowledged item, or why it did not. A topic the stream
	// does not capture answers ErrNotCaptured.
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

// WindowUntil returns a Sender whose sends all go out on one connection, or not at all. Its first
// send waits, within publishAcknowledgementTimeout, for the client to have a connection, and pins
// the window to it. Every later send checks that connection first and answers ErrUnavailable at
// once, without sending, once it has been lost or has reconnected since, so a window does not go
// on sending across a connection it saw fail.
//
// A window keeps no order of its own between its sends. Its answers come back in whatever order
// the server and the stream give them, and a JetStream publish NATS did not acknowledge may still
// be stored after sends made later. Order per (owner, destination) is the caller's: the outbox
// makes the next send of one owner to one destination only once the answer to the one before it
// is in (internal/dispatch/outbox, planChains and sendChains), so no two sends of one (owner,
// destination) are ever in flight on a window, and lets everything else proceed side by side.
//
// The pin's check comes before the write, and a reconnect can land between the two. That breaks no
// order: the envelope is then written on the reconnected connection, and its answer - the stream's
// acknowledgement, or a Request's receipt - comes on that connection and answers for it truly, as
// for any send made after the reconnect. An earlier send lost with the old connection is answered
// ErrUnavailable and sent again by its caller; since it is of another (owner, destination), the
// envelope that landed ahead of it is ahead of nothing it must follow.
//
// PublishAsync sends only to the stream, under the MsgId Publish would use: a topic the stream does
// not capture answers ErrNotCaptured, and a core forward goes through Request. An outcome decided
// before anything was sent is in the channel when PublishAsync returns: a refusal (ErrRefused),
// ErrNotCaptured, or ErrUnavailable. Otherwise the answer is the stream's acknowledgement, within
// publishAcknowledgementTimeout (the JetStream context's PublishAsyncTimeout). A publish NATS does
// not acknowledge, or answers with no responders, is ErrNotAcknowledged while the window's
// connection stays current and ErrUnavailable once it does not.
//
// A Request is a send of the window too, pinned and refused by the same rules, and a Probe pins
// the window when nothing has yet (window.Request, window.Probe). The window's methods are safe to
// call from several goroutines at once.
//
// The window's sends never write at or past deadline, read again at each check so a deadline that
// moves on (a hold's renewal) is the one in force. A send checks it before anything else, its first
// send waits for a connection no later than it, and every send checks it again once pinned, the
// last act before the write; a send that finds it passed at any of these answers ErrWindowDeadline,
// in the channel when the send returns, and sends nothing. A nil deadline is none. A Probe sends no
// envelope and does not check it.
func (c *Client) WindowUntil(deadline func() time.Time) Sender {
	return &window{client: c, deadline: deadline}
}

type window struct {
	client *Client
	// deadline, when not nil, is the moment the window's sends stop writing (WindowUntil).
	deadline func() time.Time

	// pinning guards conn, js and reconnects while the first send pins them (pin); they never
	// change after it.
	pinning    sync.Mutex
	conn       *nats.Conn
	js         nats.JetStreamContext
	reconnects uint64
}

func (w *window) PublishAsync(item contracts.Envelope) <-chan error {
	return outcomeOf(w.send(item))
}

// outcomeOf is the channel a send's outcome comes in: err at once when the send failed before
// anything waits, otherwise answer's result once it returns.
func outcomeOf(answer func() error, err error) <-chan error {
	outcome := make(chan error, 1)
	if err != nil {
		outcome <- err
		return outcome
	}
	go func() { outcome <- answer() }()
	return outcome
}

// send writes item to the window's connection and returns what waits for the stream's answer.
func (w *window) send(item contracts.Envelope) (func() error, error) {
	if err := w.expired(); err != nil {
		return nil, err
	}
	if err := checkSubject(item.Topic); err != nil {
		return nil, err
	}
	if !StreamCaptures(item.Topic) {
		return nil, fmt.Errorf("bus: %w: %q", ErrNotCaptured, item.Topic)
	}
	data, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	if err := w.pinToSend(); err != nil {
		return nil, err
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

// expired answers ErrWindowDeadline once the window's deadline has passed, and nil before it or
// when the window has none.
func (w *window) expired() error {
	if w.deadline != nil && !time.Now().Before(w.deadline()) {
		return fmt.Errorf("bus: %w", ErrWindowDeadline)
	}
	return nil
}

// pinToSend pins the window for a send (pin), within publishAcknowledgementTimeout and no later
// than the window's deadline, and answers ErrWindowDeadline when the deadline has passed before
// the pin, during its wait for a connection, or by its end, which is the last check before the
// send writes.
func (w *window) pinToSend() error {
	if w.deadline == nil {
		return w.pin(publishAcknowledgementTimeout)
	}
	remaining := time.Until(w.deadline())
	if remaining <= 0 {
		return fmt.Errorf("bus: %w", ErrWindowDeadline)
	}
	pinned := w.pin(min(publishAcknowledgementTimeout, remaining))
	if err := w.expired(); err != nil {
		return err
	}
	return pinned
}

// current reports whether the window's connection is connected and has not reconnected since the
// window pinned it.
func (w *window) current() bool {
	return w.conn.IsConnected() && w.conn.Stats().Reconnects == w.reconnects
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
// Its inbox is unsubscribed once the outcome is decided.
func (w *window) Request(subject string, item contracts.Envelope, opts RequestOptions) <-chan error {
	return outcomeOf(w.request(subject, item, opts))
}

// request sends item as Request describes and returns what waits for its answer.
func (w *window) request(subject string, item contracts.Envelope, opts RequestOptions) (func() error, error) {
	if err := w.expired(); err != nil {
		return nil, err
	}
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
	if err := w.pinToSend(); err != nil {
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
	deadline := time.Now().Add(cmp.Or(opts.Timeout, publishAcknowledgementTimeout))
	before := observe(w.conn)
	if err := w.conn.PublishMsg(msg); err != nil {
		_ = receipt.Unsubscribe()
		return nil, w.unsent(err, len(data))
	}
	return func() error {
		defer receipt.Unsubscribe()
		unconfirmed := confirmFlushed(w.conn, subject, before, deadline)
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
