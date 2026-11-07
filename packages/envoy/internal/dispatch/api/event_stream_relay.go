package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

const (
	// eventRelayCoalesce is how long a notification waits before the relay reads what it
	// announced. One transaction commonly commits several events (an issue write and its
	// children's), and the trigger notifies once per statement, so the window collapses a burst
	// into one read instead of one read each. It is 25 ms rather than the second the delivery
	// hub's catch-up waits before a failed read, or the 250 ms it settles a dirty view for
	// (viewQuiet): those feed a dashboard tile, while this is the delay a person watching a page
	// sees between a comment being posted and its frame arriving.
	eventRelayCoalesce = 25 * time.Millisecond
	// eventRelayRetry is how long the relay waits before reading again after a read failed, the
	// delivery hub's retry (catchUp). The notification that woke the failed read does not come
	// again, so nothing but the relay itself will ask for those rows.
	eventRelayRetry = time.Second
	// eventRelayGapMin and eventRelayGapMax bound the re-read a generation schedules while a gap
	// stands open above the safe horizon (Horizon.Pending): it first waits eventRelayGapMin,
	// doubles the wait up to eventRelayGapMax while the horizon stays where it was, and starts
	// again from eventRelayGapMin each time the horizon moves. A commit's notification can wake
	// the read before the horizon covers that commit: only a later observation proves an earlier
	// one safe, and nothing notifies again once the open lower ids have ended, so without this
	// re-read an event the read held back would wait for the next unrelated commit.
	eventRelayGapMin = 10 * time.Millisecond
	eventRelayGapMax = 250 * time.Millisecond
)

// EventsChannel carries no payload and is notified as an event commit announces itself
// (events_notify, migration 0107), whichever process committed it. Of the live paths the relay
// alone listens on it, while something subscribes to it, and the delivery hub takes the events
// from the relay's feed; it is also the channel whose pglisten.Hub wake cmd/dispatch passes to
// RunSuggestionOutcomeSweep.
const EventsChannel = "dispatch_events"

// eventGapReread schedules a generation's re-read while a gap stands open above the safe horizon
// (run): timer is the scheduled re-read, nil while none is; wait the current delay, zero while no
// gap is open; and safe the horizon the read that scheduled it read through. Its one generation's
// read goroutine alone touches it, and receives the re-read from due.
type eventGapReread struct {
	timer *time.Timer
	wait  time.Duration
	safe  int64
}

// schedule replaces the scheduled re-read, if any, with the one a read through safe needs: none
// when it left no gap open (gap false), otherwise after eventRelayGapMin for the first gap or a
// horizon that moved, or after the last delay doubled up to eventRelayGapMax.
func (g *eventGapReread) schedule(safe int64, gap bool) {
	g.stop()
	switch {
	case !gap:
		g.wait = 0
		return
	case g.wait == 0 || safe != g.safe:
		g.wait = eventRelayGapMin
	default:
		g.wait = min(2*g.wait, eventRelayGapMax)
	}
	g.safe = safe
	g.timer = time.NewTimer(g.wait)
}

