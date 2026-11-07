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
	// stands open above the safe horizon it read through (Horizon.Behind): it first waits
	// eventRelayGapMin, doubles the wait up to eventRelayGapMax while the horizon stays where it
	// was, and starts again from eventRelayGapMin each time the horizon moves. A commit's
	// notification can wake the read before the horizon covers that commit: only a later
	// observation proves an earlier one safe, and nothing notifies again once the open lower ids
	// have ended, so without this re-read an event the read held back would wait for the next
	// unrelated commit.
	eventRelayGapMin = 10 * time.Millisecond
	eventRelayGapMax = 250 * time.Millisecond
)

// eventGapReread schedules a reader's re-read while a gap stands open above the safe horizon,
// shared by the relay (run) and the delivery hub (intake): timer is the scheduled re-read, nil
// while none is; wait the current delay, zero while no gap is open; and safe the horizon the read
// that scheduled it read through. Its one owner's goroutine alone touches it, and receives the
// re-read from due.
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

// eventStreamRelay is the live source of every events subscription on this process (the event
// feed, live_events.go): it holds a `listen dispatch_events` connection of its own (migration 0107
// notifies on each commit) and publishes every committed event into its events.Stream, whichever
// process committed it.
//
// Two envoy-dispatch processes serve one Postgres. The broker's fan-out reaches only the process
// that appended the event, so a page whose socket landed on the other one would see nothing of it
// until it subscribed again. Re-sourcing the live feed from the database carries both processes'
// events on the one path, and makes the live frame and the replayed frame the same bytes: both
// come from readEventsAfterID, with an ask event's read-time fields attached.
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
	// live is the pool every read of the relay's goes through (store.Pool.Live), so a commit
	// reaches the open streams while the requests hold every connection of theirs. start sets it,
	// before the goroutines that read it run.
	live *store.Pool

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

// subscribe answers one events subscription its live feed and the id the relay had published
// through when that feed began, starting the relay on the first one: a process nobody subscribes
// to opens no listening connection of its own.
//
// The subscription and the cursor are taken under the one hold of mu that every page's publish
// and cursor move also take, so they describe the same moment: every event at or below the
// cursor was published before the subscription existed (it is final, and the caller replays it
// from the log), and every event above it will be published into the subscription, in strictly
// increasing id order. Read apart, a page published between the two would reach the caller twice
// or, read in the other order, not at all. A start that fails starts nothing and is the caller's
// error; the next subscription tries again.
func (r *eventStreamRelay) subscribe(ctx context.Context) (<-chan model.Event, func(), int64, time.Time, error) {
	if err := r.start(ctx); err != nil {
		return nil, nil, 0, time.Time{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	subscription, cancel := r.stream.Subscribe()
	return subscription, cancel, r.cursor, r.cursorAt, nil
}

// start reads the first cursor and runs the relay's two goroutines, once. The first cursor stands
// on a row whose created_at the reset check can compare, and is the first subscription's start
// point (subscribe), so it must leave nothing that commits after that subscription began at or
// below it (firstRelayCursor). That read, like every read the relay makes, is on the live pool.
//
// That read can wait for a safe observation, which an open write anywhere on the instance can
// hold back, so it runs outside mu: the caller running it marks starting, and every other caller
// waits on starting (or its own ctx) rather than on mu or a spin. A read that fails, ctx's end
// included, leaves the relay unstarted, and the next caller runs the read again.
//
// A start whose read ends after the relay's lifetime has is refused, and starts nothing. The
// caller's subscribe runs on a goroutine of the live socket's own (runSubscription), so the
// server's shutdown does not wait for it, and the shutdown may already be in wait: running's Add
// then would race that Wait, and the goroutines it counted would run unwaited for. The check and
// the Add are under the one hold of mu that wait takes before its Wait, so either the Add comes
// first or the check sees the lifetime ended.
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
				return fmt.Errorf("wait for the event stream relay to start: %w", ctx.Err())
			}
		}
		starting := make(chan struct{})
		r.starting = starting
		r.mu.Unlock()

		// Every read the relay makes goes through the live pool, the first cursor's included, so
		// a commit reaches the open streams while the requests hold every connection of theirs.
		live, err := r.server.deps.Store.Pool.Live()
		var cursor int64
		var cursorAt time.Time
		if err != nil {
			err = fmt.Errorf("open the live event stream's pool: %w", err)
		} else if cursor, cursorAt, err = r.server.firstRelayCursor(ctx, live); err != nil {
			err = fmt.Errorf("read the event stream relay's cursor: %w", err)
		}

		r.mu.Lock()
		r.starting = nil
		close(starting)
		if err != nil {
			r.mu.Unlock()
			return err
		}
		if err := r.lifetime.Err(); err != nil {
			r.mu.Unlock()
			return fmt.Errorf("start the event stream relay: the server is shutting down: %w", err)
		}
		r.cursor, r.cursorAt, r.live, r.started = cursor, cursorAt, live, true
		r.running.Add(2)
		r.mu.Unlock()

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
			OnConnected: r.onConnected,
			OnNotify:    r.onNotify,
		}
		go func() { defer r.running.Done(); listener.Run(r.lifetime) }()
		go func() { defer r.running.Done(); r.run(r.lifetime) }()
		return nil
	}
}

