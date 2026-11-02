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
// and a Request's answer when its options set no timeout of their own. Publish takes publishBound
// instead.
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
// publishAcknowledgementTimeout, no responders for the stream's subject, or a Request the stream
// did not acknowledge within its timeout. The envelope may or may not be stored, so the caller
// backs it off and counts an attempt, rather than resending it on the next connection as it does
// an ErrUnavailable.
//
// A Request that ran out of time went unanswered because the stream did not acknowledge the copy
// it was asked to store, which is the whole of a request's outcome: nobody else answers one,
// since no session subscribes to its agent subject (INFRA-771). Such a request is known to have
// left this process when its flush confirmed the publish; a flush that failed or timed out - a
// reconnecting or stalled connection still buffering the request - adds ErrPublishUnconfirmed
// with its own reason, since that request is not known to have left. One the server denied is
// ErrPublishDenied instead, as is one during which the server reported another error or the
// connection dropped, since either may have hidden its denial.
var ErrNotAcknowledged = errors.New("NATS did not acknowledge the publish")

// ErrNotCaptured is wrapped by the outcome of a window's PublishAsync of a topic the stream does
// not capture (StreamCaptures), answered before anything is sent. The window publishes only to the
// stream, whose acknowledgement answers for each envelope; a core forward goes through Request,
// whose receiver answers it. It is neither ErrRefused nor ErrUnavailable: the caller backs the
// envelope off as a failure of its own.
var ErrNotCaptured = errors.New("the stream does not capture the topic")

// ErrWindowDeadline is the outcome of a send of a window with a deadline (Client.WindowUntil) that
// would write at or past it: the deadline had passed when the send began, while its first send
// waited for a connection, or by the moment it would write. The stream holds nothing of such a
// send. It is answered before anything is sent, but for the send without its id that an
// acknowledgement of a duplicate with no sequence (ErrNotStored) calls for, of a publish
// (sendJetStream) or of a Request (request): that first send stored nothing either, so a deadline
// which fences the send without the id still leaves the stream holding nothing of the envelope. It
// is neither ErrUnavailable nor ErrRefused: the window's owner may no longer send at all, whatever
// the connection does.
var ErrWindowDeadline = errors.New("the window's deadline has passed")

// Sender sends envelopes without waiting for NATS's answers (Client.WindowUntil).
type Sender interface {
	// PublishAsync sends item to the stream and returns a channel that receives the outcome
	// exactly once: nil once the stream acknowledged item, or why it did not. A topic the stream
	// does not capture answers ErrNotCaptured.
	PublishAsync(item contracts.Envelope) <-chan error
	// Request sends item over core NATS to subject, which can differ from item.Topic, with a reply
	// inbox, and returns a channel that receives the outcome exactly once: nil once the stream
	// that captures subject acknowledged the copy it stored, or why it did not. No receiver
	// answers a request (window.Request).
	Request(subject string, item contracts.Envelope, opts RequestOptions) <-chan error
	// Probe flushes the sender's connection within timeout: nil once the server answered on a
	// connection that is still current, and otherwise an error wrapping ErrUnavailable.
	Probe(timeout time.Duration) error
}

