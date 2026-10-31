package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The events view is the Dispatch event feed on a page's live socket: every committed event, in id
// order, one event frame each.
//
// Its live source is the process's event stream relay (event_stream_relay.go), which publishes
// every committed event it reads back out of Postgres - this process's and the other
// envoy-dispatch process's alike - up to the safe horizon (events.Horizon), in id order. A
// subscription takes its relay subscription together with the relay's published cursor, which is
// what leaves no gap: every event at or below that cursor is final and is replayed by the
// catch-up, and every event above it reaches the relay subscription (feedEvents).
//
// That handover is also why a subscription that names no since starts where the relay stands
// rather than at a head a page asked for first: a page that read the newest id through a separate
// request and subscribed from it would lose an event that drew a lower id and committed after
// that read. The one head a page reads beforehand is GET /auth/whoami's event_head, which it reads
// before it starts any read the feed keeps current: an id at or below which every event is already
// final (eventFeedHead), so what commits after it stands above it and is the feed's to replay.
//
// The event log can be reset under a reader (`truncate events restart identity`, the browser
// suite's e2e/seed.ts): the ids restart at 1 and the relay, which notices, ends every open
// subscription, and the page subscribes again. A page only ever raises its cursor and never lowers
// it, so it resumes with a since above the new log's newest id. Unclamped, the live loop would
// then drop every event of the new log for ever, on a subscription that stays open: a reader
// silent for good, with nothing logged and nothing failed. So a since above the newest id starts
// at the relay's cursor, as no since does.

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

// startEventsSubscription starts sub's event feed on a goroutine of its own, after since when it
// is not nil, and returns at once. The goroutine writes the subscription's frames through stream
// until ctx ends or the subscription is stopped: events_start first, then the catch-up after the
// id it names, then the events the relay publishes, and an ended frame (behind) should the relay
// drop it or a read fail.
func (s *server) startEventsSubscription(ctx context.Context, stream liveStreamer, sub string, since *int64) liveSubscription {
	return runSubscription(ctx, func(ctx context.Context) { s.feedEvents(ctx, stream, sub, since) })
}

