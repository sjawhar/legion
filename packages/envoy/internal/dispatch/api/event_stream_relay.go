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
// cursor is the id the relay has published through. events.id is commit order, because
// events.Broker.Append takes the database-wide commit-order lock before its insert and holds it
// until the transaction commits, and nothing else in this module inserts an event
// (TestOnlyTheBrokerInsertsEvents): no row at or below a committed max(id) can still be
// uncommitted, so reading `id > cursor` skips nothing. The cursor may lag the database — a
// notification not yet read, a connection being redialled — and must never run ahead of it, which
// is why the first subscriber reads it before its subscription exists.
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

	mu      sync.Mutex
	started bool
	cursor  int64
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

// start reads the cursor and runs the relay's two goroutines, once.
func (r *eventStreamRelay) start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return nil
	}
	var cursor int64
	if err := r.server.deps.Store.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&cursor); err != nil {
		return fmt.Errorf("read the live event stream's cursor: %w", err)
	}
	r.cursor = cursor
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
// notifications sent while the relay had no connection. It never moves the cursor: a redial reads
// from where the relay published through, not from the head, so nothing committed in that window
// is skipped.
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
// published. It pages until a short page, as streamEvents does. false is a read that failed.
func (r *eventStreamRelay) read(ctx context.Context) bool {
	for {
		committed, err := r.server.readEventsAfterID(store.WithTransactionTracking(ctx), r.at())
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("dispatch: live event stream could not read the committed events; retrying", "error", err)
			}
			return false
		}
		for _, event := range committed {
			r.stream.Publish(event)
			r.advance(event.ID)
		}
		if len(committed) < maxSSEReplay {
			return true
		}
	}
}

// at is the id the relay has published through.
func (r *eventStreamRelay) at() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

// advance moves the cursor to an id the relay has just published.
func (r *eventStreamRelay) advance(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursor = id
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