// RequestOptions shapes one Request.
type RequestOptions struct {
	// MsgID is sent as the request's Nats-Msg-Id header (ForwardMsgID), so the stream that
	// captures the subject stores one copy of the envelope however often it is sent. It is
	// required: a request is a forward of one envelope to one recipient, and an unnamed one
	// would be stored again by every attempt, so an empty MsgID is refused ErrNoForwardMsgID
	// before anything is sent, as StartRequestStoredTo refuses one.
	MsgID string
	// Timeout bounds the wait for the stream's acknowledgement once the request is sent; zero is
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
// order: the envelope is then written on the reconnected connection, and its answer - the
// stream's acknowledgement of the copy it stored, of a publish or of a Request alike - comes on
// that connection and answers for it truly, as for any send made after the reconnect. An earlier
// send lost with the old connection is answered ErrUnavailable and sent again by its caller;
// since it is of another (owner, destination), the envelope that landed ahead of it is ahead of
// nothing it must follow.
//
// PublishAsync sends only to the stream, under the MsgId Publish would use: a topic the stream does
// not capture answers ErrNotCaptured, and a core forward goes through Request. An outcome decided
// before anything was sent is in the channel when PublishAsync returns: a refusal (ErrRefused),
// ErrNotCaptured, or ErrUnavailable. Otherwise the answer is the stream's acknowledgement, within
// publishAcknowledgementTimeout (the JetStream context's PublishAsyncTimeout). An acknowledgement
// of a duplicate with no sequence (ErrNotStored) has the envelope sent once more without its
// MsgId, as Publish does, and that send's acknowledgement answers for it (sendJetStream). A
// publish NATS does not acknowledge, or answers with no responders, is ErrNotAcknowledged while
// the window's connection stays current and ErrUnavailable once it does not.
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
// acknowledgement. It notes the time it put the message on the wire, which is what its
// acknowledgement reads a denial of this send alone by (acknowledged). A failure is ErrUnavailable
// once the window's connection is no longer current or nats.go reports the disconnect, and
// ErrNotAcknowledged when the acknowledgement ran out of time or the server answered with no
// responders on a connection that stayed current. An acknowledgement of a duplicate with no
// sequence (ErrNotStored) has the envelope sent once more without its MsgId, as publishJetStream
// does, while the window's connection is still current and its deadline has not passed: the answer
// is that send's, under its own send time, and a failure of it still wraps ErrNotStored. The
// resend goes out after the window's later sends, so the stream stores it after them, and the
// stream does not deduplicate a copy sent without its MsgId: it stores that copy as a message of
// its own.
func (w *window) sendJetStream(item contracts.Envelope, data []byte) (func() error, error) {
	var options []nats.PubOpt
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		options = append(options, nats.MsgId(ForwardMsgID(item.DedupeKey, item.Topic)))
	}
	sentAt := time.Now()
	future, err := w.js.PublishMsgAsync(&nats.Msg{Subject: item.Topic, Data: data}, options...)
	if err != nil {
		return nil, w.unsent(err, len(data))
	}
	return func() error {
		err := w.acknowledged(future, item.Topic, sentAt)
		if !errors.Is(err, ErrNotStored) {
			return err
		}
		if expired := w.expired(); expired != nil {
			return fmt.Errorf("%w, and the window's deadline passed before the publish to %q was sent without its id: %w", err, item.Topic, expired)
		}
		if !w.current() {
			return fmt.Errorf("bus: %w: %w, and the window's connection was lost or reconnected before the publish to %q was sent without its id", ErrUnavailable, err, item.Topic)
		}
		resentAt := time.Now()
		future, sendErr := w.js.PublishMsgAsync(&nats.Msg{Subject: item.Topic, Data: data})
		if sendErr == nil {
			sendErr = w.acknowledged(future, item.Topic, resentAt)
		} else {
			sendErr = w.unsent(sendErr, len(data))
		}
		if sendErr != nil {
			return fmt.Errorf("%w, and its publish without its id failed: %w", err, sendErr)
		}
		return nil
	}, nil
}

// requestReplies is how many replies to one Request wait to be read: it expects at most two
// stream acknowledgements (the first send's, and the send without its id that a duplicate with
// no sequence calls for), the server's no-responders answer, and whatever a subscriber of the
// subject might send it, and nats.go drops a reply that finds the channel full.
const requestReplies = 4

