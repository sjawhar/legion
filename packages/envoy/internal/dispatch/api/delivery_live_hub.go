package api

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The delivery hub's side of GET /ws/delivery (LEGION-690). The socket keeps no view state of its
// own. It hands each subscribe and unsubscribe frame to the hub (Subscribe, Unsubscribe), calls
// Close however it ends, and writes every frame the hub sends it through its deliveryLiveConn.

// deliveryLiveConn is one socket as the hub sees it. Send queues one frame (a DeliveryLiveSnapshot,
// DeliveryLiveDelta, DeliveryLiveFreshness or DeliveryLiveError) and never blocks: false means the
// socket's queue is full, and the socket then closes itself (1013), so the page reconnects and
// resumes.
type deliveryLiveConn interface {
	Send(frame any) bool
}

// Subscribe answers one subscribe frame on conn. A view the matching GET route would refuse is an
// INVALID_QUERY error frame naming the sub, a malformed view or a resume seq that is not a decimal
// change position INVALID_FRAME. Otherwise conn's sub follows the view from now on: it is answered
// a snapshot, or, on a resume, the delta against the revisions the page holds (a snapshot when the
// page applied nothing since before the oldest kept change, OldestChangeSeq() - 1), and later only
// the deltas. While delivery is not configured the sub is told DELIVERY_NOT_CONFIGURED once and
// stays open, and the settings' first save answers its snapshot. A sub id conn already holds is
// replaced.
func (h *deliveryHub) Subscribe(conn deliveryLiveConn, frame DeliveryLiveSubscribe) {
	if frame.Sub == "" {
		conn.Send(deliveryLiveRefusal("", "INVALID_FRAME", "a subscribe frame names its sub"))
		return
	}
	spec, refusal := parseDeliveryLiveView(frame.View)
	if refusal != nil {
		refusal.Sub = frame.Sub
		conn.Send(*refusal)
		return
	}
	h.start()
	var held *DeliveryLiveRevs
	if frame.Resume != nil {
		seq, err := strconv.ParseInt(frame.Resume.Seq, 10, 64)
		if err != nil || seq < 0 {
			conn.Send(deliveryLiveRefusal(frame.Sub, "INVALID_FRAME", "resume.seq must be a decimal change position"))
			return
		}
		// Read before the hub's lock, which no database read is made under.
		oldest, err := delivery.OldestChangeSeq(h.readContext(), h.server.deps.Store.Pool)
		switch {
		case err != nil:
			slog.Warn("dispatch: delivery live resume answers a snapshot: read the oldest kept change", "error", err)
		case oldest > 0 && seq < oldest-1:
			// The page applied nothing since before the oldest kept change: it reads its window once.
		default:
			held = &frame.Resume.Revs
		}
	}

	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(conn, frame.Sub)
	view := h.views[spec.key]
	if view == nil {
		view = &deliveryView{spec: spec, subs: map[*deliverySub]struct{}{}}
		h.views[spec.key] = view
	}
	sub := &deliverySub{id: frame.Sub, conn: conn, view: view, needsSnapshot: held == nil}
	if held != nil {
		sub.revs = *held
	}
	view.subs[sub] = struct{}{}
	subs := h.conns[conn]
	if subs == nil {
		subs = map[string]*deliverySub{}
		h.conns[conn] = subs
	}
	subs[frame.Sub] = sub
	switch {
	case view.body != nil && !view.dirty && !view.computing:
		h.answerLocked(sub)
	case view.computing || view.dirty:
		// The recompute under way, or the one the view already waits for, answers it.
	default:
		h.markDirtyLocked(view, now, deliveryCauseSubscribe)
	}
}

// Unsubscribe drops conn's sub; a view's last sub drops the view.
func (h *deliveryHub) Unsubscribe(conn deliveryLiveConn, sub string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(conn, sub)
}

// Close drops every sub conn holds. The socket calls it however it ends: shutdown (1012), its
// request ending (1008), a full queue (1013), a read error.
func (h *deliveryHub) Close(conn deliveryLiveConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.conns[conn] {
		h.dropLocked(conn, id)
	}
}

// dropLocked drops conn's sub id, the view it leaves without a sub, and conn once it holds none.
func (h *deliveryHub) dropLocked(conn deliveryLiveConn, id string) {
	subs := h.conns[conn]
	sub := subs[id]
	if sub == nil {
		return
	}
	delete(subs, id)
	if len(subs) == 0 {
		delete(h.conns, conn)
	}
	delete(sub.view.subs, sub)
	if len(sub.view.subs) == 0 && h.views[sub.view.spec.key] == sub.view {
		delete(h.views, sub.view.spec.key)
	}
}