// stop drops the scheduled re-read, if any; a stopped timer delivers nothing on due afterwards.
func (g *eventGapReread) stop() {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// due delivers the scheduled re-read once its delay has passed; nil, which never delivers, while
// none is scheduled.
func (g *eventGapReread) due() <-chan time.Time {
	if g.timer == nil {
		return nil
	}
	return g.timer.C
}

// relayedEvent is one committed event as the relay publishes it: the event and its own JSON
// encoding, made once for every subscriber (encodeLiveEvent). Every subscriber receives the same
// event and the same bytes, so none of them modifies either.
type relayedEvent struct {
	Event   model.Event
	Encoded []byte
}

// eventStreamRelay is the live source of every events subscription on this process (the event
// feed, live_events.go) and of the delivery hub's intake: while anyone subscribes, it holds a
// `listen dispatch_events` connection of its own (migration 0107 notifies on each commit) and
// publishes every committed event into its stream, whichever process committed it. While nobody
// subscribes it holds no connection and reads nothing.
//
// Two envoy-dispatch processes serve one Postgres. The broker's fan-out reaches only the process
// that appended the event, so a page whose socket landed on the other one would see nothing of it
// until it subscribed again. Re-sourcing the live feed from the database carries both processes'
// events on the one path, and makes the live frame and the replayed frame the same bytes: both
// carry encodeLiveEvent of an event read through readEventRows, with an ask event's read-time
// fields attached.
//
// Each run of the relay is a generation (relayGeneration): the first subscription starts one, and
// the cancellation of its last subscription stops it, at once, with no grace period. A stopped
// generation's goroutines end with its context; the next subscription starts a new generation,
// which reads its first cursor afresh (firstRelayCursor).
type eventStreamRelay struct {
	server   *server
	lifetime context.Context

	// dial, listenIdle, pingTimeout and backoff are the listener's (pglisten.Listener). Nil dial
	// is the shared pool's Listener, taken when a generation starts rather than when the relay is
	// built: Register mounts a relay on every server, a storeless one included (the route table's).
	dial                    func(ctx context.Context) (*pgx.Conn, error)
	listenIdle, pingTimeout time.Duration
	backoff                 func(attempt int) time.Duration
	// coalesce is the window a notification waits before its read; zero is eventRelayCoalesce.
	coalesce time.Duration
	// readAfter reads one page of the committed events in (after, through]; nil is
	// readEventsAfterID. A test counts the relay's reads of the event log through it.
	readAfter func(ctx context.Context, after, through int64) ([]model.Event, error)

	// running counts every generation's goroutines. Add runs under mu and only while closed is
	// false, so no Add can follow the Wait in wait.
	running sync.WaitGroup

	// mu guards closed, starting, generation, and the running generation's cursor and subscriber
	// count. It is held across publishing a page and moving the cursor over it, and across a new
	// subscription and its read of that cursor (subscribe), so the cursor a subscriber reads
	// covers exactly the events published before its subscription existed.
	mu sync.Mutex
	// closed is set by wait: the lifetime has ended and no generation starts again.
	closed bool
	// starting is open while one caller runs a generation's first cursor read, which runs without
	// mu so that no other caller's subscribe waits on the database while holding it: that read
	// waits for a safe horizon, which any open write on the instance can hold back.
	starting chan struct{}
	// generation is the running generation, nil while nobody subscribes.
	generation *relayGeneration
}

// relayGeneration is one run of the relay. Its goroutines run on a context of their own, which
// stop cancels; stream, wake and stop are set once at its start, and the rest is guarded by the
// relay's mu.
//
// cursor is the id the generation has published through, and cursorAt the created_at of the event
// it stands at. Event ids are not commit order: writes to different owners take no lock in common
// (events.Broker.Append), so a lower id can still be open after a higher one has committed, and a
// read of `id > cursor` that moved its cursor past the higher one would never publish the lower.
// A generation therefore reads only up to the safe horizon H of the server's events.Horizon, at or
// below which every event is final, and keeps its cursor on the last existing row at or below an
// H, never above one: everything at or below the cursor is final and was published, in id order,
// and everything above it is yet to be. Its first cursor is firstRelayCursor's, which leaves
// nothing that commits after the subscription that started the generation at or below it. The
// cursor may lag the database — a notification not yet read, a gap not yet closed, a connection
// being redialled — and never runs ahead of the horizon.
//
// cursorAt is what tells an event log that was reset from one that has merely gone quiet: see
// resetCursor. The cursor is always an existing row's id (or 0 before the first), so the row at it
// carries that created_at until the log is reset.
type relayGeneration struct {
	stop context.CancelFunc
	// ended is the generation context's Done: closed once the generation has stopped.
	ended  <-chan struct{}
	stream *events.Stream[relayedEvent]
	// wake holds at most one signal: what a read answers covers every notification before it.
	wake chan struct{}

	cursor      int64
	cursorAt    time.Time
	subscribers int
}

// newEventStreamRelay is s's relay. Nothing runs and no connection is held until the first
// subscription starts a generation on lifetime.
func newEventStreamRelay(s *server, lifetime context.Context) *eventStreamRelay {
	return &eventStreamRelay{server: s, lifetime: lifetime}
}

// relaySubscription is one consumer's live feed from the relay (subscribe).
type relaySubscription struct {
	// events carries every event above head, in id order, until it closes. It closes for one of
	// two reasons: the consumer fell its buffer behind (Stream.Publish drops it) while its
	// generation runs on, or its generation ended (a reset, the lifetime).
	events <-chan relayedEvent
	// ended is closed once the subscription's generation has ended, and always before that end
	// closes events (endLocked stops the generation, then closes its subscriptions): events closed
	// while ended is still open is an overflow.
	ended <-chan struct{}
	// head is the id the generation had published through when the subscription was made, and
	// headAt the created_at of the event it stands on. Every event at or below head is final, so
	// the consumer reads those from the log itself; every event above it arrives here.
	head   int64
	headAt time.Time
	// wake asks the generation to read now, which is also how a consumer that finds the relay
	// standing on a replaced log's cursor has its reset check run (feedEvents).
	wake func()
	// cancel ends the subscription; it is idempotent.
	cancel func()
}

// subscribe answers one consumer its live feed on a channel of buffer slots (relaySubscription).
//
// The subscription, the head and the start of a generation when none runs happen under mu, which
// the read loop also holds across publishing a page and moving the cursor over it: a page is
// either published into this subscription or already at or below its head, never neither.
//
// A generation's first cursor read (firstRelayCursor) waits for a safe horizon, which an open
// write anywhere on the instance can hold back, so it runs outside mu: the caller running it marks
// starting, and every other caller waits on starting (or its own ctx) rather than on mu or a spin.
// A read that fails starts nothing and is the caller's error; the next subscription tries again.
// Once the lifetime has ended, or a read ends after it has, subscribe refuses.
//
// Cancelling the last subscription of the running generation stops that generation: its listening
// connection closes and it reads nothing more. An overflowed subscription still counts until it is
// cancelled, so a consumer that subscribes again before cancelling it keeps the generation running.
func (r *eventStreamRelay) subscribe(ctx context.Context, buffer int) (relaySubscription, error) {
	for {
		r.mu.Lock()
		if r.closed || r.lifetime.Err() != nil {
			r.mu.Unlock()
			return relaySubscription{}, errorf(http.StatusServiceUnavailable, "SHUTTING_DOWN", "the server is shutting down")
		}
		if g := r.generation; g != nil {
			defer r.mu.Unlock()
			return r.joinLocked(g, buffer), nil
		}
		if starting := r.starting; starting != nil {
			r.mu.Unlock()
			select {
			case <-starting:
				continue
			case <-ctx.Done():
				return relaySubscription{}, fmt.Errorf("wait for the event stream relay to start: %w", ctx.Err())
			}
		}
		starting := make(chan struct{})
		r.starting = starting
		r.mu.Unlock()

		cursor, cursorAt, err := r.server.firstRelayCursor(ctx)

		r.mu.Lock()
		r.starting = nil
		close(starting)
		if err != nil {
			r.mu.Unlock()
			return relaySubscription{}, fmt.Errorf("read the event stream relay's cursor: %w", err)
		}
		if r.closed || r.lifetime.Err() != nil {
			r.mu.Unlock()
			return relaySubscription{}, errorf(http.StatusServiceUnavailable, "SHUTTING_DOWN", "the server is shutting down")
		}
		g := r.startLocked(cursor, cursorAt)
		subscription := r.joinLocked(g, buffer)
		r.mu.Unlock()
		return subscription, nil
	}
}

// joinLocked gives one consumer its feed on g, with mu held.
func (r *eventStreamRelay) joinLocked(g *relayGeneration, buffer int) relaySubscription {
	feed, unsubscribe := g.stream.Subscribe(buffer)
	g.subscribers++
	cancel := sync.OnceFunc(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		unsubscribe()
		g.subscribers--
		if g.subscribers == 0 && r.generation == g {
			r.generation = nil
			g.stop()
		}
	})
	return relaySubscription{
		events: feed, ended: g.ended, head: g.cursor, headAt: g.cursorAt,
		wake: func() { signal(g.wake) }, cancel: cancel,
	}
}

