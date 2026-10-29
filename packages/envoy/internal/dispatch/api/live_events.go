package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The events view is the Dispatch event feed on a page's live socket: every committed event, in id
// order, one event frame each.
//
// Its live source is the process's event stream relay (event_stream_relay.go), which publishes
// every committed event it reads back out of Postgres - this process's and the other
// envoy-dispatch process's alike - each encoded once for every subscriber. The subscription
// answers head, the id the relay had published through when it was made: every event above head
// arrives on the subscription, and every event at or below it has committed, so the replay reads
// those from the log. Nothing falls between the two.
//
// That is also why a subscription that names no since resolves its position itself rather than a
// page asking for it first: a page that read the head through a separate request and then
// subscribed from it would lose an event committing between the two, excluded from the replay
// (id <= since) and missed by a relay subscription made only afterwards. The one head a page reads
// beforehand is GET /auth/whoami's event_head, which it reads before it starts any read the feed
// keeps current, so what commits after it is the feed's to replay.
//
// What a resume is replayed is bounded (resolveEventReplay): past maxLiveReplay events or
// maxLiveReplayBytes of their encodings it is replayed nothing, and its events_start says
// replay_complete false, which is the page's cue to refresh everything it shows once. A
// subscription holds no record of what it replayed: the one id it keeps is the highest it has
// sent, so its state is bounded whatever the backlog was, and it is freed with the subscription
// when the page unsubscribes or the socket closes.

const (
	// maxLiveReplay and maxLiveReplayBytes bound what a resuming events subscription is replayed:
	// at most 256 events, whose encodings total at most 1 MiB. A page further behind is replayed
	// nothing and refreshes what it shows, which costs less than sending it that backlog
	// (resolveEventReplay).
	maxLiveReplay      = 256
	maxLiveReplayBytes = 1 << 20
	// liveEventsBuffer is how many relayed events one events subscription may fall behind before
	// the relay's stream drops it: 64, more than any one request commits. A dropped subscription
	// subscribes to the relay again and catches up what it missed from the event log, within the
	// replay's bounds (resumeAfterOverflow).
	liveEventsBuffer = 64
)

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
// until ctx ends or the subscription is stopped: events_start first, then the replay after the
// id it names, then the events the relay publishes, and an ended frame (behind) should the relay
// end its generation, a catch-up pass the replay's bounds, or a read fail.
func (s *server) startEventsSubscription(ctx context.Context, stream liveStreamer, sub string, since *int64) liveSubscription {
	return runSubscription(ctx, func(ctx context.Context) { s.feedEvents(ctx, stream, sub, since) })
}

// feedEvents is one events subscription's whole life: it returns once the subscription has ended,
// on its own or because ctx did.
func (s *server) feedEvents(ctx context.Context, stream liveStreamer, sub string, since *int64) {
	subscription, err := s.eventRelay.subscribe(ctx, liveEventsBuffer)
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "subscribe to the event stream relay", err)
		return
	}
	defer func() { subscription.cancel() }()
	replay, err := s.resolveEventReplay(ctx, since, subscription.head)
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "read the event log", err)
		return
	}
	if stream.SendWait(ctx, liveEventsStart(sub, replay.start, replay.complete)) != nil {
		return
	}
	if !sendReplayedEvents(ctx, stream, sub, replay.events) {
		return
	}

	// The live loop skips an event at or below after, the highest id this subscription holds: it
	// starts at replay.after (the replay's last id, or what the page held through its since when
	// the relay stands behind it) and moves to each id the loop sends. The relay publishes in id
	// order, and the commit-order lock events.Broker.Append holds until commit means no id at or
	// below a committed max(id) is still uncommitted, so nothing above after can hide below it
	// later. One id, not a set: a relay still catching up to a subscription made after it sends
	// ids at or below after - events this page has already seen, or backlog a page with no since
	// asked not to get - and the comparison is all it takes to leave them out.
	//
	// A subscription that falls liveEventsBuffer events behind is dropped by the relay's stream,
	// which closes its channel while the generation runs on (Stream.Publish). The loop reads what
	// the channel still holds, subscribes again and catches up from the log what it missed after
	// after (resumeAfterOverflow), then goes on live on the new subscription. A backlog past the
	// replay's bounds ends the subscription instead: the page subscribes again from the last id
	// it applied, which resolves its position as any resume does.
	//
	// A reset reuses ids, and the comparison cannot tell this page apart from the events of the
	// log it was reading; the relay ends the generation at a reset instead, which ends this
	// subscription, and the page comes back on a new one, which resolves the new log's head.
	after := replay.after
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-subscription.events:
			if open {
				if event.Event.ID <= after {
					continue
				}
				if stream.SendWait(ctx, liveEventFrame(sub, event)) != nil {
					return
				}
				after = event.Event.ID
				continue
			}
			select {
			case <-subscription.ended:
				// The relay ended this generation: the log was reset under its cursor. The page
				// subscribes again from the last id it applied. A shutdown ends the generation
				// too, but it closes the socket with 1012 and stops its writer (live_socket.go),
				// so no ended frame reaches the page then: this one is a reset's.
				_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
				return
			default:
			}
			next, caughtUp, ok := s.resumeAfterOverflow(ctx, after)
			if !ok {
				_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
				return
			}
			subscription.cancel()
			subscription = next
			if !sendReplayedEvents(ctx, stream, sub, caughtUp.events) {
				return
			}
			after = caughtUp.after
		}
	}
}