// deliveryHub keeps every open Delivery and Pipeline page current (LEGION-690). It learns of a change
// from Postgres (LISTEN delivery_changes, delivery_freshness, dispatch_events, on one connection of
// its own, deliveryListener) and from the in-process event broker; marks dirty only the views the
// change can touch; recomputes each dirty view once per burst, one recompute at a time, as the GET
// routes' population read and pure compute; re-runs the compute alone, reading nothing, when
// something leaves a preset window; and sends each subscriber only the keys whose revision differs
// from what it holds.
//
// Three goroutines start on the first Subscribe and run until lifetime ends: the listener; the
// intake, which alone moves the two cursors (the delivery change log's and the event log's) and
// marks views dirty; and the worker, which alone recomputes, so one recompute runs at a time.
type deliveryHub struct {
	server   *server
	clock    hubClock
	lifetime context.Context
	// dial, listenIdle, pingTimeout and backoff are the listener's (deliveryListener); dial is the
	// shared pool's Listener, and a test sets them otherwise before the first Subscribe.
	dial                    func(ctx context.Context) (deliveryListenerConn, error)
	listenIdle, pingTimeout time.Duration
	backoff                 func(attempt int) time.Duration
	// observe, when set, is told of each recompute and catch-up; tests set it.
	observe func(deliveryHubNote)

	once    sync.Once
	running sync.WaitGroup
	// ready closes once the intake has read where both logs stand, which the worker waits for.
	ready chan struct{}
	// Each wake holds at most one pending signal: what it asks for covers every signal before it.
	wakeWork, wakeChanges, wakeEvents, connected chan struct{}
	barrier                                      chan chan struct{}

	mu    sync.Mutex
	views map[string]*deliveryView // by canonical view key
	conns map[deliveryLiveConn]map[string]*deliverySub
	// changeSeq and changeAt are the last delivery change read and its at, to tell a reset log;
	// epoch counts the resets, so a recompute that read across one stores no change position.
	changeSeq int64
	changeAt  time.Time
	epoch     int64
	// eventCursor and eventAt are the last event id scanned and its created_at, to tell a reset log.
	eventCursor int64
	eventAt     time.Time
	// p0 is the open P0 issues (OpenP0Keys), kept current from issue event payloads.
	p0 map[string]struct{}
	// fresh is the freshness row as last heard, and sent the verdict last sent; known is false
	// until the first recompute sets them, silently.
	fresh deliveryFreshnessHeard
	sent  deliveryFreshnessVerdict
	known bool
}

// deliveryListenerConn is what the hub's dial returns: one connection of its own for LISTEN.
type deliveryListenerConn = *pgx.Conn

// hubClock is the hub's clock: what time it is, and a timer that calls f once d has passed. A test
// moves it forward without waiting.
type hubClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) hubTimer
}

type hubTimer interface{ Stop() bool }

// deliveryHubClock is the real clock.
type deliveryHubClock struct{}

func (deliveryHubClock) Now() time.Time { return time.Now().UTC() }

func (deliveryHubClock) AfterFunc(d time.Duration, f func()) hubTimer { return time.AfterFunc(d, f) }

// deliveryView is one view the open pages show, shared by every sub that names it.
type deliveryView struct {
	spec deliveryLiveSpec
	subs map[*deliverySub]struct{}
	// dirty is set when a change can have moved the view's answer; immediate when a sub waits for
	// its first answer, which no quiet delays. firstDirty and lastDirty time the coalescing, and
	// notBefore holds a failed read's retry back.
	dirty, immediate      bool
	firstDirty, lastDirty time.Time
	notBefore             time.Time
	cause                 string
	computing             bool
	// settings, pop and pipe are as of the last read, and readFrom the window start pop was read
	// for: an expiry narrows them without a read.
	settings delivery.DeliverySettings
	pop      deliveryPopulation
	pipe     pipelinePopulation
	readFrom time.Time
	// body is the last answer, nil until the first and while delivery is not configured; seq the
	// change position it reflects and changedAt when that change was recorded; readingSeq the
	// position a read under way reflects, so a change at or before it marks nothing.
	body       *deliveryLiveEncoded
	seq        int64
	readingSeq int64
	changedAt  *time.Time
	// from and at are the window start and the pipeline clock the last answer resolved; expiry is
	// the next moment something leaves it, zero for a custom range or a pinned at.
	from, at time.Time
	expiry   time.Time
	// issues are the issue keys the population names, ancestors theirs, and sessions each
	// session the population names with the title it shows.
	issues, ancestors map[string]struct{}
	sessions          map[string]string
}

// deliverySub is one page's subscription to a view.
type deliverySub struct {
	id   string
	conn deliveryLiveConn
	view *deliveryView
	// revs is what the page holds; needsSnapshot is set until its first answer; told is set once
	// it was told delivery is not configured, so it is told once.
	revs          DeliveryLiveRevs
	needsSnapshot bool
	told          bool
}

// deliveryLiveSpec is a subscribed view, resolved: its kind, its window (a preset's length in days,
// or a custom range), its query without from and to, a pipeline view's pinned at, and the
// canonical key identical views share.
type deliveryLiveSpec struct {
	kind     string
	preset   int
	from, to time.Time
	query    url.Values
	at       *time.Time
	key      string
}