// startLocked runs a new generation's listener and read loop from the cursor its start read, with
// mu held.
func (r *eventStreamRelay) startLocked(cursor int64, cursorAt time.Time) *relayGeneration {
	runCtx, stop := context.WithCancel(r.lifetime)
	g := &relayGeneration{
		stop:     stop,
		ended:    runCtx.Done(),
		stream:   events.NewStream[relayedEvent](),
		wake:     make(chan struct{}, 1),
		cursor:   cursor,
		cursorAt: cursorAt,
	}
	dial := r.dial
	if dial == nil {
		dial = r.server.deps.Store.Pool.Listener
	}
	listener := &pglisten.Listener{
		Owner:       "dispatch",
		Dial:        dial,
		Channels:    []string{EventsChannel},
		ListenIdle:  r.listenIdle,
		PingTimeout: r.pingTimeout,
		Backoff:     r.backoff,
		OnConnected: g.onConnected,
		OnNotify:    g.onNotify,
	}
	r.running.Add(2)
	go func() { defer r.running.Done(); listener.Run(runCtx) }()
	go func() { defer r.running.Done(); r.run(runCtx, g) }()
	r.generation = g
	return g
}

// wait marks the relay closed, so no generation starts again, then returns once every
// generation's goroutines have. It is called after the lifetime ended, which ends them all. It
// takes mu once before its Wait, so a start still finishing has either counted its goroutines by
// then or will see the relay closed and start none (subscribe).
func (r *eventStreamRelay) wait() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.running.Wait()
}

