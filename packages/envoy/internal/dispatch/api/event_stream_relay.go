package api

import (
	"context"
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
	// eventRelayPage is how many events one relay read takes; a full page is followed at once by
	// another. It bounds one read's rows, not what the relay publishes: a burst of commits larger
	// than a page is published in pages, all of it.
	eventRelayPage = 1000
)

// relayedEvent is one committed event as the relay publishes it: the event and its whole SSE
// frame, encoded once for every subscriber (encodeSSEFrame). Every subscriber receives the same
// event and the same frame bytes, so none of them modifies either.
type relayedEvent struct {
	Event model.Event
	Frame []byte
}

// eventStreamRelay is the live source of every open SSE connection on this process: while anyone
// subscribes, it holds a `listen dispatch_events` connection of its own (migration 0107 notifies
// on each commit) and publishes every committed event into its stream, whichever process committed
// it. While nobody subscribes it holds no connection and reads nothing.
//
// Two envoy-dispatch processes serve one Postgres. The broker's fan-out reaches only the process
// that appended the event, so a reader whose connection landed on the other one would see nothing
// of it until it reconnected. Re-sourcing the live stream from the database carries both
// processes' events on the one path, and makes the live frame and the replayed frame the same
// bytes: both are encodeSSEFrame of an event read through readEventRows, with an ask event's
// read-time fields attached.
//
// Each run of the relay is a generation (relayGeneration): the first subscription starts one, and
// the cancellation of its last subscription stops it, at once, with no grace period. A stopped
// generation's goroutines end with its context; the next subscription starts a new generation,
// which reads the cursor afresh.
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
	// readAfter reads one page of the committed events after an id, in id order; nil is
	// readEventsAfterID.
	readAfter func(ctx context.Context, after int64) ([]model.Event, error)
	// beforeLiveWrite runs before an SSE connection writes a live frame (streamEvents); nil in
	// every server the deployment builds. A test blocks it to hold a reader still while the relay
	// publishes past its buffer.
	beforeLiveWrite func()

	// running counts every generation's goroutines. Add runs under mu and only while closed is
	// false, so no Add can follow the Wait in wait.
	running sync.WaitGroup

	// mu guards closed, generation, and the running generation's cursor and subscriber count.
	mu sync.Mutex
	// closed is set by wait: the lifetime has ended and no generation starts again.
	closed bool
	// generation is the running generation, nil while nobody subscribes.
	generation *relayGeneration
}

// relayGeneration is one run of the relay. Its goroutines run on a context of their own, which
// stop cancels; stream, wake and stop are set once at its start, and the rest is guarded by the
// relay's mu.
//
// cursor is the id the generation has published through, and cursorAt the created_at of the event
// it stands at. events.id is commit order, because events.Broker.Append takes the database-wide
// commit-order lock before its insert and holds it until the transaction commits, and nothing
// else in this module inserts an event (TestOnlyTheBrokerInsertsEvents): no row at or below a
// committed max(id) can still be uncommitted, so reading `id > cursor` skips nothing. The cursor
// may lag the database — a notification not yet read, a connection being redialled — and never
// runs ahead of it: it starts at the newest committed row and moves only over rows the generation
// has published. cursorAt is what tells an event log that was reset from one that has merely gone
// quiet: see resetCursor.
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
	// generation runs on, or its generation ended (a reset, disconnect, the lifetime).
	events <-chan relayedEvent
	// ended is closed once the subscription's generation has ended, and always before that end
	// closes events (endLocked stops the generation, then closes its subscriptions): events closed
	// while ended is still open is an overflow.
	ended <-chan struct{}
	// head is the id the relay had published through when the subscription was made. Every event
	// at or below it is committed, so the consumer reads those from the log itself.
	head int64
	// cancel ends the subscription; it is idempotent.
	cancel func()
}

// subscribe answers one consumer its live feed on a channel of buffer slots (relaySubscription).
//
// The subscription, the head and the start of a generation when none runs happen under mu, which
// the read loop also holds across publishing a page and moving the cursor over it: a page is
// either published into this subscription or already at or below its head, never neither. A
// generation started here reads its cursor through eventCursorCheck, the same read the reset
// check uses, so it carries the newest event's created_at beside its id from its first moment. A
// cursor read that fails starts nothing and is the caller's error; the next subscription tries
// again. Once the lifetime has ended, subscribe refuses.
//
// Cancelling the last subscription of the running generation stops that generation: its listening
// connection closes and it reads nothing more. An overflowed subscription still counts until it is
// cancelled, so a consumer that subscribes again before cancelling it keeps the generation running.
func (r *eventStreamRelay) subscribe(ctx context.Context, buffer int) (relaySubscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.lifetime.Err() != nil {
		return relaySubscription{}, errorf(http.StatusServiceUnavailable, "SHUTTING_DOWN", "the server is shutting down")
	}
	g := r.generation
	if g == nil {
		started, err := r.startLocked(ctx)
		if err != nil {
			return relaySubscription{}, err
		}
		g = started
	}
	feed, unsubscribe := g.stream.Subscribe(buffer)
	g.subscribers++
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			unsubscribe()
			g.subscribers--
			if g.subscribers == 0 && r.generation == g {
				r.generation = nil
				g.stop()
			}
		})
	}
	return relaySubscription{events: feed, ended: g.ended, head: g.cursor, cancel: cancel}, nil
}