// deliveryFreshnessHeard is the freshness row as the hub last heard it, and the population's
// unfetchable count as the last recompute computed it.
type deliveryFreshnessHeard struct {
	lastEventAt, lastReconcileAt *time.Time
	lastError                    *string
	unfetchable                  int
}

// deliveryFreshnessVerdict is what a freshness frame is sent for: the last error, the sources past
// their threshold and the unfetchable count.
type deliveryFreshnessVerdict struct {
	lastError   *string
	stale       []string
	unfetchable int
}

// deliveryHubNote is one thing the hub did, for a test's observe.
type deliveryHubNote struct {
	what          string // "recompute begin", "recompute end", "events caught up", "connected", "broker dropped"
	view          string // the view's key
	cause         string
	sent          int
	from, through int64
}

const (
	// A dirty view is recomputed once no change has marked it for viewQuiet, or viewMaxWait after
	// the first did, whichever comes first.
	viewQuiet   = 250 * time.Millisecond
	viewMaxWait = 2 * time.Second
	// deliveryLiveRetry is how long a view whose read failed waits before the next.
	deliveryLiveRetry = 5 * time.Second
	// deliveryLivePage is how many changes or events one catch-up read takes.
	deliveryLivePage = 1000
	// deliveryStaleAfter is how long the reconciled sources go without a pass before the verdict
	// names them stale: freshness.ts's three missed five-minute intervals.
	deliveryStaleAfter = 3 * delivery.ReconcileInterval

	deliveryCauseSubscribe = "subscribe"
	deliveryCauseChange    = "change"
	deliveryCauseExpiry    = "expiry"
)

// deliveryLivePresets are the preset windows a page offers, in days.
var deliveryLivePresets = []int{7, 14, 28, 84}

// deliveryReconciledSources are the sources a missed reconcile leaves stale; the events, which
// arrive irregularly, never are.
var deliveryReconciledSources = []string{"prs", "runs", "ci"}

// newDeliveryHub is the hub of s's open pages. Nothing runs until the first Subscribe, which starts
// its goroutines on lifetime.
func newDeliveryHub(s *server, lifetime context.Context, clock hubClock) *deliveryHub {
	return &deliveryHub{
		server: s, clock: clock, lifetime: lifetime,
		dial:     s.deps.Store.Pool.Listener,
		ready:    make(chan struct{}),
		wakeWork: make(chan struct{}, 1), wakeChanges: make(chan struct{}, 1),
		wakeEvents: make(chan struct{}, 1), connected: make(chan struct{}, 1),
		barrier: make(chan chan struct{}),
		views:   map[string]*deliveryView{},
		conns:   map[deliveryLiveConn]map[string]*deliverySub{},
		p0:      map[string]struct{}{},
	}
}

// start runs the hub's goroutines once.
func (h *deliveryHub) start() {
	h.once.Do(func() {
		listener := &deliveryListener{
			dial: h.dial, listenIdle: h.listenIdle, pingTimeout: h.pingTimeout, backoff: h.backoff,
			onConnected: func() { signal(h.connected) },
			onNotify:    h.onNotify,
		}
		h.running.Add(3)
		go func() { defer h.running.Done(); listener.run(h.lifetime) }()
		go func() { defer h.running.Done(); h.intake(h.lifetime) }()
		go func() { defer h.running.Done(); h.work(h.lifetime) }()
	})
}

// wait returns once every goroutine the hub started has, after its lifetime ended.
func (h *deliveryHub) wait() { h.running.Wait() }

// signal leaves one signal on wake unless one is already there.
func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// readContext is a context for one read of the shared pool, marked so the pool refuses it a second
// connection.
func (h *deliveryHub) readContext() context.Context {
	return store.WithTransactionTracking(h.lifetime)
}

func (h *deliveryHub) note(n deliveryHubNote) {
	if h.observe != nil {
		h.observe(n)
	}
}

// onNotify routes one notification, on the listener's goroutine: a change or an event wakes the
// intake's catch-up of its log, and a freshness row is applied at once, reading nothing.
func (h *deliveryHub) onNotify(channel, payload string) {
	switch channel {
	case deliveryChangesChannel:
		signal(h.wakeChanges)
	case dispatchEventsChannel:
		signal(h.wakeEvents)
	case deliveryFreshnessChannel:
		h.applyFreshness(payload)
	}
}

// catchUpNow catches both logs up and returns once it has, or once the hub's lifetime ended.
func (h *deliveryHub) catchUpNow() {
	done := make(chan struct{})
	select {
	case h.barrier <- done:
		<-done
	case <-h.lifetime.Done():
	}
}

// deliveryLiveRefusal is the error frame refusing sub with code.
func deliveryLiveRefusal(sub, code, message string) DeliveryLiveError {
	return DeliveryLiveError{Type: "error", Sub: sub, Code: code, Message: message}
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func equalStringPointers(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
