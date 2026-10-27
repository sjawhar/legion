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
	"github.com/sjawhar/envoy/internal/pglisten"
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
// replaced. The first sub of a hub with none starts a generation (deliveryHub); once the server's
// lifetime has ended no generation starts and the frame is answered nothing, as the socket closes
// with 1012.
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
	var held *DeliveryLiveRevs
	if frame.Resume != nil {
		seq, err := strconv.ParseInt(frame.Resume.Seq, 10, 64)
		if err != nil || seq < 0 {
			conn.Send(deliveryLiveRefusal(frame.Sub, "INVALID_FRAME", "resume.seq must be a decimal change position"))
			return
		}
		// Read before the hub's lock, which no database read is made under.
		oldest, err := delivery.OldestChangeSeq(store.WithTransactionTracking(h.lifetime), h.server.deps.Store.Pool)
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
	g := h.generation
	if g == nil {
		if h.closed || h.lifetime.Err() != nil {
			return
		}
		g = h.startLocked()
	}
	subs := h.conns[conn]
	if subs == nil {
		subs = map[string]*deliverySub{}
		h.conns[conn] = subs
	}
	if replaced := subs[frame.Sub]; replaced != nil {
		// conn keeps a sub, so the generation runs on.
		h.leaveLocked(replaced)
	}
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
	subs[frame.Sub] = sub
	switch {
	case view.body != nil && !view.dirty && !view.computing:
		h.answerLocked(sub)
	case view.computing || view.dirty:
		// The recompute under way, or the one the view already waits for, answers it.
	default:
		h.markDirtyLocked(g, view, now, deliveryCauseSubscribe)
	}
}

// Unsubscribe drops conn's sub; a view's last sub drops the view, and the hub's last its generation.
func (h *deliveryHub) Unsubscribe(conn deliveryLiveConn, sub string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(conn, sub)
}

// Close drops every sub conn holds. The socket calls it however it ends: shutdown (1012), its
// caller no longer resolving (1008), a full queue (1013), a read error.
func (h *deliveryHub) Close(conn deliveryLiveConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.conns[conn] {
		h.dropLocked(conn, id)
	}
}

// dropLocked drops conn's sub id, the view it leaves without a sub, and conn once it holds none. The
// drop that leaves the hub no conn stops its generation: its listener closes its connection, its
// intake cancels its relay subscription, and nothing reads either log until the next Subscribe.
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
	h.leaveLocked(sub)
	if len(h.conns) == 0 && h.generation != nil {
		h.generation.stop()
		h.generation = nil
	}
}

// leaveLocked takes sub out of its view, and drops the view it leaves without a sub.
func (h *deliveryHub) leaveLocked(sub *deliverySub) {
	delete(sub.view.subs, sub)
	if len(sub.view.subs) == 0 && h.views[sub.view.spec.key] == sub.view {
		delete(h.views, sub.view.spec.key)
	}
}

// The channels the delivery hub listens on, and the event stream relay's. delivery_changes carries
// no payload and is notified as a delivery_changes row commits (migration 0100); delivery_freshness
// carries the settings row's freshness columns as JSON (0100). dispatch_events carries no payload
// and is notified as an event commits (0107); the relay alone listens on it (event_stream_relay.go),
// and the hub takes the events from the relay's feed.
const (
	deliveryChangesChannel   = "delivery_changes"
	deliveryFreshnessChannel = "delivery_freshness"
	dispatchEventsChannel    = "dispatch_events"
)

// deliveryHubFeedBuffer is the hub's slots on the relay's feed: more than one relay page
// (eventRelayPage), which the relay publishes at once, so a page never overflows an intake that
// was busy when it came.
const deliveryHubFeedBuffer = 1024