// startLocked reads the cursor and runs a new generation's listener and read loop, with mu held.
func (r *eventStreamRelay) startLocked(ctx context.Context) (*relayGeneration, error) {
	cursor, cursorAt, _, err := eventCursorCheck(ctx, r.server.deps.Store.Pool, 0)
	if err != nil {
		return nil, fmt.Errorf("read the live event stream's cursor: %w", err)
	}
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
		Channels:    []string{dispatchEventsChannel},
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
	return g, nil
}

// wait marks the relay closed, so no generation starts again, then returns once every
// generation's goroutines have. It is called after the lifetime ended, which ends them all.
func (r *eventStreamRelay) wait() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.running.Wait()
}

// disconnect ends the running generation and closes every subscription it holds, as a reset does
// (resetCursor). The next subscription starts a new generation.
func (r *eventStreamRelay) disconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != nil {
		r.endLocked(r.generation)
	}
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
func (r *eventStreamRelay) run(ctx context.Context, g *relayGeneration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.wake:
		}
		if !waitFor(ctx, r.window()) {
			return
		}
		for !r.read(ctx, g) {
			if !waitFor(ctx, eventRelayRetry) {
				return
			}
		}
	}
}

// read publishes every committed event after g's cursor, in id order, each with its frame encoded
// once, and moves the cursor over the rows it has published - once a page, to the last of them.
// It holds mu across publishing a page and moving the cursor, so a subscription made meanwhile
// either receives the page or has a head past it. Nothing but this goroutine writes g's cursor,
// and a generation stopped while its read was in flight publishes only into its own stream, which
// has no subscriber left. A log that was reset under the cursor is answered first, and publishes
// nothing (resetCursor). false is a read that failed.
func (r *eventStreamRelay) read(ctx context.Context, g *relayGeneration) bool {
	tracked := store.WithTransactionTracking(ctx)
	switch reset, err := r.resetCursor(tracked, g); {
	case err != nil:
		if ctx.Err() == nil {
			slog.Warn("dispatch: live event stream could not check its cursor against the event log; retrying", "error", err)
		}
		return false
	case reset:
		return true
	}
	for {
		cursor, _ := r.at(g)
		committed, err := r.page(tracked, cursor)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("dispatch: live event stream could not read the committed events; retrying", "error", err)
			}
			return false
		}
		relayed := make([]relayedEvent, len(committed))
		for index, event := range committed {
			frame, err := encodeSSEFrame(event)
			if err != nil {
				slog.Warn("dispatch: live event stream could not encode a committed event; retrying", "id", event.ID, "error", err)
				return false
			}
			relayed[index] = relayedEvent{Event: event, Frame: frame}
		}
		r.publish(g, relayed)
		if len(committed) < eventRelayPage {
			return true
		}
	}
}

// page reads the committed events after the cursor: the relay's one read of the event log's rows.
func (r *eventStreamRelay) page(ctx context.Context, after int64) ([]model.Event, error) {
	if r.readAfter != nil {
		return r.readAfter(ctx, after)
	}
	return r.server.readEventsAfterID(ctx, after)
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
// quiet, and answers a reset by ending g and every subscription it holds. The check is the
// delivery hub's, through the same read (eventCursorCheck, catchUpEvents): a log whose newest id
// is below the cursor, or whose row at the cursor is gone or carries another created_at — a
// `truncate events restart identity` refilled past it, which is what e2e/seed.ts does between
// browser scenarios — is a log the cursor no longer names a position in. Nothing of the first kind
// of reset would ever be published: the new log's ids start again at 1, far below the cursor, so
// `id > cursor` matches no row until they have climbed back past it, and the stream falls silent
// for every reader.
//
// A reset is the one thing the relay cannot publish its way out of, because its ids have been
// reused: the rows it would read as `id > cursor` are not the ones it published, and every open
// subscription's position (streamEvents' dedupe cursor) is in a log that no longer exists, so no
// frame it sent could mean what its reader would take it to mean. Ending the subscriptions is the
// honest answer: the generation ends, so each SSE connection ends its response rather than
// catching up as it does after an overflow, and each browser goes through its
// reconnect-from-last-id, which resolves its position against the log as it now stands. The next
// subscription starts a new generation, whose cursor is the new log's newest row.
//
// The delivery hub answers the same reset by marking its views dirty rather than by dropping
// anything, because it holds a projection of its own that it can recompute from the database; the
// relay holds nothing but a reader's stream, which only that reader can rebuild.
func (r *eventStreamRelay) resetCursor(ctx context.Context, g *relayGeneration) (bool, error) {
	cursor, at := r.at(g)
	maxID, _, cursorAt, err := eventCursorCheck(ctx, r.server.deps.Store.Pool, cursor)
	if err != nil {
		return false, err
	}
	if maxID >= cursor && (cursor == 0 || (cursorAt != nil && cursorAt.Equal(at))) {
		return false, nil
	}
	r.mu.Lock()
	r.endLocked(g)
	r.mu.Unlock()
	slog.Warn("dispatch: the event log was reset; the live stream ended its open streams",
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

// readEventsAfterID reads the next page of committed events after an id, ask fields attached.
func (s *server) readEventsAfterID(ctx context.Context, after int64) ([]model.Event, error) {
	return s.readEventRows(ctx, eventSelect+`
		where e.id > $1
		order by e.id asc limit $2
	`, after, eventRelayPage)
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
