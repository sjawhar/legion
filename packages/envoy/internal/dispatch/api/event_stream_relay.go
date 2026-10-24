package api

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pglisten"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// stands at. events.id is commit order, because events.Broker.Append takes the database-wide
// commit-order lock before its insert and holds it until the transaction commits, and nothing
// else in this module inserts an event (TestOnlyTheBrokerInsertsEvents): no row at or below a
// committed max(id) can still be uncommitted, so reading `id > cursor` skips nothing. The cursor
// may lag the database — a notification not yet read, a connection being redialled — and must
// never run ahead of it, which is why the first subscriber reads it before its subscription
// exists.
//
// cursorAt is what tells an event log that was reset from one that has merely gone quiet: see
// resetCursor.
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

	running sync.WaitGroup
	// wake holds at most one signal: what a read answers covers every notification before it.
	wake chan struct{}

	mu       sync.Mutex
	started  bool
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

// subscribe answers one open SSE connection its live feed, starting the relay on the first one:
// a process nobody streams from opens no listening connection of its own.
//
// The cursor is read here, before the subscription exists, so it cannot stand ahead of the head
// the caller resolves next (streamEvents' since): an id the relay had already published past
// would otherwise reach neither that connection's catch-up (id <= since) nor this subscription. A
// cursor read that fails starts nothing and is the caller's error; the next connection tries
// again.
func (r *eventStreamRelay) subscribe(ctx context.Context) (<-chan model.Event, func(), error) {
	if err := r.start(ctx); err != nil {
		return nil, nil, err
	}
	subscription, cancel := r.stream.Subscribe()
	return subscription, cancel, nil
}

// start reads the cursor and runs the relay's two goroutines, once. The cursor is read through
// eventCursorCheck, the same read the reset check uses, so it carries the newest event's
// created_at beside its id from the first moment the relay holds one.
func (r *eventStreamRelay) start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return nil
	}
	cursor, cursorAt, _, err := eventCursorCheck(ctx, r.server.deps.Store.Pool, 0)
	if err != nil {
		return fmt.Errorf("read the live event stream's cursor: %w", err)
	}
	r.cursor, r.cursorAt = cursor, cursorAt
	r.started = true
	dial := r.dial
	if dial == nil {
		dial = r.server.deps.Store.Pool.Listener
	}
	listener := &pglisten.Listener{
		Dial:        dial,
		Channels:    []string{dispatchEventsChannel},
		ListenIdle:  r.listenIdle,
		PingTimeout: r.pingTimeout,
		Backoff:     r.backoff,
		OnConnected: r.onConnected,
		OnNotify:    r.onNotify,
	}
	r.running.Add(2)
	go func() { defer r.running.Done(); listener.Run(r.lifetime) }()
	go func() { defer r.running.Done(); r.run(r.lifetime) }()
	return nil
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
func (r *eventStreamRelay) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		if !waitFor(ctx, r.window()) {
			return
		}
		for !r.read(ctx) {
			if !waitFor(ctx, eventRelayRetry) {
				return
			}
		}
	}
}

// read publishes every committed event after the cursor, in id order, through the same read a
// reconnect's catch-up uses (readEventsAfterID), and moves the cursor only over the rows it has
// published - once a page, to the last of them, rather than taking the lock for each of a
// thousand. Nothing but this goroutine writes the cursor once the relay has started, and a page
// is published before the cursor moves over it, so no reader of it can see a row as published
// that is not. It pages until a short page, as streamEvents does. A log that was reset under the
// cursor is answered first, and publishes nothing (resetCursor). false is a read that failed.
func (r *eventStreamRelay) read(ctx context.Context) bool {
	tracked := store.WithTransactionTracking(ctx)
	switch reset, err := r.resetCursor(tracked); {
	case err != nil:
		if ctx.Err() == nil {
			slog.Warn("dispatch: live event stream could not check its cursor against the event log; retrying", "error", err)
		}
		return false
	case reset:
		return true
	}
	for {
		cursor, _ := r.at()
		committed, err := r.server.readEventsAfterID(tracked, cursor)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("dispatch: live event stream could not read the committed events; retrying", "error", err)
			}
			return false
		}
		for _, event := range committed {
			r.stream.Publish(event)
		}
		if len(committed) > 0 {
			last := committed[len(committed)-1]
			r.advance(last.ID, last.CreatedAt)
		}
		if len(committed) < maxSSEReplay {
			return true
		}
	}
}

// resetCursor tells an event log that was reset under the cursor from one that has merely gone
// quiet, and answers a reset by moving the cursor to the newest event and ending every open
// subscription. The check is the delivery hub's, through the same read (eventCursorCheck,
// catchUpEvents): a log whose newest id is below the cursor, or whose row at the cursor is gone
// or carries another created_at — a `truncate events restart identity` refilled past it, which is
// what e2e/seed.ts does between browser scenarios — is a log the cursor no longer names a
// position in. Nothing of the first kind of reset would ever be published: the new log's ids
// start again at 1, far below the cursor, so `id > cursor` matches no row until they have climbed
// back past it, and the stream falls silent for every reader.
//
// A reset is the one thing the relay cannot publish its way out of, because its ids have been
// reused: the rows it would read as `id > cursor` are not the ones it published, and every open
// subscription's since (streamEvents) is a position in a log that no longer exists, so no frame
// it sent could mean what its reader would take it to mean. Ending the subscriptions is the
// honest answer and the one the stream already has for a subscriber it cannot serve in order: it
// closes the SSE response body, exactly as an overflowed subscriber's is closed (Stream.Publish),
// and sends each browser through its reconnect-from-last-id, which resolves its position against
// the log as it now stands. The rows between the old cursor and the new one are published to
// nobody: a subscription that could have received them is gone, and one opened after this holds a
// since the relay's cursor is at or past.
//
// The delivery hub answers the same reset by marking its views dirty rather than by dropping
// anything, because it holds a projection of its own that it can recompute from the database; the
// relay holds nothing but a reader's stream, which only that reader can rebuild.
func (r *eventStreamRelay) resetCursor(ctx context.Context) (bool, error) {
	cursor, at := r.at()
	maxID, maxAt, cursorAt, err := eventCursorCheck(ctx, r.server.deps.Store.Pool, cursor)
	if err != nil {
		return false, err
	}
	if maxID >= cursor && (cursor == 0 || (cursorAt != nil && cursorAt.Equal(at))) {
		return false, nil
	}
	r.mu.Lock()
	r.cursor, r.cursorAt = maxID, maxAt
	r.mu.Unlock()
	r.stream.CloseAll()
	slog.Warn("dispatch: the event log was reset; the live stream moved its cursor and ended its open streams",
		"from", cursor, "to", maxID)
	return true, nil
}

// at is the id the relay has published through and that event's created_at.
func (r *eventStreamRelay) at() (int64, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor, r.cursorAt
}

// advance moves the cursor to an event the relay has just published.
func (r *eventStreamRelay) advance(id int64, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursor, r.cursorAt = id, at
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
