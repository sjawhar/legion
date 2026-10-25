package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A live socket's subscriptions. Each subscribe frame names its subscription by a sub id of the
// page's own and dispatches on its view's kind: a delivery view (timeline, measures, pipeline)
// goes to the delivery hub, which holds the subscription and sends its frames through the socket's
// queue; any other kind the server serves (liveKinds: the events view, an agent view) starts a
// liveSubscription, which runs on a goroutine of its own and waits for room in the socket's stream
// queue. The read loop never waits on a subscription: it starts one and returns, and stops one
// beside itself.
//
// The socket holds at most contracts.LiveSocketMaxSubscriptions, of every kind; one more is
// refused TOO_MANY_SUBSCRIPTIONS and starts nothing. A subscribe for a sub the socket holds
// replaces it, whatever its kind. Every subscription ends when the socket does.

// errLiveSocketClosed is what SendWait answers once the socket has ended.
var errLiveSocketClosed = errors.New("dispatch: the live socket closed")

// liveSubscription is one events or agent subscription, running on a goroutine of its own. stop
// ends it: it cancels the subscription's context and returns once the goroutine has returned. The
// socket calls it once, beside its read loop when the page unsubscribes or replaces the sub, or as
// the socket ends; the subscription may already have ended on its own.
type liveSubscription interface{ stop() }

// liveStreamer is what an events or agent subscription writes through.
type liveStreamer interface {
	// SendWait queues frame on the stream queue, waiting for room; it returns ctx's error once
	// ctx (the subscription's own) ends, and errLiveSocketClosed once the socket ends.
	SendWait(ctx context.Context, frame any) error
	// Human reports whether the caller is a person (cookie identity) rather than a bearer.
	Human() bool
}

// liveKind is one view kind a live socket serves beside the delivery views.
type liveKind struct {
	// resumes is whether the kind takes a resume, an EventsLiveResume; a kind that does not (an
	// agent view) refuses any.
	resumes bool
	// start checks the view, and either refuses it with an error frame (whose sub the socket sets)
	// having started nothing, or starts the subscription's goroutine and returns at once. The
	// goroutine writes the subscription's frames, each naming sub, through stream until ctx ends.
	// ctx ends when the socket does, and carries a pool mark of the subscription's own
	// (store.ForConcurrentRead), so it may read the database beside the socket's other
	// subscriptions; resume is nil when the page sent none.
	start func(ctx context.Context, stream liveStreamer, sub string, view LiveView, resume *EventsLiveResume) (liveSubscription, *LiveError)
}

// liveSub is one subscription a socket holds. A delivery view's is the hub's and has neither a
// running subscription nor a stream of its own.
type liveSub struct {
	running liveSubscription
	stream  *liveSubscriptionStream
}

// liveSubscriptionStream is the liveStreamer one events or agent subscription writes through.
// Every frame it queues carries it, so the writer passes over the frames of a subscription that
// has stopped: none of them follows what replaced it.
type liveSubscriptionStream struct {
	socket  *liveSocket
	stopped atomic.Bool
}

// liveStreamFrame is one frame on a socket's stream queue, with the subscription that queued it.
type liveStreamFrame struct {
	from  *liveSubscriptionStream
	frame any
}

func (stream *liveSubscriptionStream) SendWait(ctx context.Context, frame any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-stream.socket.closed:
		return errLiveSocketClosed
	default:
	}
	select {
	case stream.socket.stream <- liveStreamFrame{from: stream, frame: frame}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.socket.closed:
		return errLiveSocketClosed
	}
}

func (stream *liveSubscriptionStream) Human() bool { return stream.socket.human }

