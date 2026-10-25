package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The events view is the Dispatch event feed on a page's live socket: every committed event, in id
// order, one event frame each. It replaced GET /api/v1/events, the server-sent event stream, and
// keeps every rule that stream kept.
//
// Its live source is the process's event stream relay (event_stream_relay.go), which publishes
// every committed event it reads back out of Postgres - this process's and the other
// envoy-dispatch process's alike. A subscription subscribes to the relay before it reads the log's
// head, which is what leaves no gap: an event committing after the relay subscription exists lands
// in its channel whatever its id, while one committing before it is at or below the head read next
// and so is replayed by the catch-up. The relay reads its cursor before it answers a subscription,
// so it cannot already stand past that head either.
//
// That ordering is also why a subscription that names no since resolves the head itself rather
// than a page asking for it first: a page that read the head through a separate request and then
// subscribed from it would lose an event committing between the two, excluded from the catch-up
// (id <= since) and missed by a relay subscription made only afterwards. The one head a page reads
// beforehand is GET /auth/whoami's event_head, which it reads before it starts any read the feed
// keeps current, so what commits after it is the feed's to replay.
//
// Every subscription resolves the head, a resuming one too. The event log can be reset under a
// reader (`truncate events restart identity`, the browser suite's e2e/seed.ts): the ids restart at
// 1 and the relay, which notices, ends every open subscription, and the page subscribes again. A
// page only ever raises its cursor and never lowers it, so it resumes with a since above the new
// log's head. Unclamped, the live loop would then drop every event of the new log for ever, on a
// subscription that stays open: a reader silent for good, with nothing logged and nothing failed.
// So a since above the head starts at the head, as no since does.

// startEventsView checks an events view and its resume, then starts the view's subscription. The
// view names its kind alone, and the resume the decimal id of the last event the page applied.
func (s *server) startEventsView(ctx context.Context, stream liveStreamer, sub string, view LiveView, resume *EventsLiveResume) (liveSubscription, *LiveError) {
	if view.Window != nil || view.Query != nil || view.At != nil || view.SessionID != nil {
		refusal := liveRefusal(sub, "INVALID_FRAME", "an events view names its kind and nothing else")
		return nil, &refusal
	}
	var since *int64
	if resume != nil {
		value, err := strconv.ParseInt(resume.Since, 10, 64)
		if err != nil || value < 0 {
			refusal := liveRefusal(sub, "INVALID_FRAME", "resume.since must be a decimal event id")
			return nil, &refusal
		}
		since = &value
	}
	return s.startEventsSubscription(ctx, stream, sub, since), nil
}

// eventsSubscription is one running events view. stop cancels the subscription's own context,
// which ends its goroutine wherever it waits, and returns once the goroutine has: the socket stops
// a subscription while it stays open (an unsubscribe, a replacement) as well as when it ends.
type eventsSubscription struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (running *eventsSubscription) stop() {
	running.cancel()
	<-running.done
}

// startEventsSubscription starts sub's event feed on a goroutine of its own, after since when it
// is not nil, and returns at once. The goroutine writes the subscription's frames through stream
// until ctx ends or the subscription is stopped: events_start first, then the catch-up after the
// id it names, then the events the relay publishes, and an ended frame (behind) should the relay
// drop it or a read fail.
func (s *server) startEventsSubscription(ctx context.Context, stream liveStreamer, sub string, since *int64) liveSubscription {
	ctx, cancel := context.WithCancel(ctx)
	running := &eventsSubscription{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		defer cancel()
		s.feedEvents(ctx, stream, sub, since)
	}()
	return running
}