// deliveryHub keeps every open Delivery and Pipeline page current (LEGION-690). It learns of a
// delivery change from Postgres (LISTEN delivery_changes and delivery_freshness, on one connection
// of its own, pglisten.Listener) and of a committed event from the event stream relay's feed, the
// process's one reader of the event log; marks dirty only the views the change can touch;
// recomputes each dirty view once per burst, one recompute at a time, as the GET routes'
// population read and pure compute; re-runs the compute alone, reading nothing, when something
// leaves a preset window; and sends each subscriber only the keys whose revision differs from
// what it holds.
//
// Each run of the hub is a generation (hubGeneration): the first Subscribe of a hub with no sub
// starts one, and the drop of its last sub stops it, at once, with no grace period, so a process
// with no open page holds no connection for the hub and reads neither log for it. A generation
// runs three goroutines on a context of its own: the listener; the intake, which alone moves the
// change log's cursor, applies the relay's events and marks views dirty; and the worker, which
// alone recomputes, so one recompute runs at a time. Views and conns outlive no generation (the
// last conn's drop is what stops one), so a goroutine of a stopped generation could only reach the
// next generation's: each checks under mu that its generation is still the hub's before it reads
// or marks a view or sends a frame, and returns once its context ends.
type deliveryHub struct {
	server   *server
	clock    hubClock
	lifetime context.Context
	// feed subscribes to the event stream relay (eventStreamRelay.subscribe).
	feed func(ctx context.Context, buffer int) (relaySubscription, error)
	// dial, listenIdle, pingTimeout and backoff are the listener's (pglisten.Listener); dial is the
	// shared pool's Listener, and a test sets them otherwise before the first Subscribe.
	dial                    func(ctx context.Context) (*pgx.Conn, error)
	listenIdle, pingTimeout time.Duration
	backoff                 func(attempt int) time.Duration
	// observe, when set, is told of each recompute, connection and event; tests set it.
	observe func(deliveryHubNote)

	// running counts every generation's goroutines. Add runs under mu and only while closed is
	// false, so no Add can follow the Wait in wait.
	running sync.WaitGroup

	mu sync.Mutex
	// closed is set by wait: the lifetime has ended and no generation starts again.
	closed bool
	// generation is the running generation, nil while no conn holds a sub.
	generation *hubGeneration
	views      map[string]*deliveryView // by canonical view key
	conns      map[deliveryLiveConn]map[string]*deliverySub
}

// hubGeneration is one run of the hub. ctx, stop and the channels are set once at its start; the
// rest is guarded by the hub's mu.
type hubGeneration struct {
	ctx  context.Context
	stop context.CancelFunc
	// ready closes once the intake has subscribed to the relay's feed and read where the change log
	// stands, which the worker waits for.
	ready chan struct{}
	// Each wake holds at most one pending signal: what it asks for covers every signal before it.
	wakeWork, wakeChanges, connected chan struct{}
	barrier                          chan chan struct{}

	// changeSeq and changeAt are the last delivery change read and its at, to tell a reset log;
	// epoch counts the resets, so a recompute that read across one stores no change position.
	changeSeq int64
	changeAt  time.Time
	epoch     int64
	// p0 is the open P0 issues (OpenP0Keys), kept current from issue event payloads.
	p0 map[string]struct{}
	// fresh is the freshness row as last heard, and sent the verdict last sent; known is false
	// until the first recompute sets them, silently.
	fresh deliveryFreshnessHeard
	sent  deliveryFreshnessVerdict
	known bool
}

// hubClock is the hub's clock: what time it is, and a timer that calls f once the clock reaches
// at, at once when it already has. The timer names a moment rather than a wait, so a clock that
// moves between the hub's read of it and the arm (a test's, which moves forward without waiting)
// fires the timer all the same.
type hubClock interface {
	Now() time.Time
	At(at time.Time, f func()) hubTimer
}

type hubTimer interface{ Stop() bool }

// deliveryHubClock is the real clock.
type deliveryHubClock struct{}

func (deliveryHubClock) Now() time.Time { return time.Now().UTC() }