// endLocked stops g and closes every subscription it holds, with mu held. Those subscribers'
// cancels then find g ended and touch nothing else.
func (r *eventStreamRelay) endLocked(g *relayGeneration) {
	if r.generation == g {
		r.generation = nil
	}
	g.stop()
	g.stream.CloseAll()
}

// onNotify wakes a read for one event commit, on the listener's goroutine. The relay's connection
// listens on dispatch_events alone: the delivery hub's other two channels say nothing about the
// event log, and a relay that heard them would read the log for every settings write.
func (g *relayGeneration) onNotify(_, _ string) { signal(g.wake) }

// onConnected catches up from the cursor after each LISTEN succeeds, which is what covers the
// notifications sent while the generation had no connection, the commits between its cursor read
// and its first LISTEN among them. It moves the cursor to no head of its own: a redial reads from
// where the generation published through, so nothing committed in that window is skipped.
func (g *relayGeneration) onConnected() { signal(g.wake) }

// run publishes what each wake announced, until ctx ends. A wake waits the coalescing window
// before its read; a notification arriving during a read leaves the one signal the channel holds,
// which becomes exactly one more read after it.
//
// A read that leaves a gap open above the horizon (an observed id the horizon does not yet cover)
// schedules a re-read of its own, eventRelayGapMin doubling to eventRelayGapMax while the horizon
// stands still and back to eventRelayGapMin whenever it moves. The timer runs only while a gap is
// open, so an idle generation observes nothing; a notification arriving meanwhile reads at once, as
// ever. A gap re-read skips the reset check: it asks only whether the horizon has moved, on the
// horizon's own connection, and reads the shared pool only once there is something to publish,
// so a gap held open by a long transaction costs no request a connection. A reset is still caught
// at the next notification, which the refill's own inserts send.
func (r *eventStreamRelay) run(ctx context.Context, g *relayGeneration) {
	var gaps eventGapReread
	defer gaps.stop()
	for {
		checkReset := true
		select {
		case <-ctx.Done():
			return
		case <-g.wake:
			if !waitFor(ctx, r.window()) {
				return
			}
		case <-gaps.due():
			checkReset = false
		}
		gaps.stop()
		safe, gap, ok := r.read(ctx, g, checkReset)
		for !ok {
			if !waitFor(ctx, eventRelayRetry) {
				return
			}
			safe, gap, ok = r.read(ctx, g, checkReset)
		}
		gaps.schedule(safe, gap)
	}
}