// Request publishes item's encoding to subject over core NATS on the window's connection, with a
// reply inbox it subscribes first and a Nats-Msg-Id header of opts.MsgID, which every request
// carries: an empty one is refused ErrNoForwardMsgID before anything is sent. It is pinned and
// refused as a send of the window is: a subject NATS would not accept answers
// ErrRefused, and a window whose connection was lost or reconnected answers ErrUnavailable, both in
// the channel when Request returns, before anything is sent. Otherwise the channel receives:
//
//   - nil on the stream's acknowledgement of a copy it holds, at a sequence and without an error,
//     a duplicate's included (answers): the request is stored on a subject the stream captures,
//     and that acknowledgement is its whole outcome, since no receiver answers a request. A reply
//     that is neither an acknowledgement nor a refusal is passed over and the wait goes on: the
//     server's no-responders answer, and anything a subscriber of the subject sends;
//   - the stream's refusal of the copy, the moment it comes back: an acknowledgement that carries
//     an error settles the request as that error, a full stream's as ErrStreamFull
//     (streamRefusal). It never wraps ErrNotAcknowledged, which says the stream gave no answer at
//     all, and the request does not wait out opts.Timeout for an answer that is already in, as a
//     window's PublishAsync (acknowledged) and Client.StartRequestStoredTo's Wait do not;
//   - the outcome of the send the stream's acknowledgement of a duplicate with no sequence
//     (ErrNotStored) calls for: the request goes out once more, to the same inbox and
//     without its Nats-Msg-Id, which the record of that id at sequence 0 does not match, and the
//     acknowledgement of the copy the stream then stores answers. A request makes that send at
//     most once, under the checks sendJetStream's resend makes: a window whose deadline has
//     passed answers an error wrapping ErrNotStored and ErrWindowDeadline, one whose connection
//     is no longer current ErrNotStored and ErrUnavailable, and a send nats.go refuses
//     ErrNotStored and its refusal, the stream holding nothing of the request in each case;
//   - ErrPublishDenied when the server denied the publish: the request's flush shows it
//     (confirmFlushed), or, when a violation of another subject hid it there, the violation the
//     connection recorded under this subject does (deniedPublish). The flush runs beside the wait
//     for the answer, not ahead of it, so an acknowledgement that lands first answers the
//     request while the flush is still out; the record is read on the wait's own goroutine, when
//     a flush that could not confirm the publish comes back and once more before the timeout is
//     answered;
//   - ErrUnavailable once the window's connection is lost or reconnects before an answer, whether
//     the loss came before the flush, during it, or after it;
//   - when the stream did not acknowledge the copy within opts.Timeout on a connection that
//     stayed current, an error wrapping ErrNotAcknowledged, and ErrPublishUnconfirmed with the
//     flush's own reason when the flush failed or could not confirm the publish.
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
	if opts.MsgID == "" {
		return nil, fmt.Errorf("%w: to %q", ErrNoForwardMsgID, subject)
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
	inboxSub, err := w.conn.ChanSubscribe(inbox, replies)
	if err != nil {
		return nil, w.unsent(err, len(data))
	}
	msg := &nats.Msg{
		Subject: subject,
		Reply:   inbox,
		Data:    data,
		Header:  nats.Header{nats.MsgIdHdr: []string{opts.MsgID}},
	}
	deadline := time.Now().Add(cmp.Or(opts.Timeout, publishAcknowledgementTimeout))
	before := observe(w.conn)
	// Read right before the send, so the request takes only a violation the server reported of
	// its own message and never one an earlier send of the subject earned (deniedPublish).
	sentAt := time.Now()
	if err := w.conn.PublishMsg(msg); err != nil {
		_ = inboxSub.Unsubscribe()
		return nil, w.unsent(err, len(data))
	}
	return func() error {
		defer inboxSub.Unsubscribe()
		// The flush runs beside the wait for the answer rather than ahead of it. nats.go clears
		// the flushes pending on a connection only as that connection turns from connected to
		// reconnecting (clearPendingFlushCalls), so a flush begun after that moment - a server
		// that goes away right behind the publish is enough - queues a PING no server answers and
		// returns only at its timeout, which is the request's whole remaining time. Waiting for it
		// there would hold a request whose connection is already gone until its deadline, when the
		// loss is its answer the moment it is seen. A flush left out this way ends with its own
		// timeout, on a goroutine that then has nothing left to do; its outcome has no reader, and
		// settles nothing, because the request is answered already.
		flushed := make(chan error, 1)
		go func() {
			if tick := w.client.beforeRequestFlush; tick != nil {
				tick()
			}
			flushed <- confirmFlushed(w.conn, subject, before, deadline)
		}()
		// denied is the server's refusal of this very send, as the connection recorded it under
		// the subject (deniedPublish). It is the only evidence of a denial the flush cannot name:
		// nats.go keeps one last error per connection, so a violation of another subject recorded
		// between the publish and the flush's read leaves the flush unable to confirm the publish
		// (confirmPublished), and the request would wait out its timeout and end as one nobody
		// answered, naming another subject's violation. It is read on this goroutine alone and
		// never on the flush's: a flush still running after an early answer would take the
		// violation another send of the subject is owed.
		denied := func() error { return w.client.deniedPublish(subject, sentAt) }
		// unconfirmed is what the flush, once it has returned, leaves the timeout to report, named
		// as ErrPublishUnconfirmed whatever the reason; settleFlush takes the flush's outcome and
		// returns the denial that answers the request at once, nil for anything else.
		var unconfirmed error
		settleFlush := func(err error) error {
			flushed = nil
			if err == nil {
				return nil
			}
			if errors.Is(err, ErrPublishDenied) {
				return err
			}
			if denial := denied(); denial != nil {
				return denial
			}
			unconfirmed = err
			if !errors.Is(err, ErrPublishUnconfirmed) {
				// A flush that failed or ran out of the window leaves the publish as unconfirmed
				// as one confirmPublished could not judge, and says so under the one sentinel a
				// caller reads to tell the two timeouts apart.
				unconfirmed = fmt.Errorf("%w: %w", ErrPublishUnconfirmed, err)
			}
			return nil
		}
		// resend sends the request once more, to the same inbox and without its Nats-Msg-Id, after
		// the stream acknowledged a duplicate with no sequence: the record of an id at sequence 0
		// matches no send without the id. It is a write of the window, made under the checks
		// sendJetStream's resend makes right before its own, and its failure wraps ErrNotStored as
		// that one's does.
		resend := func() error {
			if expired := w.expired(); expired != nil {
				return fmt.Errorf("bus: request to %q: %w, and the window's deadline passed before it was sent without its id: %w", subject, ErrNotStored, expired)
			}
			if !w.current() {
				return fmt.Errorf("bus: %w: request to %q: %w, and the window's connection was lost or reconnected before it was sent without its id", ErrUnavailable, subject, ErrNotStored)
			}
			if err := w.conn.PublishMsg(&nats.Msg{Subject: subject, Reply: inbox, Data: data}); err != nil {
				return fmt.Errorf("bus: request to %q: %w, and its send without its id failed: %w", subject, ErrNotStored, w.unsent(err, len(data)))
			}
			return nil
		}
		resent := false
		// outcome names what reply settles the request as, and reports false for a reply that
		// settles nothing, so the wait goes on. The stream's refusal of the copy settles the
		// request as that refusal, since the stream answered and nothing later changes its answer
		// (streamRefusal). The stream's acknowledgement of a duplicate with no sequence has the
		// request sent once more without its id, at most once per request and only while the
		// request still waits (resending), and settles it only when that send is fenced or fails,
		// since otherwise the resend's own acknowledgement answers.
		outcome := func(reply *nats.Msg, resending bool) (bool, error) {
			if answers(reply) {
				return true, nil
			}
			if refusal := streamRefusal(reply); refusal != nil {
				return true, fmt.Errorf("bus: request to %q: the stream refused the copy: %w", subject, refusal)
			}
			if !resending || resent || !notStoredReply(reply) {
				return false, nil
			}
			resent = true
			if err := resend(); err != nil {
				return true, err
			}
			return false, nil
		}
		// drained reads the replies already in, so an answer that arrived with the connection's
		// loss or the deadline still counts.
		drained := func(resending bool) (bool, error) {
			for len(replies) > 0 {
				if settled, err := outcome(<-replies, resending); settled {
					return true, err
				}
			}
			return false, nil
		}
		lost := func() error {
			return fmt.Errorf("bus: %w: the window's connection was lost or reconnected before the request to %q was answered", ErrUnavailable, subject)
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		for {
			select {
			case err := <-flushed:
				if settled := settleFlush(err); settled != nil {
					return settled
				}
			case reply := <-replies:
				if settled, err := outcome(reply, true); settled {
					return err
				}
			case <-changed:
				if settled, err := drained(true); settled {
					return err
				}
				if !w.current() {
					return lost()
				}
				changed = w.client.ConnectionChanged()
			case <-timer.C:
				// The request is out of time: its answer is the timeout's, and no resend goes out
				// once the timeout is taken. A resend already sent may still be stored after it, as a
				// publish whose acknowledgement runs out of time may; the holder drops the copy by
				// its event id.
				if settled, err := drained(false); settled {
					return err
				}
				if !w.current() {
					return lost()
				}
				if flushed != nil {
					// The flush is bounded by this very deadline, so it is returning now, and
					// what it says belongs in the timeout's answer.
					if settled := settleFlush(<-flushed); settled != nil {
						return settled
					}
				}
				// The violation is recorded on nats.go's error callback, which can run after the
				// flush has returned, so the record is read once more here, with every send of
				// this request behind it: a denial found now is this request's answer, and the
				// timeout would have named another subject's violation or nothing at all.
				if denial := denied(); denial != nil {
					return denial
				}
				if unconfirmed != nil {
					return fmt.Errorf("bus: request to %q: %w: %w", subject, ErrNotAcknowledged, unconfirmed)
				}
				return fmt.Errorf("bus: request to %q: %w", subject, ErrNotAcknowledged)
			}
		}
	}, nil
}

// replyAck is a JetStream acknowledgement as it comes back on a Request's reply inbox: the
// PubAck's fields, and the error the server sends in their place when the stream refused the
// message.
type replyAck struct {
	nats.PubAck
	Error *nats.APIError `json:"error"`
}

// acknowledgement reads the stream's acknowledgement from reply, and reports false for a reply
// that is not one: anything a subscriber of the subject sent, or the server's refusal, which
// names no stream.
func acknowledgement(reply *nats.Msg) (replyAck, bool) {
	var ack replyAck
	if json.Unmarshal(reply.Data, &ack) != nil || ack.Stream == "" {
		return replyAck{}, false
	}
	return ack, true
}

// answers reports whether reply answers a Request with the copy it asked for: the stream's
// acknowledgement of a copy it holds, at a sequence and without an error, a duplicate's included.
// Nothing else answers one that way - no session subscribes to its agent subject, and a reply a
// subscriber of the subject did send is passed over. An acknowledgement of a duplicate with no
// sequence answers nothing either: the stream holds no copy of the request, which goes out once
// more without its id instead (notStoredReply, request). An acknowledgement that carries an error
// is the stream's refusal, and settles the request as that refusal (streamRefusal).
func answers(reply *nats.Msg) bool {
	ack, ok := acknowledgement(reply)
	return ok && ack.Error == nil && ack.Sequence > 0
}

// streamRefusal names the stream's refusal of a Request's copy, as reply carries it: the error
// the server sent in the acknowledgement's place, a full stream's as ErrStreamFull, which is how
// every other path hands that refusal to its caller (streamFull, acknowledged,
// PendingForward.Wait). Every other reply is nil. The refusal is the stream's own answer, so it
// never wraps ErrNotAcknowledged, which says the stream gave none, and it settles the request at
// once rather than leaving it to run out its timeout.
func streamRefusal(reply *nats.Msg) error {
	ack, ok := acknowledgement(reply)
	if !ok || ack.Error == nil {
		return nil
	}
	return streamFull(ack.Error)
}

// notStoredReply reports whether reply is the stream's acknowledgement of a duplicate with no
// sequence and no error (ErrNotStored): a repeat of a message id the stream holds no message for,
// which a clustered server answers every send under an id it refused with.
func notStoredReply(reply *nats.Msg) bool {
	ack, ok := acknowledgement(reply)
	return ok && ack.Error == nil && ack.Duplicate && ack.Sequence == 0
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

// acknowledged waits for future's acknowledgement of a publish to topic, sent at sentAt, and names
// what it came to: nil for a message the stream stored or holds, ErrNotStored, ErrPublishDenied for
// a publish the server refused under the connection's own publish permissions, ErrUnavailable when
// the connection failed or nats.go reported the disconnect, ErrNotAcknowledged when the
// acknowledgement ran out of time or the server answered with no responders on a connection that
// stayed current, or the server's refusal, a full stream's as ErrStreamFull.
func (w *window) acknowledged(future nats.PubAckFuture, topic string, sentAt time.Time) error {
	select {
	case ack := <-future.Ok():
		return notStored(ack)
	case err := <-future.Err():
		// A denied publish is acknowledged by nothing, so the violation the connection reported
		// (deniedPublish) is the only evidence of it, read under this send's own subject and its
		// own send time. A window sends every destination of one event on one connection, so a
		// grant that denies several of them has the server report a violation of each: the record
		// answers each send with its own, where the connection's one last error held only the one
		// that landed last and left every other denied send to report this arm's ErrUnavailable.
		if denied := w.client.deniedPublish(topic, sentAt); denied != nil {
			return denied
		}
		if !w.current() || errors.Is(err, nats.ErrDisconnected) {
			return fmt.Errorf("bus: %w: %w", ErrUnavailable, err)
		}
		if errors.Is(err, nats.ErrAsyncPublishTimeout) || errors.Is(err, nats.ErrNoResponders) {
			return fmt.Errorf("bus: %w: %w", ErrNotAcknowledged, err)
		}
		return streamFull(err)
	}
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