func (deliveryHubClock) At(at time.Time, f func()) hubTimer {
	return time.AfterFunc(time.Until(at), f)
}

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
	what    string // "recompute begin", "recompute read", "recompute end", "connected", "event", "feed closed"
	view    string // the view's key
	cause   string
	sent    int
	through int64 // an event's id
}

const (
	// A dirty view is recomputed once no change has marked it for viewQuiet, or viewMaxWait after
	// the first did, whichever comes first.
	viewQuiet   = 250 * time.Millisecond
	viewMaxWait = 2 * time.Second
	// deliveryLiveRetry is how long a view whose read failed waits before the next.
	deliveryLiveRetry = 5 * time.Second
	// deliveryLivePage is how many changes one catch-up read takes.
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

// newDeliveryHub is the hub of s's open pages, on s's event stream relay. Nothing runs until the
// first Subscribe, which starts a generation on lifetime.
func newDeliveryHub(s *server, lifetime context.Context, clock hubClock) *deliveryHub {
	return &deliveryHub{
		server: s, clock: clock, lifetime: lifetime,
		feed:  s.eventRelay.subscribe,
		dial:  s.deps.Store.Pool.Listener,
		views: map[string]*deliveryView{},
		conns: map[deliveryLiveConn]map[string]*deliverySub{},
	}
}

// startLocked runs a new generation's listener, intake and worker, with mu held.
func (h *deliveryHub) startLocked() *hubGeneration {
	ctx, stop := context.WithCancel(h.lifetime)
	g := &hubGeneration{
		ctx: ctx, stop: stop,
		ready:    make(chan struct{}),
		wakeWork: make(chan struct{}, 1), wakeChanges: make(chan struct{}, 1), connected: make(chan struct{}, 1),
		barrier: make(chan chan struct{}),
		p0:      map[string]struct{}{},
	}
	listener := &pglisten.Listener{
		Owner:       "dispatch",
		Dial:        h.dial,
		Channels:    []string{deliveryChangesChannel, deliveryFreshnessChannel},
		ListenIdle:  h.listenIdle,
		PingTimeout: h.pingTimeout,
		Backoff:     h.backoff,
		OnConnected: func() { signal(g.connected) },
		OnNotify:    func(channel, payload string) { h.onNotify(g, channel, payload) },
	}
	h.running.Add(3)
	go func() { defer h.running.Done(); listener.Run(ctx) }()
	go func() { defer h.running.Done(); h.intake(g) }()
	go func() { defer h.running.Done(); h.work(g) }()
	h.generation = g
	return g
}

// wait marks the hub closed, so no generation starts again, then returns once every generation's
// goroutines have. It is called after the lifetime ended, which ends them all.
func (h *deliveryHub) wait() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.running.Wait()
}

// signal leaves one signal on wake unless one is already there.
func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// readContext is a context for one of g's reads of the shared pool, which ends with g and is marked
// so the pool refuses it a second connection.
func (g *hubGeneration) readContext() context.Context {
	return store.WithTransactionTracking(g.ctx)
}

func (h *deliveryHub) note(n deliveryHubNote) {
	if h.observe != nil {
		h.observe(n)
	}
}

// onNotify routes one notification to g, on the listener's goroutine: a change wakes the intake's
// catch-up of the change log, and a freshness row is applied at once, reading nothing.
func (h *deliveryHub) onNotify(g *hubGeneration, channel, payload string) {
	switch channel {
	case deliveryChangesChannel:
		signal(g.wakeChanges)
	case deliveryFreshnessChannel:
		h.applyFreshness(g, payload)
	}
}

// catchUpNow catches the change log up and returns once it has, or once the running generation
// ended; with no generation running it returns at once.
func (h *deliveryHub) catchUpNow() {
	h.mu.Lock()
	g := h.generation
	h.mu.Unlock()
	if g == nil {
		return
	}
	done := make(chan struct{})
	select {
	case g.barrier <- done:
		<-done
	case <-g.ctx.Done():
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