// read publishes every event after g's cursor and at or below the safe horizon, in id order, each
// with its frame encoded once, and moves the cursor over each page as it publishes it. It holds mu
// across publishing a page and moving the cursor, so a subscription made meanwhile either receives
// the page or has a head past it. Nothing but this goroutine writes g's cursor, and a generation
// stopped while its read was in flight publishes only into its own stream, which has no subscriber
// left. It pages until a short page or until the cursor stands on the horizon, all through the one
// horizon it read first: what commits meanwhile is the next read's. A log that was reset under the
// cursor is answered first, and publishes nothing (resetCursor); checkReset false skips that check
// (run's gap re-read).
//
// It answers the horizon it read through and whether a gap stands open above it (Pending): an
// event observed above the horizon that this read could not publish, which run re-reads for.
// ok false is a read that failed.
func (r *eventStreamRelay) read(ctx context.Context, g *relayGeneration, checkReset bool) (safe int64, gap, ok bool) {
	tracked := store.WithTransactionTracking(ctx)
	horizon := r.server.horizon()
	if checkReset {
		switch reset, err := r.resetCursor(tracked, g); {
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("dispatch: the event stream relay could not check its cursor against the event log; retrying", "error", err)
			}
			return 0, false, false
		case reset:
			return 0, false, true
		}
	}
	safe, err := horizon.Safe(tracked)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: the event stream relay could not observe the event log's safe horizon; retrying", "error", err)
		}
		return 0, false, false
	}
	for {
		cursor, _ := r.at(g)
		if cursor >= safe {
			return safe, horizon.Pending(), true
		}
		committed, err := r.page(tracked, cursor, safe)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("dispatch: the event stream relay could not read the committed events; retrying", "error", err)
			}
			return 0, false, false
		}
		relayed := make([]relayedEvent, len(committed))
		for index, event := range committed {
			encoded, err := encodeLiveEvent(event)
			if err != nil {
				slog.Warn("dispatch: the event stream relay could not encode a committed event; retrying", "id", event.ID, "error", err)
				return 0, false, false
			}
			relayed[index] = relayedEvent{Event: event, Encoded: encoded}
		}
		r.publish(g, relayed)
		if len(committed) < maxEventReplay {
			return safe, horizon.Pending(), true
		}
	}
}

// page reads the committed events in (after, through]: the relay's one read of the event log's
// rows, the read an events subscription's catch-up also pages through (readEventsAfterID).
func (r *eventStreamRelay) page(ctx context.Context, after, through int64) ([]model.Event, error) {
	if r.readAfter != nil {
		return r.readAfter(ctx, after, through)
	}
	return r.server.readEventsAfterID(ctx, after, through, maxEventReplay)
}

// publish fans one page out to g's subscribers and moves g's cursor to its last event, under mu.
func (r *eventStreamRelay) publish(g *relayGeneration, page []relayedEvent) {
	if len(page) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, event := range page {
		g.stream.Publish(event)
	}
	last := page[len(page)-1].Event
	g.cursor, g.cursorAt = last.ID, last.CreatedAt
}