// feedEvents is one events subscription's whole life: it returns once the subscription has ended,
// on its own or because ctx did.
func (s *server) feedEvents(ctx context.Context, stream liveStreamer, sub string, since *int64) {
	relayed, unsubscribe, err := s.eventRelay.subscribe(ctx)
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "subscribe to the live event stream", err)
		return
	}
	defer unsubscribe()
	head, err := EventHead(ctx, s.deps.Store)
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "read the event log's head", err)
		return
	}
	from := head
	if since != nil && *since <= head {
		from = *since
	}
	if stream.SendWait(ctx, liveEventsStart(sub, from)) != nil {
		return
	}
	send := func(event model.Event) bool {
		frame, err := liveEventFrame(sub, event)
		if err != nil {
			endEventsAfterFailure(ctx, stream, sub, "encode an event", err)
			return false
		}
		return stream.SendWait(ctx, frame) == nil
	}

	// Ids are assigned by nextval() when the insert statement runs, before commit, so a lower id
	// can commit after a higher one is already visible to a catch-up query - a still-open
	// transaction that took an earlier id is the case the relay subscription above, taken before
	// any catch-up query runs, exists to cover. The catch-up pages through the whole backlog on that
	// one subscription without ever ending it: a capped page that ended the subscription and sent
	// the page to subscribe again would leave a gap between "read this page" and "the next
	// subscription subscribes" where a low id could commit and be missed by both the next page's
	// `id > cursor` read and the relay subscription already ended.
	cursor := from
	sentDuringCatchup := map[int64]bool{}
	for {
		page, err := s.readEventsAfterID(ctx, cursor)
		if err != nil {
			endEventsAfterFailure(ctx, stream, sub, "read the event log", err)
			return
		}
		for _, event := range page {
			if !send(event) {
				return
			}
			sentDuringCatchup[event.ID] = true
			cursor = event.ID
		}
		if len(page) < maxEventReplay {
			break
		}
	}

	// The live loop never compares an event's id with the last id it sent: a lower id committing
	// after a higher one was already sent would be dropped for ever, and that is exactly what the
	// relay sends after a redial. The one comparison it makes is with from, which is fixed for the
	// subscription: the commit-order lock events.Broker.Append holds until commit means no id at or
	// below a committed max(id) is still uncommitted, so nothing above from can hide below it later.
	// A relay still catching up to a subscription made after it sends ids at or below from - events
	// this page has already seen, or backlog a page with no since asked not to get - and
	// sentDuringCatchup cannot be asked about them, since it holds only the ids catch-up sent, all
	// above from.
	//
	// That argument covers a log whose ids only ever grow. A reset reuses them, and the clamp above
	// is what extends the comparison across one: from is held at or below the head this
	// subscription resolved, so an id the new log hands out is never taken for an id the old log
	// already showed this page. What the clamp cannot do is tell this page apart from the events of
	// the log it was reading; the relay ends the subscription at a reset instead, and the page comes
	// back on a new subscription, which resolves the new log's head.
	//
	// sentDuringCatchup is for an id catch-up sent that is also still in the relay subscription's
	// channel: committed, and so readable, in the narrow window between the relay subscription and
	// a catch-up read, but not yet drained from it.
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-relayed:
			if !open {
				// The relay dropped this subscription: it fell more than the relay holds for one
				// subscriber behind, or the log was reset under the relay's cursor. The page
				// subscribes again from the last id it applied, and its catch-up replays what it missed.
				_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
				return
			}
			if event.ID <= from || sentDuringCatchup[event.ID] {
				continue
			}
			if !send(event) {
				return
			}
		}
	}
}

// endEventsAfterFailure ends a subscription whose read failed, with an ended frame (behind), so its
// page subscribes again from the last id it applied, as the server-sent stream's client reconnected
// after its stream ended. It waits the relay's own retry first: a page answers an ended frame by
// subscribing again at once, and a read that keeps failing (a database that does not answer) would
// otherwise have the page and the subscription take turns as fast as the failure comes back. A
// subscription whose own context ended (stopped, or its socket closed) sends nothing: that is why
// its read failed.
func endEventsAfterFailure(ctx context.Context, stream liveStreamer, sub, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.Warn("dispatch: an events subscription could not "+what+"; it ends, and its page subscribes again", "error", err)
	if !waitFor(ctx, eventRelayRetry) {
		return
	}
	_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
}

// liveEventsStart is the frame opening sub, a subscription that starts after the event since.
func liveEventsStart(sub string, since int64) LiveEventsStart {
	return LiveEventsStart{Type: "events_start", Sub: sub, Since: strconv.FormatInt(since, 10)}
}

// liveEventFrame is event's frame on sub: its decimal id and its own JSON encoding, byte for byte
// the data line the server-sent event stream carried for it.
func liveEventFrame(sub string, event model.Event) (LiveEventFrame, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return LiveEventFrame{}, err
	}
	return LiveEventFrame{Type: "event", Sub: sub, ID: strconv.FormatInt(event.ID, 10), Event: data}, nil
}
