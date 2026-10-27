package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// eventRelayGapMin and eventRelayGapMax bound the re-read the relay schedules while a gap
	// stands open above the safe horizon (Horizon.Pending): it first waits eventRelayGapMin,
	// doubles the wait up to eventRelayGapMax while the horizon stays where it was, and starts
	// again from eventRelayGapMin each time the horizon moves. A commit's notification can wake
	// the read before the horizon covers that commit: only a later observation proves an earlier
	// one safe, and nothing notifies again once the open lower ids have ended, so without this
	// re-read an event the read held back would wait for the next unrelated commit.
	eventRelayGapMin = 10 * time.Millisecond
	eventRelayGapMax = 250 * time.Millisecond
)

// eventStreamRelay is the live source of every open SSE connection on this process: it holds a
// `listen dispatch_events` connection of its own (migration 0107 notifies on each commit) and
// publishes every committed event into its events.Stream, whichever process committed it.
//
// Two envoy-dispatch processes serve one Postgres. The broker's fan-out reaches only the process
// that appended the event, so a reader whose connection landed on the other one would see nothing
// of it until it reconnected. Re-sourcing the live stream from the database carries both
// processes' events on the one path, and makes the live frame and the replayed frame the same
// bytes: both come from readEventsAfterID, with an ask event's read-time fields attached.
//
// cursor is the id the relay has published through, and cursorAt the created_at of the event it
// stands at. Event ids are not commit order: writes to different owners take no lock in common
// (events.Broker.Append), so a lower id can still be open after a higher one has committed, and
// a read of `id > cursor` that moved its cursor past the higher one would never publish the
// lower. The relay therefore reads only up to the safe horizon H of the server's events.Horizon,
// at or below which every event is final, and keeps its cursor on the last existing row at or
// below an H, never above one: everything at or below the cursor is final and was published, in
// id order, and everything above it is yet to be. The cursor may lag the database — a
// notification not yet read, a gap not yet closed, a connection being redialled — and never runs
// ahead of the horizon.
//
// cursorAt is what tells an event log that was reset from one that has merely gone quiet: see
// resetCursor. The cursor is always an existing row's id (or 0 before the first), so the row at
// it carries that created_at until the log is reset.
type eventStreamRelay struct {
	server   *server
	lifetime context.Context
	// stream is the reader's live feed; nothing but this relay publishes into it.
	stream *events.Stream

	// dial, listenIdle, pingTimeout and backoff are the listener's (pglisten.Listener). Nil dial
	// is the shared pool's Listener, taken when the relay starts rather than when it is built:
	// Register mounts a relay on every server, a storeless one included (the route table's).
	dial                    func(ctx context.Context) (*pgx.Conn, error)
	listenIdle, pingTimeout time.Duration
	backoff                 func(attempt int) time.Duration
	// coalesce is the window a notification waits before its read; zero is eventRelayCoalesce.
	coalesce time.Duration
	// beforeAdvance, when a test sets it before the relay starts, runs after each page is
	// published and before the cursor moves over it, with mu held: the one moment a subscriber
	// must not be let in (publish). Nil outside tests.
	beforeAdvance func(page []model.Event)

	running sync.WaitGroup
	// wake holds at most one signal: what a read answers covers every notification before it.
	wake chan struct{}

	// mu guards started, starting, cursor and cursorAt. It is held across each page's publish and
	// the cursor's move over that page, and across a new subscription and its read of the cursor
	// (subscribe), so the cursor a subscriber reads covers exactly the events published before
	// its subscription existed. starting is open while one caller runs start's first cursor read,
	// which runs without mu so that no other caller's subscribe waits on the database while
	// holding it.
	mu       sync.Mutex
	started  bool
	starting chan struct{}
	cursor   int64
	cursorAt time.Time
}

// newEventStreamRelay is s's relay. Nothing runs and no connection is held until the first
// subscription starts it on lifetime.
func newEventStreamRelay(s *server, lifetime context.Context) *eventStreamRelay {
	return &eventStreamRelay{
		server:   s,
		lifetime: lifetime,
		stream:   events.NewStream(),
		wake:     make(chan struct{}, 1),
	}
}