// resetCursor tells an event log that was reset under g's cursor from one that has merely gone
// quiet, and answers a reset by forgetting the horizon the old log proved and ending g with every
// subscription it holds. The check reads eventCursorCheck and eventLogWasReset, which the events
// subscriptions share: a log whose newest id is below the cursor, or whose row at the cursor is
// gone or carries another created_at — a `truncate events restart identity` refilled past it,
// which is what e2e/seed.ts does between browser scenarios — is a log the cursor no longer names a
// position in. Nothing of the first kind of reset would ever be published: the new log's ids start
// again at 1, far below the cursor, so `id > cursor` matches no row until they have climbed back
// past it, and the stream falls silent for every reader.
//
// The horizon is the old log's too: its H stands among the old ids, above every id the new log has
// handed out, so read as it stands it would cap nothing until the new log climbed past it, and a
// cursor taken under it could step over a lower id the refill still holds open. Forgetting it
// (resetEventHorizon) is what makes the next generation's first cursor the last event at or below
// a horizon proven after the reset, which waits for every write open at the reset to end.
//
// A reset is the one thing the relay cannot publish its way out of, because its ids have been
// reused: the rows it would read as `id > cursor` are not the ones it published, and every open
// subscription's since (live_events.go) is a position in a log that no longer exists, so no frame
// it sent could mean what its reader would take it to mean. Ending the subscriptions is the
// honest answer: the generation ends, so each events subscription ends with an ended frame
// (behind) rather than catching up as it does after an overflow, and its page subscribes again
// from the last id it applied, a since the new subscription clamps to the log as it now stands.
//
// The delivery hub, one of those subscriptions, answers the closed feed by subscribing again and
// marking every view dirty rather than by dropping anything, because it holds a projection of its
// own that it can recompute from the database; an events subscription holds nothing but a
// reader's feed, which only that reader can rebuild.
func (r *eventStreamRelay) resetCursor(ctx context.Context, g *relayGeneration) (bool, error) {
	cursor, at := r.at(g)
	maxID, cursorAt, err := eventCursorCheck(ctx, r.server.deps.Store.Pool, cursor)
	if err != nil {
		return false, err
	}
	if !eventLogWasReset(cursor, at, maxID, cursorAt) {
		return false, nil
	}
	r.server.resetEventHorizon()
	r.mu.Lock()
	r.endLocked(g)
	r.mu.Unlock()
	slog.Warn("dispatch: the event log was reset; the event stream relay forgot its horizon and ended its open subscriptions",
		"cursor", cursor, "newest", maxID)
	return true, nil
}

// at is the id g has published through and that event's created_at.
func (r *eventStreamRelay) at(g *relayGeneration) (int64, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return g.cursor, g.cursorAt
}

func (r *eventStreamRelay) window() time.Duration {
	if r.coalesce == 0 {
		return eventRelayCoalesce
	}
	return r.coalesce
}