// sendReplayedEvents writes each replayed event's frame on sub, in order; false once the
// subscription has ended.
func sendReplayedEvents(ctx context.Context, stream liveStreamer, sub string, events []relayedEvent) bool {
	for _, event := range events {
		if stream.SendWait(ctx, liveEventFrame(sub, event)) != nil {
			return false
		}
	}
	return true
}

// eventReplay is what one events subscription is replayed before its live events, or after an
// overflow (resumeAfterOverflow): the events to send, whether they are everything the page lacks
// (complete), the id its events_start names (start), and the id at or below which the live loop
// skips an event (after).
type eventReplay struct {
	events   []relayedEvent
	complete bool
	start    int64
	after    int64
}

// resolveEventReplay answers a subscription made at head (eventStreamRelay.subscribe).
//
// A subscription that names no since asked for no backlog: it is replayed nothing, starts at head
// and is not complete. A resume at or below head is replayed (since, head] when that is at most
// maxLiveReplay events of at most maxLiveReplayBytes of encodings, and is then complete; past
// either bound it is replayed nothing and its page refreshes what it shows (replay_complete false
// is its signal). Rows are counted, not ids subtracted: ids have gaps.
//
// A resume above head is either ahead of this process's relay or from a replaced log. When since
// is at or below the log's max(id), the relay has not yet published what the page already holds
// (another process served it, or the newest commit is not yet read): nothing is replayed, the
// replay is complete, and the live loop skips what the relay publishes through since. When since
// is above max(id), the log was replaced under the page (`truncate events restart identity`, the
// browser suite's e2e/seed.ts) and the page, which only ever raises its cursor and never lowers
// it, resumes from an id of the old log: it is replayed nothing, is not complete and starts at
// head, so the new log's events reach it rather than sitting below a since that will never be
// passed.
func (s *server) resolveEventReplay(ctx context.Context, since *int64, head int64) (eventReplay, error) {
	if since == nil {
		return eventReplay{start: head, after: head}, nil
	}
	resume := *since
	if resume > head {
		newest, err := EventHead(ctx, s.deps.Store)
		if err != nil {
			return eventReplay{}, err
		}
		if resume <= newest {
			return eventReplay{complete: true, start: resume, after: resume}, nil
		}
		return eventReplay{start: head, after: head}, nil
	}
	var behind int
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select count(*) from (select 1 from events where id > $1 and id <= $2 order by id limit $3) as behind
	`, resume, head, maxLiveReplay+1).Scan(&behind); err != nil {
		return eventReplay{}, err
	}
	if behind > maxLiveReplay {
		return eventReplay{start: head, after: head}, nil
	}
	missed, err := s.readEventRows(ctx, eventSelect+`
		where e.id > $1 and e.id <= $2
		order by e.id asc limit $3
	`, resume, head, maxLiveReplay)
	if err != nil {
		return eventReplay{}, err
	}
	replayed := make([]relayedEvent, 0, len(missed))
	size := 0
	for _, event := range missed {
		encoded, err := encodeLiveEvent(event)
		if err != nil {
			return eventReplay{}, err
		}
		size += len(encoded)
		if size > maxLiveReplayBytes {
			return eventReplay{start: head, after: head}, nil
		}
		replayed = append(replayed, relayedEvent{Event: event, Encoded: encoded})
	}
	return eventReplay{events: replayed, complete: true, start: resume, after: head}, nil
}

// resumeAfterOverflow answers a subscription the relay dropped, holding every event through after:
// a new relay subscription and what it missed, (after, head] of that new subscription, resolved as
// a resume from after (resolveEventReplay). The new subscription is made before the caller cancels
// the dropped one, so the generation runs on between them. false is a catch-up that is not
// complete (past the replay's bounds, or from a log replaced meanwhile) or that failed; the
// subscription then ends, and its page subscribes again from the last id it applied.
func (s *server) resumeAfterOverflow(ctx context.Context, after int64) (relaySubscription, eventReplay, bool) {
	next, err := s.eventRelay.subscribe(ctx, liveEventsBuffer)
	if err != nil {
		return relaySubscription{}, eventReplay{}, false
	}
	caughtUp, err := s.resolveEventReplay(ctx, &after, next.head)
	if err != nil || !caughtUp.complete {
		next.cancel()
		return relaySubscription{}, eventReplay{}, false
	}
	return next, caughtUp, true
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
// replayComplete says its replay covers every event the page missed.
func liveEventsStart(sub string, since int64, replayComplete bool) LiveEventsStart {
	return LiveEventsStart{Type: "events_start", Sub: sub, Since: strconv.FormatInt(since, 10), ReplayComplete: replayComplete}
}

// liveEventFrame is event's frame on sub: its decimal id and its own JSON encoding, byte for byte,
// which is what a page reads every event as. The relay encodes each live event once for every
// subscriber and a replay encodes each missed event once, so a frame sent live and the same frame
// replayed carry the same bytes.
func liveEventFrame(sub string, event relayedEvent) LiveEventFrame {
	return LiveEventFrame{
		Type: "event", Sub: sub, ID: strconv.FormatInt(event.Event.ID, 10), Event: event.Encoded,
	}
}

// encodeLiveEvent is one event's own JSON encoding, which a frame carries whole.
func encodeLiveEvent(event model.Event) ([]byte, error) { return json.Marshal(event) }