// subscribeLive answers one subscribe frame: it starts the subscription the frame's view's kind
// names in place of any the socket holds under the frame's sub, or answers the error frame refusing
// it. A frame the socket refuses itself (no sub, a kind it does not serve, a resume of another
// kind's shape, a sub past the cap) changes nothing. Otherwise the subscription the sub held ends
// first, so a refusal from the hub or the kind leaves the sub holding nothing; but a delivery view
// in a delivery view's place is the hub's to replace, and the hub keeps the earlier one when it
// refuses the new, as it always has.
func (s *server) subscribeLive(socket *liveSocket, frame LiveSubscribe) {
	sub := frame.Sub
	if sub == "" {
		socket.Send(liveRefusal("", "INVALID_FRAME", "a subscribe frame names its sub"))
		return
	}
	kind := frame.View.Kind
	delivery := kind == deliveryLiveTimeline || kind == deliveryLiveMeasures || kind == deliveryLivePipeline
	other, served := s.liveKinds[kind]
	if !delivery && !served {
		socket.Send(liveRefusal(sub, "INVALID_FRAME", "view.kind "+strconv.Quote(kind)+" is not a view this socket serves"))
		return
	}
	var deliveryResume *DeliveryLiveResume
	var eventsResume *EventsLiveResume
	var err error
	switch {
	case delivery:
		if frame.View.SessionID != nil {
			socket.Send(liveRefusal(sub, "INVALID_FRAME", "a "+kind+" view names no session_id"))
			return
		}
		deliveryResume, err = decodeLiveResume[DeliveryLiveResume](frame.Resume)
	case other.resumes:
		eventsResume, err = decodeLiveResume[EventsLiveResume](frame.Resume)
	case hasLiveResume(frame.Resume):
		err = errors.New("view.kind " + strconv.Quote(kind) + " takes no resume")
	}
	if err != nil {
		socket.Send(liveRefusal(sub, "INVALID_FRAME", "a subscribe frame's resume: "+err.Error()))
		return
	}
	held := socket.subs[sub]
	if held == nil && len(socket.subs) >= contracts.LiveSocketMaxSubscriptions {
		socket.Send(liveRefusal(sub, "TOO_MANY_SUBSCRIPTIONS", fmt.Sprintf(
			"a socket holds at most %d subscriptions; unsubscribe one before subscribing another", contracts.LiveSocketMaxSubscriptions)))
		return
	}

	if delivery {
		if held != nil && held.running != nil {
			s.endLive(socket, sub, held)
		}
		if s.deliveryHub.Subscribe(socket, deliverySubscription{Sub: sub, View: frame.View.DeliveryLiveView, Resume: deliveryResume}) {
			socket.subs[sub] = &liveSub{}
		}
		return
	}
	if held != nil {
		s.endLive(socket, sub, held)
	}
	ctx, err := store.ForConcurrentRead(socket.ctx)
	if err != nil {
		// The read loop holds no pooled connection, so this is a bug, not the page's frame.
		slog.Error("dispatch: live socket could not mark a subscription's context for the pool", "error", err)
		socket.Send(liveRefusal(sub, codeInternal, "the subscription could not start"))
		return
	}
	stream := &liveSubscriptionStream{socket: socket}
	running, refusal := other.start(ctx, stream, sub, frame.View, eventsResume)
	if refusal != nil {
		refusal.Sub = sub
		socket.Send(*refusal)
		return
	}
	socket.subs[sub] = &liveSub{running: running, stream: stream}
}

// unsubscribeLive ends the subscription sub names, of any kind; a sub the socket does not hold is
// no error.
func (s *server) unsubscribeLive(socket *liveSocket, sub string) {
	if held := socket.subs[sub]; held != nil {
		s.endLive(socket, sub, held)
	}
}

// endLive ends the subscription the socket holds under sub. The hub drops a delivery view's. An
// events or agent subscription stops beside the read loop, which does not wait for it: its frames
// still queued are no longer written, and the socket waits for it to return once it ends.
func (s *server) endLive(socket *liveSocket, sub string, held *liveSub) {
	delete(socket.subs, sub)
	if held.running == nil {
		s.deliveryHub.Unsubscribe(socket, sub)
		return
	}
	held.stream.stopped.Store(true)
	socket.ending.Add(1)
	go func() {
		defer socket.ending.Done()
		held.running.stop()
	}()
}

// endLiveSubscriptions stops every events and agent subscription the socket holds, once it has
// ended, and returns when each, and each it stopped earlier, has returned. The hub drops the
// delivery views' (deliveryHub.Close).
func (s *server) endLiveSubscriptions(socket *liveSocket) {
	for sub, held := range socket.subs {
		if held.running != nil {
			s.endLive(socket, sub, held)
		}
	}
	socket.ending.Wait()
}

// hasLiveResume reports whether a subscribe frame carried a resume: JSON null is none.
func hasLiveResume(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// decodeLiveResume decodes a subscribe frame's resume as T, refusing a member T does not declare,
// so a resume of another kind's shape is refused; it is nil when the frame carried none.
func decodeLiveResume[T any](raw json.RawMessage) (*T, error) {
	if !hasLiveResume(raw) {
		return nil, nil
	}
	var resume T
	if err := decodeSocketClientFrame(raw, &resume); err != nil {
		return nil, err
	}
	return &resume, nil
}