// firstRelayCursor is a generation's first cursor, which is the start point of the subscription
// that starts it, so no event that commits after that subscription began may stand at or below it.
// It reads the log as it stands at the start, in one statement: its newest id and every gap among
// its ids above a floor at or below which no event can still be open (firstRelayFloor). An event
// that commits after that read stands above the newest id, or in one of those gaps: an id it drew
// before the read, while its transaction was still open. A gap can also be an id whose transaction
// rolled back. Below the floor no gap can fill.
//
// It then waits for a horizon proven after the read (Horizon.Await), past which every id at or
// below that newest one is final, and reads which of the gaps have filled since: those are events
// that committed after the start began. The cursor is the last event below the lowest of them, or
// at the newest id when none filled, so the first subscription is sent every event that committed
// after the read and none committed before it, but those between the lowest late one and the
// newest id. A gap still empty then is a rollback, and moves the cursor nowhere.
//
// The wait is the start's: as long as any transaction open on the instance holds the horizon back,
// with the subscription's events_start not yet sent (feedEvents). The gap read covers the ids above
// the floor, which are the ids written since a recent mark, however long the log. An event that
// commits between the subscription's start and this read, a round trip apart, counts as committed
// before it.
func (s *server) firstRelayCursor(ctx context.Context) (int64, time.Time, error) {
	pool := s.deps.Store.Pool
	floor, err := s.firstRelayFloor(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	newest, gaps, err := eventLogGapsAbove(ctx, pool, floor)
	if err != nil {
		return 0, time.Time{}, err
	}
	if _, err := s.horizon().Await(ctx); err != nil {
		return 0, time.Time{}, err
	}
	through := newest
	if len(gaps.from) > 0 {
		late, err := firstEventInGaps(ctx, pool, gaps)
		if err != nil {
			return 0, time.Time{}, err
		}
		if late > 0 {
			through = late - 1
		}
	}
	return lastEventThrough(ctx, pool, through)
}

// firstRelayFloor is the id a generation's start reads the log's gaps above (firstRelayCursor):
// one at or below which no event can still be open, so no gap there can fill (server.writerFloor:
// the newest of the server's marks that counts, or else the horizon already proven or persisted).
// A mark counts once every transaction that holds a write lock on events took its transaction id
// after the mark, so the floor is the newest mark that counts, and the read covers only the ids
// written since it, whatever the size of the log. A transaction that holds a transaction id but
// has inserted no event (a read in a transaction, a write to another table, another database's
// job) holds no event id and keeps no mark from counting.
//
// The floor is read once, when the start begins, with the newest id and the gaps above it, so
// everything that commits after the start began stands above the newest id or in one of those
// gaps. When no mark counts, an event insert of this database has been open since before every
// mark kept, the boot mark included, and may hold an id below all of them; no later mark could
// bound it without skipping what commits meanwhile. The floor is then the horizon this process
// proved or the newest one any process persisted (events.Horizon.Persisted), every id at or below
// which is final: on a deploy, the horizon the process before it proved before that insert began.
// Only with neither, a first boot or a log reset, does the read cover the whole log, which is
// logged.
func (s *server) firstRelayFloor(ctx context.Context) (int64, error) {
	if s.bootMark != nil {
		select {
		case <-s.bootMark:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	floor, ok, _, err := s.writerFloor(ctx)
	if err != nil {
		return 0, err
	}
	if !ok && floor == 0 {
		slog.Warn("dispatch: the event stream relay's start reads the whole event log: an event insert of this database has been open since before every mark it keeps, and no horizon is proven or persisted")
	}
	return floor, nil
}

// takeBootMark takes the server's boot mark (eventMarks' boot) on a goroutine of its own, so
// register, which must not touch the Store, returns at once; a server with no Store pool takes
// none. It is one statement that waits on no transaction; bootMark closes once it has run, and a
// mark that fails leaves none, so the marks the readers take themselves are all there are.
func (s *server) takeBootMark() {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return
	}
	s.bootMark = make(chan struct{})
	go func() {
		defer close(s.bootMark)
		mark, err := s.horizon().Mark(s.lifetime)
		if err != nil {
			if s.lifetime.Err() == nil {
				slog.Warn("dispatch: could not take the event log's boot mark; readers of the event log mark it themselves", "error", err)
			}
			return
		}
		s.eventMarks.setBoot(mark)
	}()
}

// eventLogGaps are runs of ids missing from the event log, from[i] through to[i].
type eventLogGaps struct {
	from, to []int64
}

// eventLogGapsAbove reads, in one statement and so one snapshot, the event log's newest id and
// every run of ids missing from it above floor and below that newest id. It reads the ids above
// floor, in id order, through the primary key, and so as many as were written since floor. A
// floor above the newest id is a horizon of a log that was reset since it was proven, which says
// nothing of the new one, so the read is made again from 0.
func eventLogGapsAbove(ctx context.Context, q queryer, floor int64) (int64, eventLogGaps, error) {
	var newest int64
	var gaps eventLogGaps
	err := q.QueryRow(ctx, `
		with ids as (
			select id, coalesce(lag(id) over (order by id), $1) as prev from events where id > $1
		)
		select (select coalesce(max(id), 0) from events),
			coalesce(array_agg(prev + 1 order by id) filter (where id > prev + 1), '{}'),
			coalesce(array_agg(id - 1 order by id) filter (where id > prev + 1), '{}')
		from ids
	`, floor).Scan(&newest, &gaps.from, &gaps.to)
	if err == nil && floor > newest {
		return eventLogGapsAbove(ctx, q, 0)
	}
	return newest, gaps, err
}

// firstEventInGaps is the lowest id of an event that now stands in one of gaps, 0 when none does.
func firstEventInGaps(ctx context.Context, q queryer, gaps eventLogGaps) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		select coalesce(min(e.id), 0)
		from unnest($1::bigint[], $2::bigint[]) as gap(first, last)
		join events e on e.id between gap.first and gap.last
	`, gaps.from, gaps.to).Scan(&id)
	return id, err
}

// horizon is the server's one events.Horizon (eventHorizon), built by the first reader that asks
// for it and observing through the store's horizon pool (store.Pool.Horizon), so an observation
// never queues behind the requests holding the shared pool. Every global-id reader of the event
// log on this server reads through it: one per process is what lets concurrent readers share an
// observation, each of which consumes a transaction id, rather than each reader observing on its
// own.
//
// The horizon pool opens only on a store that is still open. A closed store's Horizon observes
// through the shared pool, which is closed too, so every observation fails with its error: that
// fallback says nothing worth logging. Any other failure to open the pool also falls back to the
// shared pool, so the process still serves its live readers, but now their observations queue
// behind the API's requests, which is what the horizon pool exists to prevent, and horizonOnce
// keeps that choice for the process's lifetime; so it is logged, once, with its cause.
func (s *server) horizon() *events.Horizon {
	s.horizonOnce.Do(func() {
		var db events.HorizonDB = s.deps.Store.Pool
		switch pool, err := s.deps.Store.Pool.Horizon(); {
		case err == nil:
			db = pool
		case !errors.Is(err, store.ErrPoolClosed):
			slog.Error("dispatch: could not open the event horizon's pool; the event log's live readers observe through the shared request pool, behind the API's requests, until the process restarts",
				"error", err)
		}
		s.eventHorizon = events.NewHorizon(db)
		s.eventHorizon.PersistProven()
	})
	return s.eventHorizon
}

// resetEventHorizon forgets what this process proved of an event log that was reset under its
// readers (eventLogWasReset): the horizon (Horizon.Reset) and the marks, each of which names a
// position in a log that no longer exists. The next generation's first cursor is then taken under
// a horizon proven after the reset, which waits for every write open at the reset to end. The
// relay's reset check is its one caller (resetCursor); the delivery hub, which reads no event log
// of its own, answers the same reset by recomputing its views.
func (s *server) resetEventHorizon() {
	s.horizon().Reset()
	s.eventMarks.forget()
}

// lastEventThrough answers the newest event whose id is at or below through, with its
// created_at: 0 and the zero time when there is none.
func lastEventThrough(ctx context.Context, q queryer, through int64) (int64, time.Time, error) {
	var id int64
	var at time.Time
	err := q.QueryRow(ctx, `select id, created_at from events where id <= $1 order by id desc limit 1`, through).Scan(&id, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	return id, at, err
}

// eventCursorCheck reads the newest event's id (0 when the log is empty) and the created_at of the
// event at cursor (nil when no row holds it): the event stream relay (resetCursor) and an events
// subscription (feedEvents) compare them with what they read for their cursor to tell an event log
// that was reset (eventLogWasReset).
//
// Both come from one statement, so they describe one log. A `truncate events restart identity`
// (e2e/seed.ts) takes an ACCESS EXCLUSIVE lock, and a statement holds its ACCESS SHARE lock on
// events until it ends, so the reset lands wholly before or wholly after this read. Read as two
// statements, a reset committing between them answered with the old log's newest id and no row at
// the cursor: the relay then took the old log's head for the new log's and ended the generation
// opened after the reset as a second reset.
func eventCursorCheck(ctx context.Context, q queryer, cursor int64) (maxID int64, cursorAt *time.Time, err error) {
	if err := q.QueryRow(ctx, `
		select coalesce((select max(id) from events), 0), (select created_at from events where id = $1)
	`, cursor).Scan(&maxID, &cursorAt); err != nil {
		return 0, nil, err
	}
	return maxID, cursorAt, nil
}

// eventLogWasReset is whether the event log was reset under a reader's cursor, from what
// eventCursorCheck read for it (maxID, cursorAt) and what the reader read when it moved there (at):
// a log whose newest id is below the cursor, or whose row at the cursor is gone or carries another
// created_at — a `truncate events restart identity` refilled past it, which is what e2e/seed.ts:20
// and :150 do between browser scenarios. The cursor is always an existing row's id, or 0 before the
// first, so until a reset the row at it carries the created_at the reader read.
func eventLogWasReset(cursor int64, at time.Time, maxID int64, cursorAt *time.Time) bool {
	return maxID < cursor || (cursor > 0 && (cursorAt == nil || !cursorAt.Equal(at)))
}

// waitFor waits for d, or until ctx ends; false means ctx ended.
func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