// firstRelayCursor is the relay's first cursor, which is the first subscription's start point, so
// no event that commits after that subscription began may stand at or below it. It reads the
// log as it stands at the start, in one statement: its newest id and every gap among its ids above
// a floor at or below which no event can still be open (firstRelayFloor). An event that commits
// after that read stands above the newest id, or in one of those gaps: an id it drew before the
// read, while its transaction was still open. A gap can also be an id whose transaction rolled
// back. Below the floor no gap can fill.
//
// It then waits for a horizon proven after the read (Horizon.Await), past which every id at or
// below that newest one is final, and reads which of the gaps have filled since: those are events
// that committed after the start began. The cursor is the last event below the lowest of them, or
// at the newest id when none filled, so the first subscription is sent every event that committed
// after the read and none committed before it, but those between the lowest late one and the
// newest id. A gap still empty then is a rollback, and moves the cursor nowhere.
//
// The wait is the start's, as before: as long as any transaction open on the instance holds the
// horizon back, with the subscription's events_start not yet sent (feedEvents). The gap read
// covers the ids above the floor, which are the ids written since a recent mark, however long the
// log. An event that commits between the first subscription's start and this read, a round trip
// apart, counts as committed before it.
//
// pool is the relay's own, the live pool (eventStreamRelay.live), as every read the relay makes
// is; the horizon observes on the horizon pool, which is neither.
func (s *server) firstRelayCursor(ctx context.Context, pool queryer) (int64, time.Time, error) {
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

// firstRelayFloor is the id the first start reads the log's gaps above (firstRelayCursor): one at
// or below which no event can still be open, so no gap there can fill (server.writerFloor: the
// newest of the server's marks that counts, or else the horizon already proven or persisted). A
// mark counts once every transaction that holds a write lock on events took its transaction id
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
		slog.Warn("dispatch: the event stream relay's first start reads the whole event log: an event insert of this database has been open since before every mark it keeps, and no horizon is proven or persisted")
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

// awaitEventCursor waits for a horizon proven safe by an observation taken during the call
// (Horizon.Await), which is at or above every id committed before the call, and answers it with
// the last event at or below it and that event's created_at: 0 and the zero time when there is
// none. It returns early when ctx ends, and with the observation's error once every observation
// has failed for a while (Horizon.Await), so no caller waits on a failing database without one.
//
// Its callers are the live readers - the relay's reset check and the delivery hub's seed - so its
// read goes through the live pool, as theirs do.
func (s *server) awaitEventCursor(ctx context.Context) (safe, id int64, at time.Time, err error) {
	if safe, err = s.horizon().Await(ctx); err != nil {
		return 0, 0, time.Time{}, err
	}
	live, err := s.deps.Store.Pool.Live()
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("open the live event readers' pool: %w", err)
	}
	id, at, err = lastEventThrough(ctx, live, safe)
	return safe, id, at, err
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

// reseedEventCursor answers an event log that was reset under a reader's cursor
// (eventLogWasReset). The horizon is then the old log's: its H stands among the old ids, above
// every id the new log has handed out, so read as it stands it would cap nothing until the new log
// climbed past it, and a cursor taken under it (the new log's raw max(id)) could step over a lower
// id the refill still holds open. It forgets that horizon (Horizon.Reset) and answers the new
// cursor as awaitEventCursor does: the last event at or below the first horizon proven after the
// reset, which waits for every write open at the reset to end. The relay (resetCursor) and the
// delivery hub (seedEvents) both answer a reset through it.
func (s *server) reseedEventCursor(ctx context.Context) (safe, id int64, at time.Time, err error) {
	s.horizon().Reset()
	s.eventMarks.forget()
	return s.awaitEventCursor(ctx)
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
// subscription ever started has none to wait for. It takes mu once before its Wait, so a start
// still finishing has either counted its goroutines by then or will see the lifetime ended and
// start none (start).
func (r *eventStreamRelay) wait() {
	r.mu.Lock()
	r.mu.Unlock() //nolint:staticcheck // an empty critical section orders this Wait after start's Add
	r.running.Wait()
}

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
// ever. A gap re-read skips the reset check: it asks only whether the horizon has moved, on the
// horizon's own connection, and reads the shared pool only once there is something to publish,
// so a gap held open by a long transaction costs no request a connection. A reset is still caught
// at the next notification, which the refill's own inserts send.
func (r *eventStreamRelay) run(ctx context.Context) {
	var gaps eventGapReread
	defer gaps.stop()
	for {
		checkReset := true
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			if !waitFor(ctx, r.window()) {
				return
			}
		case <-gaps.due():
			checkReset = false
		}
		gaps.stop()
		safe, gap, ok := r.read(ctx, checkReset)
		for !ok {
			if !waitFor(ctx, eventRelayRetry) {
				return
			}
			safe, gap, ok = r.read(ctx, checkReset)
		}
		gaps.schedule(safe, gap)
	}
}

// read publishes every event after the cursor and at or below the safe horizon, in id order,
// through the same read an events subscription's catch-up uses (readEventsAfterID), and moves the cursor
// over each page as it publishes it (publish). Nothing but this goroutine writes the cursor once
// the relay has started. It pages until a short page, as that catch-up does, or until the cursor
// stands on the horizon, all through the one horizon it read first: what commits meanwhile is the
// next read's. A log that was reset under the cursor is answered first, and publishes nothing
// (resetCursor); checkReset false skips that check (run's gap re-read).
//
// It answers the horizon it read through and whether a gap stands open above it (Behind, asked
// with that horizon and not the one the Horizon holds by then, which another reader's
// observation can have raised past it): an event observed above the horizon this read could not
// publish, which run re-reads for.
// ok false is a read that failed.
func (r *eventStreamRelay) read(ctx context.Context, checkReset bool) (safe int64, gap, ok bool) {
	tracked := store.WithTransactionTracking(ctx)
	horizon := r.server.horizon()
	if checkReset {
		switch reset, resetSafe, err := r.resetCursor(tracked); {
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("dispatch: the event stream relay could not check its cursor against the event log; retrying", "error", err)
			}
			return 0, false, false
		case reset:
			return resetSafe, horizon.Behind(resetSafe), true
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
		cursor, _ := r.at()
		if cursor >= safe {
			return safe, horizon.Behind(safe), true
		}
		page, err := r.server.readEventsAfterID(tracked, r.live, cursor, safe)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("dispatch: the event stream relay could not read the committed events; retrying", "error", err)
			}
			return 0, false, false
		}
		r.publish(page)
		if len(page) < maxEventReplay {
			return safe, horizon.Behind(safe), true
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
// hub's, through the same read and test (eventCursorCheck, eventLogWasReset): a log whose newest
// id is below the cursor, or whose row at the cursor is gone or carries another created_at — a
// `truncate events restart identity` refilled past it, which is what e2e/seed.ts does between
// browser scenarios — is a log the cursor no longer names a position in. Nothing of the first kind
// of reset would ever be published: the new log's ids start again at 1, far below the cursor, so
// `id > cursor` matches no row until they have climbed back past it, and the stream falls silent
// for every reader.
//
// The horizon is the old log's too, so the new cursor comes from reseedEventCursor, which forgets
// it and takes the last event at or below the first horizon proven after that.
//
// A reset is the one thing the relay cannot publish its way out of, because its ids have been
// reused: the rows it would read as `id > cursor` are not the ones it published, and every open
// subscription's since (live_events.go) is a position in a log that no longer exists, so no frame
// it sent could mean what its reader would take it to mean. Ending the subscriptions is the
// honest answer and the one the stream already has for a subscriber it cannot serve in order: it
// closes each subscriber's channel, exactly as an overflowed subscriber's is closed
// (Stream.Publish), which ends each events subscription with an ended frame (behind) and sends its
// page to subscribe again from the last id it applied, a since the subscription clamps to the log
// as it now stands. The cursor moves and the subscriptions end under one hold of mu, so a
// subscription made while the new horizon was awaited, on the old cursor, is ended too,
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
	maxID, cursorAt, err := eventCursorCheck(ctx, r.live, cursor)
	if err != nil {
		return false, 0, err
	}
	if !eventLogWasReset(cursor, at, maxID, cursorAt) {
		return false, 0, nil
	}
	safe, to, toAt, err := r.server.reseedEventCursor(ctx)
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