// feedEvents is one events subscription's whole life: it returns once the subscription has ended,
// on its own or because ctx did.
//
// Event ids do not commit in id order across owners (events.Broker.Append), so a page's resume
// point and every point this subscription hands over at must be ones no later commit can fall at
// or below. The relay publishes only up to the safe horizon (events.Horizon), each event once and
// in id order, and its published cursor, read together with the relay subscription
// (eventStreamRelay.subscribe), is the handover: every event at or below it is final and is
// replayed here from the log, and every event above it reaches the relay subscription.
//
// A subscription that names no since starts at the relay's published cursor. One that names a
// since at or below the log's newest id starts there, is replayed every event after it up to the
// published cursor, and says so in its events_start (replay_complete), so its page may skip the
// whole refresh it owes a gap: the page's since is an id it was sent, at or below a horizon, or the
// event_head GET /auth/whoami answered (eventFeedHead), at or below which every event was already
// final. A since above the newest id is an id of a log this one replaced, which starts at the
// published cursor and says no such thing.
//
// The relay keeps a reset log's cursor until its reset check has run (resetCursor), which waits for
// a horizon proven after the reset. A relay subscription taken meanwhile carries a published cursor
// that names a position in the old log: above the new log's newest id, or at an id whose row is
// gone or carries another created_at than the relay read (eventLogWasReset). Started there, the
// subscription would drop the new log's events up to it, or start and then be ended by the relay's
// reset, sending the page through a resubscribe and, for a page with no complete replay, the whole
// refresh it owes a gap. So the subscription waits
// for that reset, which ends the relay subscription, and subscribes again on the cursor the relay
// moved to. A subscription with no since then starts at replacedFrom, read when it found the relay
// on the replaced log, rather than at the relay's new cursor: that cursor is taken under a horizon
// proven after the reset, so an event of the new log that commits while that proof is awaited can
// stand at or below it. replacedFrom is the id before the first one missing from the new log,
// whose ids restart at 1, which is every event committed then and none still open.
func (s *server) feedEvents(ctx context.Context, stream liveStreamer, sub string, since *int64) {
	replacedFrom := int64(-1)
	var relayed <-chan model.Event
	var unsubscribe func()
	defer func() {
		if unsubscribe != nil {
			unsubscribe()
		}
	}()
	var published, head int64
	for {
		var err error
		var publishedAt time.Time
		var cursorAt *time.Time
		relayed, unsubscribe, published, publishedAt, err = s.eventRelay.subscribe(ctx)
		if err != nil {
			unsubscribe = nil
			endEventsAfterFailure(ctx, stream, sub, "subscribe to the event stream relay", err)
			return
		}
		// The log's newest id, and the row at the relay's cursor, as they stand now, after the
		// relay subscription began. A cursor above the newest id, or one whose row is gone or
		// carries another created_at, is a relay whose reset check has not yet run
		// (eventLogWasReset, the relay's own test).
		if head, cursorAt, err = eventCursorCheck(ctx, s.deps.Store.Pool, published); err != nil {
			endEventsAfterFailure(ctx, stream, sub, "read the event log's head", err)
			return
		}
		if !eventLogWasReset(published, publishedAt, head, cursorAt) {
			break
		}
		if since == nil && replacedFrom < 0 {
			if replacedFrom, err = s.eventLogUnbrokenThrough(ctx); err != nil {
				endEventsAfterFailure(ctx, stream, sub, "read the event log's unbroken ids", err)
				return
			}
		}
		signal(s.eventRelay.wake)
		// Whatever the relay publishes before its reset check is a position in the old log, so it
		// is dropped; the close is its reset ending this relay subscription.
		for open := true; open; {
			select {
			case <-ctx.Done():
				return
			case _, open = <-relayed:
			}
		}
		unsubscribe()
		unsubscribe = nil
	}

	replayComplete := since != nil && *since <= head
	from := published
	switch {
	case replayComplete:
		from = *since
	case replacedFrom >= 0:
		from = min(replacedFrom, published)
	}
	if stream.SendWait(ctx, liveEventsStart(sub, from, replayComplete)) != nil {
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

	// The catch-up: (from, published] from the log. Every event in it is final, since published is
	// at or below the safe horizon, so a page read now holds the rows any later read would, and
	// moving the cursor to a page's last row skips nothing. It pages through the whole range on the
	// one relay subscription, never ending it partway: whatever the relay publishes meanwhile waits
	// in the subscription (or overflows it, which ends the subscription with an ended frame, behind).
	//
	// It reads through the live pool, as the relay that feeds this subscription does
	// (eventStreamRelay): a socket's backlog never queues for a request's connection.
	live, err := s.deps.Store.Pool.Live()
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "open the live pool", err)
		return
	}
	for cursor := from; cursor < published; {
		page, err := s.readEventsAfterID(ctx, live, cursor, published)
		if err != nil {
			endEventsAfterFailure(ctx, stream, sub, "read the event log", err)
			return
		}
		for _, event := range page {
			if !send(event) {
				return
			}
			cursor = event.ID
		}
		if len(page) < maxEventReplay {
			break
		}
	}

	// The live feed sends only events above both from and published. The relay publishes into this
	// subscription only events above published, each once and in strictly increasing id order, so
	// the catch-up's (from, published] and the feed's ids above max(from, published) are together
	// every event after from, each sent exactly once and in id order. A from above published
	// belongs to a page that has already seen events this process's relay has not yet published
	// (read from the other process, or before a resubscribe): the relay's ids at or below it are
	// events that page has, and the comparison drops them.
	//
	// That argument covers a log whose ids only ever grow. A reset reuses them, and the clamp above
	// is what extends the comparison across one: a since above the newest id is replaced by
	// published, so an id the new log hands out is not dropped as one the old log already showed
	// this page. What the clamp cannot do is tell this page apart from the events of the log it was
	// reading; the relay ends the subscription at a reset instead, and the page comes back on a new
	// subscription, which clamps its since against the log as it then stands.
	floor := max(from, published)
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
			if event.ID <= floor {
				continue
			}
			if !send(event) {
				return
			}
		}
	}
}

// endEventsAfterFailure ends a subscription whose read failed, with an ended frame (behind), so its
// page subscribes again from the last id it applied. It waits the relay's own retry first: a page
// answers the end of a feed that had started by subscribing again at once, and a read that keeps
// failing (a database that does not answer) would otherwise have the page and the subscription take
// turns as fast as the failure comes back. An end that comes before the feed's events_start, as
// every end does while the head cannot be read, the page answers after its own doubling backoff. A
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

// liveEventsStart is the frame opening sub, a subscription that starts after the event since;
// replayComplete says its catch-up replays every event the page missed.
func liveEventsStart(sub string, since int64, replayComplete bool) LiveEventsStart {
	return LiveEventsStart{Type: "events_start", Sub: sub, Since: strconv.FormatInt(since, 10), ReplayComplete: replayComplete}
}

// liveEventFrame is event's frame on sub: its decimal id and its own JSON encoding, byte for byte,
// which is what a page reads every event as.
func liveEventFrame(sub string, event model.Event) (LiveEventFrame, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return LiveEventFrame{}, err
	}
	return LiveEventFrame{Type: "event", Sub: sub, ID: strconv.FormatInt(event.ID, 10), Event: data}, nil
}