// subscribe answers one open SSE connection its live feed and the id the relay had published
// through when that feed began, starting the relay on the first connection: a process nobody
// streams from opens no listening connection of its own.
//
// The subscription and the cursor are taken under the one hold of mu that every page's publish
// and cursor move also take, so they describe the same moment: every event at or below the
// cursor was published before the subscription existed (it is final, and the caller replays it
// from the log), and every event above it will be published into the subscription, in strictly
// increasing id order. Read apart, a page published between the two would reach the caller twice
// or, read in the other order, not at all. A start that fails starts nothing and is the caller's
// error; the next connection tries again.
func (r *eventStreamRelay) subscribe(ctx context.Context) (<-chan model.Event, func(), int64, error) {
	if err := r.start(ctx); err != nil {
		return nil, nil, 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	subscription, cancel := r.stream.Subscribe()
	return subscription, cancel, r.cursor, nil
}

// start reads the first cursor and runs the relay's two goroutines, once. The first cursor is the
// last event at or below the first horizon proven safe after the call (safeCursor), so it covers
// every event committed before the first connection subscribed and stands on a row whose
// created_at the reset check can compare.
//
// That read waits for a safe observation, which an open write anywhere on the instance can hold
// back, so it runs outside mu: the caller running it marks starting, and every other caller
// waits on starting (or its own ctx) rather than on mu or a spin. A read that fails, ctx's end
// included, leaves the relay unstarted, and the next caller runs the read again.
func (r *eventStreamRelay) start(ctx context.Context) error {
	for {
		r.mu.Lock()
		if r.started {
			r.mu.Unlock()
			return nil
		}
		if starting := r.starting; starting != nil {
			r.mu.Unlock()
			select {
			case <-starting:
				continue
			case <-ctx.Done():
				return fmt.Errorf("wait for the live event stream to start: %w", ctx.Err())
			}
		}
		starting := make(chan struct{})
		r.starting = starting
		r.mu.Unlock()

		cursor, cursorAt, err := r.safeCursor(ctx)

		r.mu.Lock()
		r.starting = nil
		close(starting)
		if err != nil {
			r.mu.Unlock()
			return fmt.Errorf("read the live event stream's cursor: %w", err)
		}
		r.cursor, r.cursorAt, r.started = cursor, cursorAt, true
		r.running.Add(2)
		r.mu.Unlock()

		dial := r.dial
		if dial == nil {
			dial = r.server.deps.Store.Pool.Listener
		}
		listener := &pglisten.Listener{
			Owner:       "dispatch",
			Dial:        dial,
			Channels:    []string{dispatchEventsChannel},
			ListenIdle:  r.listenIdle,
			PingTimeout: r.pingTimeout,
			Backoff:     r.backoff,
			OnConnected: r.onConnected,
			OnNotify:    r.onNotify,
		}
		go func() { defer r.running.Done(); listener.Run(r.lifetime) }()
		go func() { defer r.running.Done(); r.run(r.lifetime) }()
		return nil
	}
}

// safeCursor waits for a horizon proven safe by an observation taken during the call
// (Horizon.Await), which is at or above every id committed before the call, and answers the last
// event at or below it with that event's created_at: 0 and the zero time when there is none. It
// returns early only when ctx ends.
func (r *eventStreamRelay) safeCursor(ctx context.Context) (int64, time.Time, error) {
	safe, err := r.server.horizon().Await(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	return lastEventThrough(ctx, r.server.deps.Store.Pool, safe)
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

// wait returns once the relay's goroutines have, after its lifetime ended. A relay no
// subscription ever started has none to wait for.
func (r *eventStreamRelay) wait() { r.running.Wait() }

// onNotify wakes a read for one event commit, on the listener's goroutine. The relay's connection
// listens on dispatch_events alone: the delivery hub's other two channels say nothing about the
// event log, and a relay that heard them would read the log for every settings write.
func (r *eventStreamRelay) onNotify(_, _ string) { signal(r.wake) }

// onConnected catches up from the cursor after each LISTEN succeeds, which is what covers the
// notifications sent while the relay had no connection. It moves the cursor to no head of its
// own: a redial reads from where the relay published through, so nothing committed in that window
// is skipped. Only a log that was reset under the cursor moves it without publishing, and only
// because the rows it would have published are not the ones it stands for (resetCursor).
func (r *eventStreamRelay) onConnected() { signal(r.wake) }

// run publishes what each wake announced, until ctx ends. A wake waits the coalescing window
// before its read; a notification arriving during a read leaves the one signal the channel holds,
// which becomes exactly one more read after it.
//
// A read that leaves a gap open above the horizon (an observed id the horizon does not yet cover)
// schedules a re-read of its own, eventRelayGapMin doubling to eventRelayGapMax while the horizon
// stands still and back to eventRelayGapMin whenever it moves. The timer runs only while a gap is
// open, so an idle relay observes nothing; a notification arriving meanwhile reads at once, as
// ever.
func (r *eventStreamRelay) run(ctx context.Context) {
	var (
		gapTimer *time.Timer
		gapRead  <-chan time.Time
		// gapWait is the current re-read delay, zero while no gap is open; gapSafe is the horizon
		// the read that scheduled it read through.
		gapWait time.Duration
		gapSafe int64
	)
	defer func() {
		if gapTimer != nil {
			gapTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			if !waitFor(ctx, r.window()) {
				return
			}
		case <-gapRead:
		}
		if gapTimer != nil {
			gapTimer.Stop()
			gapTimer, gapRead = nil, nil
		}
		safe, gap, ok := r.read(ctx)
		for !ok {
			if !waitFor(ctx, eventRelayRetry) {
				return
			}
			safe, gap, ok = r.read(ctx)
		}
		switch {
		case !gap:
			gapWait = 0
			continue
		case gapWait == 0 || safe != gapSafe:
			gapWait = eventRelayGapMin
		default:
			gapWait = min(2*gapWait, eventRelayGapMax)
		}
		gapSafe = safe
		gapTimer = time.NewTimer(gapWait)
		gapRead = gapTimer.C
	}
}

// read publishes every event after the cursor and at or below the safe horizon, in id order,
// through the same read a reconnect's catch-up uses (readEventsAfterID), and moves the cursor
// over each page as it publishes it (publish). Nothing but this goroutine writes the cursor once
// the relay has started. It pages until a short page, as streamEvents does, all through the one
// horizon it read first: what commits meanwhile is the next read's. A log that was reset under
// the cursor is answered first, and publishes nothing (resetCursor).
//
// It answers the horizon it read through and whether a gap stands open above it (Pending): an
// event observed above the horizon that this read could not publish, which run re-reads for.
// ok false is a read that failed.
func (r *eventStreamRelay) read(ctx context.Context) (safe int64, gap, ok bool) {
	tracked := store.WithTransactionTracking(ctx)
	horizon := r.server.horizon()
	switch reset, resetSafe, err := r.resetCursor(tracked); {
	case err != nil:
		if ctx.Err() == nil {
			slog.Warn("dispatch: live event stream could not check its cursor against the event log; retrying", "error", err)
		}
		return 0, false, false
	case reset:
		return resetSafe, horizon.Pending(), true
	}
	safe, err := horizon.Safe(tracked)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: live event stream could not observe the event log's safe horizon; retrying", "error", err)
		}
		return 0, false, false
	}
	for {
		cursor, _ := r.at()
		page, err := r.server.readEventsAfterID(tracked, cursor, safe)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("dispatch: live event stream could not read the committed events; retrying", "error", err)
			}
			return 0, false, false
		}
		r.publish(page)
		if len(page) < maxSSEReplay {
			return safe, horizon.Pending(), true
		}
	}
}

// publish sends one page to every subscription and moves the cursor to its last event, under one
// hold of mu: a subscriber (subscribe) sees the page and the cursor move together or neither, so
// no cursor it reads excludes an event it was not sent, and none includes an event it was sent.
// Stream.Publish never blocks (an overflowed subscriber is dropped), so the hold is as short as
// the page.
func (r *eventStreamRelay) publish(page []model.Event) {
	if len(page) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, event := range page {
		r.stream.Publish(event)
	}
	if r.beforeAdvance != nil {
		r.beforeAdvance(page)
	}
	last := page[len(page)-1]
	r.cursor, r.cursorAt = last.ID, last.CreatedAt
}

// resetCursor tells an event log that was reset under the cursor from one that has merely gone
// quiet, and answers a reset by forgetting the horizon, moving the cursor to the last event at or
// below a horizon proven afresh, and ending every open subscription. The check is the delivery
// hub's, through the same read (eventCursorCheck, catchUpEvents): a log whose newest id is below
// the cursor, or whose row at the cursor is gone or carries another created_at — a `truncate
// events restart identity` refilled past it, which is what e2e/seed.ts does between browser
// scenarios — is a log the cursor no longer names a position in. Nothing of the first kind of
// reset would ever be published: the new log's ids start again at 1, far below the cursor, so
// `id > cursor` matches no row until they have climbed back past it, and the stream falls silent
// for every reader.
//
// The horizon is the old log's too: its H stands among the old ids, above every id the new log
// has handed out, so read as it stands it would cap nothing until the new log climbed past it,
// and a new cursor taken under it (the new log's raw max(id)) could step over a lower id the
// refill still holds open. Horizon.Reset forgets it, and the new cursor is the last event at or
// below the first horizon proven after that (safeCursor), which waits for every write open at
// the reset to end.
//
// A reset is the one thing the relay cannot publish its way out of, because its ids have been
// reused: the rows it would read as `id > cursor` are not the ones it published, and every open
// subscription's since (streamEvents) is a position in a log that no longer exists, so no frame
// it sent could mean what its reader would take it to mean. Ending the subscriptions is the
// honest answer and the one the stream already has for a subscriber it cannot serve in order: it
// closes the SSE response body, exactly as an overflowed subscriber's is closed (Stream.Publish),
// and sends each browser through its reconnect-from-last-id, which resolves its position against
// the log as it now stands. The cursor moves and the subscriptions end under one hold of mu, so a
// connection that subscribed while the new horizon was awaited, on the old cursor, is ended too,
// and one that subscribes after holds the new cursor. The rows between the old cursor and the new
// one are published to nobody: a subscription that could have received them is gone, and every
// one opened after this holds a cursor at or past them.
//
// The delivery hub answers the same reset by marking its views dirty rather than by dropping
// anything, because it holds a projection of its own that it can recompute from the database; the
// relay holds nothing but a reader's stream, which only that reader can rebuild.
//
// It answers the horizon the new cursor was taken under when it found a reset.
func (r *eventStreamRelay) resetCursor(ctx context.Context) (reset bool, safe int64, err error) {
	cursor, at := r.at()
	maxID, _, cursorAt, err := eventCursorCheck(ctx, r.server.deps.Store.Pool, cursor)
	if err != nil {
		return false, 0, err
	}
	if maxID >= cursor && (cursor == 0 || (cursorAt != nil && cursorAt.Equal(at))) {
		return false, 0, nil
	}
	horizon := r.server.horizon()
	horizon.Reset()
	safe, err = horizon.Await(ctx)
	if err != nil {
		return false, 0, err
	}
	to, toAt, err := lastEventThrough(ctx, r.server.deps.Store.Pool, safe)
	if err != nil {
		return false, 0, err
	}
	r.mu.Lock()
	r.cursor, r.cursorAt = to, toAt
	r.stream.CloseAll()
	r.mu.Unlock()
	slog.Warn("dispatch: the event log was reset; the live stream moved its cursor and ended its open streams",
		"from", cursor, "to", to)
	return true, safe, nil
}

// at is the id the relay has published through and that event's created_at.
func (r *eventStreamRelay) at() (int64, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor, r.cursorAt
}

func (r *eventStreamRelay) window() time.Duration {
	if r.coalesce == 0 {
		return eventRelayCoalesce
	}
	return r.coalesce
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
