package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	read          bool
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

// intake moves the hub's two cursors. It subscribes to the broker before it reads where the event
// log stands, so an event committed between the two reaches it either way, and the listener's
// connection (onConnected) catches both logs up after its LISTEN, for the same reason
// (api/events.go's subscribe-before-catch-up order).
func (h *deliveryHub) intake(ctx context.Context) {
	broker := h.server.deps.Events
	events, unsubscribe := broker.Subscribe()
	defer func() { unsubscribe() }()
	for attempt := 1; !h.readCursors(); attempt++ {
		timer := time.NewTimer(deliveryListenerBackoff(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	close(h.ready)
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wakeChanges:
			h.catchUp(true, false)
		case <-h.wakeEvents:
			h.catchUp(false, true)
		case <-h.connected:
			h.catchUp(true, true)
			h.note(deliveryHubNote{what: "connected"})
		case done := <-h.barrier:
			h.catchUp(true, true)
			close(done)
		case event, open := <-events:
			if !open {
				// A full buffer (broker.go's Publish) or CloseAll dropped the subscription: take a new
				// one, then read whatever the old one missed from the store.
				h.note(deliveryHubNote{what: "broker dropped"})
				events, unsubscribe = broker.Subscribe()
				h.catchUp(false, true)
				continue
			}
			h.receive(event)
		}
	}
}

// readCursors reads where both logs stand and the open P0 issues.
func (h *deliveryHub) readCursors() bool {
	ctx, pool := h.readContext(), h.server.deps.Store.Pool
	seq, err := delivery.LatestChangeSeq(ctx, pool)
	var at *time.Time
	if err == nil && seq > 0 {
		at, err = delivery.ChangeAt(ctx, pool, seq)
	}
	var eventID int64
	var eventAt time.Time
	if err == nil {
		eventID, eventAt, _, err = eventCursorCheck(ctx, pool, 0)
	}
	var p0 map[string]struct{}
	if err == nil {
		p0, err = delivery.OpenP0Keys(ctx, pool)
	}
	if err != nil {
		if h.lifetime.Err() == nil {
			slog.Warn("dispatch: delivery live hub could not read where the logs stand; retrying", "error", err)
		}
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changeSeq, h.changeAt = seq, timeOrZero(at)
	h.eventCursor, h.eventAt = eventID, eventAt
	h.p0 = p0
	return true
}

// catchUp catches up the change log, the event log, or both; a read that fails is tried again a
// second later.
func (h *deliveryHub) catchUp(changes, events bool) {
	if changes {
		if err := h.catchUpChanges(); err != nil && h.lifetime.Err() == nil {
			slog.Warn("dispatch: delivery live hub could not read the delivery changes; retrying", "error", err)
			time.AfterFunc(time.Second, func() { signal(h.wakeChanges) })
		}
	}
	if events {
		if err := h.catchUpEvents(); err != nil && h.lifetime.Err() == nil {
			slog.Warn("dispatch: delivery live hub could not read the events; retrying", "error", err)
			time.AfterFunc(time.Second, func() { signal(h.wakeEvents) })
		}
	}
}

// catchUpChanges reads every delivery change after the cursor, marking the views each can touch.
// Before each read it checks for a reset log: one holding no row as far as the cursor, or one whose
// row at the cursor was recorded at another time than the hub read (a TRUNCATE ... RESTART IDENTITY
// refilled past it, e2e/seed.ts:150). A reset moves the cursor to the newest row and marks every
// view dirty.
func (h *deliveryHub) catchUpChanges() error {
	ctx, pool := h.readContext(), h.server.deps.Store.Pool
	for {
		h.mu.Lock()
		seq, at := h.changeSeq, h.changeAt
		h.mu.Unlock()
		latest, err := delivery.LatestChangeSeq(ctx, pool)
		if err != nil {
			return err
		}
		reset := latest < seq
		if !reset && seq > 0 && h.clock.Now().Sub(at) < delivery.ChangeRetention {
			read, err := delivery.ChangeAt(ctx, pool, seq)
			if err != nil {
				return err
			}
			reset = read == nil || !read.Equal(at)
		}
		if reset {
			var latestAt *time.Time
			if latest > 0 {
				if latestAt, err = delivery.ChangeAt(ctx, pool, latest); err != nil {
					return err
				}
			}
			now := h.clock.Now()
			h.mu.Lock()
			h.changeSeq, h.changeAt = latest, timeOrZero(latestAt)
			h.epoch++
			h.dirtyAllLocked(now)
			h.mu.Unlock()
			return nil
		}
		changes, err := delivery.ChangesSince(ctx, pool, seq, deliveryLivePage)
		if err != nil {
			return err
		}
		now := h.clock.Now()
		h.mu.Lock()
		for _, change := range changes {
			for _, view := range h.views {
				if change.Seq > max(view.seq, view.readingSeq) && view.touchedBy(change, now) {
					h.markDirtyLocked(view, now, deliveryCauseChange)
				}
			}
			h.changeSeq, h.changeAt = change.Seq, change.At
		}
		h.mu.Unlock()
		if len(changes) < deliveryLivePage {
			return nil
		}
	}
}

// catchUpEvents reads every event after the cursor, acting on those the hub reads. It first checks
// for a reset log: one whose newest id is below the cursor, or whose row at the cursor is gone or
// was created at another time than the hub read (e2e/seed.ts:20 and :150 truncate events with
// RESTART IDENTITY). A reset moves the cursor to the newest event, reloads the open P0 issues and
// marks every view dirty; a cursor row some other path deleted costs only that.
func (h *deliveryHub) catchUpEvents() error {
	ctx, pool := h.readContext(), h.server.deps.Store.Pool
	h.mu.Lock()
	cursor, at := h.eventCursor, h.eventAt
	h.mu.Unlock()
	from := cursor
	maxID, maxAt, cursorAt, err := eventCursorCheck(ctx, pool, cursor)
	if err != nil {
		return err
	}
	if maxID < cursor || (cursor > 0 && (cursorAt == nil || !cursorAt.Equal(at))) {
		p0, err := delivery.OpenP0Keys(ctx, pool)
		if err != nil {
			return err
		}
		now := h.clock.Now()
		h.mu.Lock()
		h.eventCursor, h.eventAt, h.p0 = maxID, maxAt, p0
		h.dirtyAllLocked(now)
		h.mu.Unlock()
		h.note(deliveryHubNote{what: "events caught up", from: from, through: maxID})
		return nil
	}
	for {
		events, through, throughAt, err := liveEventsSince(ctx, pool, cursor, deliveryLivePage)
		if err != nil {
			return err
		}
		if through == cursor {
			break
		}
		now := h.clock.Now()
		h.mu.Lock()
		for _, event := range events {
			h.applyEventLocked(event, now)
		}
		h.eventCursor, h.eventAt = through, throughAt
		h.mu.Unlock()
		scanned := through - cursor
		cursor = through
		// Ids leave gaps, so a page that spans fewer ids than its limit scanned fewer rows: the log
		// ran dry.
		if scanned < deliveryLivePage {
			break
		}
	}
	h.note(deliveryHubNote{what: "events caught up", from: from, through: cursor})
	return nil
}

// receive acts on one event the broker published. Ids are allocated in commit order, so the event
// right after the cursor is acted on at once; any other leaves a gap the store fills, or follows
// a reset, which the catch-up's check tells.
func (h *deliveryHub) receive(event model.Event) {
	h.mu.Lock()
	next := event.ID == h.eventCursor+1
	h.mu.Unlock()
	if !next {
		h.catchUp(false, true)
		return
	}
	live := liveEvent{id: event.ID, at: event.CreatedAt, kind: event.Type, actor: event.Actor}
	if event.IssueKey != nil {
		live.issueKey = *event.IssueKey
	}
	if slices.Contains(deliveryLiveEventTypes, event.Type) {
		payload, err := json.Marshal(event.Payload)
		if err != nil {
			h.catchUp(false, true)
			return
		}
		live.payload = payload
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.applyEventLocked(live, now)
	h.eventCursor, h.eventAt = event.ID, event.CreatedAt
}

// applyEventLocked marks the views one event can touch: an issue write or claim the views whose
// issues or their ancestors hold the issue, and every timeline and measures view when the issue is
// a P0 before or after it (the measures' unowned P0 list); an architecture import every view; and
// a session's write every view that shows the session under another title than the one it wrote
// under.
func (h *deliveryHub) applyEventLocked(event liveEvent, now time.Time) {
	switch event.kind {
	case "issue.created", "issue.updated", "issue.closed":
		var issue struct {
			Key      string `json:"key"`
			Status   string `json:"status"`
			Priority *int   `json:"priority"`
		}
		key := event.issueKey
		if err := json.Unmarshal(event.payload, &issue); err == nil && issue.Key != "" {
			key = issue.Key
		}
		_, before := h.p0[key]
		after := issue.Priority != nil && *issue.Priority == 0 && issue.Status != "done"
		if after {
			h.p0[key] = struct{}{}
		} else {
			delete(h.p0, key)
		}
		h.dirtyIssueLocked(key, before || after, now)
	case "issue.claimed", "issue.released":
		// A claim carries no priority (IssueClaimEventPayload), so p0 answers for it.
		_, p0 := h.p0[event.issueKey]
		h.dirtyIssueLocked(event.issueKey, p0, now)
	case "architecture.synced":
		h.dirtyAllLocked(now)
	}
	if event.actor.Kind == "session" && event.actor.ID != "" {
		// The title delivery.ResolveSessionTitles reads: the newest event's, or the id when it
		// carries none.
		title := event.actor.ID
		if origin := event.actor.Origin; origin != nil && origin.SessionTitle != "" {
			title = origin.SessionTitle
		}
		for _, view := range h.views {
			if shown, ok := view.sessions[event.actor.ID]; ok && shown != title {
				h.markDirtyLocked(view, now, deliveryCauseChange)
			}
		}
	}
}

func (h *deliveryHub) dirtyIssueLocked(key string, p0 bool, now time.Time) {
	for _, view := range h.views {
		_, named := view.issues[key]
		_, ancestor := view.ancestors[key]
		if named || ancestor || (p0 && view.spec.kind != deliveryLivePipeline) {
			h.markDirtyLocked(view, now, deliveryCauseChange)
		}
	}
}

func (h *deliveryHub) dirtyAllLocked(now time.Time) {
	for _, view := range h.views {
		h.markDirtyLocked(view, now, deliveryCauseChange)
	}
}

// markDirtyLocked marks view for a recompute and wakes the worker. A subscribe's mark waits for no
// quiet.
func (h *deliveryHub) markDirtyLocked(view *deliveryView, now time.Time, cause string) {
	if !view.dirty {
		view.dirty, view.firstDirty, view.cause = true, now, cause
	}
	view.lastDirty = now
	if cause == deliveryCauseSubscribe {
		view.immediate, view.cause = true, cause
	}
	signal(h.wakeWork)
}

// touchedBy is whether a delivery change can move the view's answer (plan v4 Task 7 Step 7's
// table). The view's window is the one its last answer resolved, a preset's end the present; a
// change whose rows carry no time at all (a pull request never merged, a job of a run not stored)
// is taken as touching it.
func (view *deliveryView) touchedBy(change delivery.Change, now time.Time) bool {
	var times []time.Time
	for _, t := range []*time.Time{change.TOld, change.TNew} {
		if t != nil {
			times = append(times, *t)
		}
	}
	from, to := view.window(now)
	some := func(in func(time.Time) bool) bool { return slices.ContainsFunc(times, in) }
	atOrAfterFrom := func(t time.Time) bool { return !t.Before(from) }
	switch change.Kind {
	case "session_group", "settings":
		return true
	case "pull_request":
		switch view.spec.kind {
		case deliveryLiveTimeline:
			// The waiting line reads merges before the window, so every merge before its end can move it.
			return len(times) == 0 || some(func(t time.Time) bool { return t.Before(to) })
		case deliveryLiveMeasures:
			return len(times) == 0 || some(func(t time.Time) bool { return !t.Before(from) && t.Before(to) })
		default:
			return some(atOrAfterFrom)
		}
	case "run", "run_job":
		switch change.RunKind {
		case string(delivery.DeliveryRunKindPRChecks):
			return view.spec.kind == deliveryLivePipeline && (len(times) == 0 || some(atOrAfterFrom))
		case string(delivery.DeliveryRunKindDeploy):
			// The waiting line at the window's start reads deploy runs of any age.
			return view.spec.kind == deliveryLiveTimeline || len(times) == 0 || some(atOrAfterFrom)
		}
	}
	return true
}

// window is the view's window as its last answer resolved it: a custom range as named, a preset
// from its last start (or its length before now, before its first answer) to the present, and a
// pipeline view from the first shown day of its pinned at or of its last answer's.
func (view *deliveryView) window(now time.Time) (from, to time.Time) {
	switch {
	case view.spec.kind == deliveryLivePipeline:
		at := now
		if view.spec.at != nil {
			at = *view.spec.at
		} else if !view.at.IsZero() {
			at = view.at
		}
		return measures.WindowStart(at), time.Time{}
	case view.spec.preset > 0:
		if view.from.IsZero() {
			return now.Add(-view.spec.days()), now
		}
		return view.from, now
	default:
		return view.spec.from, view.spec.to
	}
}

// days is a preset's length.
func (spec deliveryLiveSpec) days() time.Duration {
	return time.Duration(spec.preset) * 24 * time.Hour
}

// work recomputes the dirty views and the expired ones, one at a time, and keeps the freshness
// verdict on its threshold. One timer, re-armed after every pass, wakes it at the earliest moment
// a view is due or the reconciled sources cross their threshold; nothing else wakes it but a mark.
func (h *deliveryHub) work(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-h.ready:
	}
	var timer hubTimer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		for h.runDue() {
		}
		now := h.clock.Now()
		h.mu.Lock()
		h.judgeLocked(now)
		next, ok := h.nextWakeLocked(now)
		h.mu.Unlock()
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if ok {
			timer = h.clock.AfterFunc(next.Sub(now), func() { signal(h.wakeWork) })
		}
		select {
		case <-ctx.Done():
			return
		case <-h.wakeWork:
		}
	}
}

// runDue recomputes, or narrows, the view that is due earliest, and is false when none is due.
func (h *deliveryHub) runDue() bool {
	now := h.clock.Now()
	h.mu.Lock()
	var due *deliveryView
	var dueAt time.Time
	var cause string
	for _, view := range h.views {
		at, why, ok := view.dueAt()
		if ok && !at.After(now) && (due == nil || at.Before(dueAt)) {
			due, dueAt, cause = view, at, why
		}
	}
	h.mu.Unlock()
	switch {
	case due == nil:
		return false
	case cause == deliveryCauseExpiry:
		h.expire(due)
	default:
		h.recompute(due, cause)
	}
	return true
}

// dueAt is when the view is next due, and why: a dirty view viewQuiet after its last mark or
// viewMaxWait after its first, whichever is sooner (at once for a subscribe's), not before a
// failed read's retry; a clean one with an answer at its expiry.
func (view *deliveryView) dueAt() (time.Time, string, bool) {
	if view.computing {
		return time.Time{}, "", false
	}
	if view.dirty {
		at := view.lastDirty.Add(viewQuiet)
		if latest := view.firstDirty.Add(viewMaxWait); latest.Before(at) {
			at = latest
		}
		if view.immediate {
			at = view.firstDirty
		}
		if at.Before(view.notBefore) {
			at = view.notBefore
		}
		return at, view.cause, true
	}
	if view.body != nil && !view.expiry.IsZero() {
		return view.expiry, deliveryCauseExpiry, true
	}
	return time.Time{}, "", false
}

// nextWakeLocked is the earliest moment after now a view is due or the verdict's reconciled
// sources cross their threshold.
func (h *deliveryHub) nextWakeLocked(now time.Time) (time.Time, bool) {
	var next time.Time
	consider := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, view := range h.views {
		if at, _, ok := view.dueAt(); ok {
			consider(at)
		}
	}
	if h.known && h.fresh.lastReconcileAt != nil {
		if crossing := h.fresh.lastReconcileAt.Add(deliveryStaleAfter).Add(time.Nanosecond); crossing.After(now) {
			consider(crossing)
		}
	}
	return next, !next.IsZero()
}

// deliveryViewRead is one recompute's read and compute.
type deliveryViewRead struct {
	seq               int64
	changedAt         *time.Time
	settings          delivery.DeliverySettings
	pop               deliveryPopulation
	pipe              pipelinePopulation
	from, at          time.Time
	body              deliveryLiveEncoded
	expiry            time.Time
	issues, ancestors map[string]struct{}
	sessions          map[string]string
	prs, runs         int
	unfetchable       int
}

// recompute answers one dirty view from a fresh read: the newest change position first, so a
// change committed during the read lands past it and marks the view again; then the GET route's
// population read, the ancestors of the issues it names, and the pure compute. It sends each sub
// the delta against what it holds (a snapshot to a sub with none), or DELIVERY_NOT_CONFIGURED to
// a sub not yet told.
func (h *deliveryHub) recompute(view *deliveryView, cause string) {
	started := time.Now()
	h.mu.Lock()
	view.dirty, view.immediate, view.computing = false, false, true
	spec, epoch := view.spec, h.epoch
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "recompute begin", view: spec.key, cause: cause, read: true})

	read, err := h.read(view, spec)

	now := h.clock.Now()
	h.mu.Lock()
	view.computing, view.readingSeq = false, 0
	sent := 0
	switch {
	case h.views[spec.key] != view:
		// Every sub left while it read.
	case errors.Is(err, delivery.ErrNoSettings):
		view.body, view.expiry, view.seq = nil, time.Time{}, read.seq
		for sub := range view.subs {
			if !sub.told {
				sub.conn.Send(deliveryLiveRefusal(sub.id, "DELIVERY_NOT_CONFIGURED",
					"delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery"))
				sub.told = true
				sent++
			}
		}
	case err != nil:
		if h.lifetime.Err() == nil {
			slog.Warn("dispatch: delivery live recompute failed; retrying", "view", spec.kind, "error", err)
		}
		if !view.dirty {
			view.dirty, view.firstDirty, view.cause = true, now, cause
		}
		view.lastDirty, view.notBefore = now, now.Add(deliveryLiveRetry)
	default:
		if h.epoch != epoch {
			// The change log was reset while it read, so its position names nothing in the new one.
			read.seq = 0
		}
		view.settings, view.pop, view.pipe, view.readFrom = read.settings, read.pop, read.pipe, read.from
		view.body, view.seq, view.changedAt = &read.body, read.seq, read.changedAt
		view.from, view.at, view.expiry = read.from, read.at, read.expiry
		view.issues, view.ancestors, view.sessions = read.issues, read.ancestors, read.sessions
		sent = h.sendLocked(view)
		if !h.known {
			// The first verdict is set silently, from the first settings read.
			h.fresh = deliveryFreshnessHeard{
				lastEventAt: read.settings.LastEventAt, lastReconcileAt: read.settings.LastReconcileAt,
				lastError: read.settings.LastError, unfetchable: read.unfetchable,
			}
			h.sent = h.verdictLocked(now)
			h.known = true
		} else {
			h.fresh.unfetchable = read.unfetchable
			h.judgeLocked(now)
		}
	}
	subscribers := len(view.subs)
	h.mu.Unlock()
	log.Printf("dispatch: delivery live recompute view=%s cause=%s read=%t elapsed_ms=%d prs=%d runs=%d subscribers=%d sent=%d",
		spec.kind, cause, true, time.Since(started).Milliseconds(), read.prs, read.runs, subscribers, sent)
	h.note(deliveryHubNote{what: "recompute end", view: spec.key, cause: cause, read: true, sent: sent})
}

// read is recompute's read and compute, outside the hub's lock.
func (h *deliveryHub) read(view *deliveryView, spec deliveryLiveSpec) (deliveryViewRead, error) {
	ctx, pool := h.readContext(), h.server.deps.Store.Pool
	var read deliveryViewRead
	var err error
	if read.seq, err = delivery.LatestChangeSeq(ctx, pool); err != nil {
		return read, err
	}
	h.mu.Lock()
	view.readingSeq = read.seq
	h.mu.Unlock()
	if read.seq > 0 {
		if read.changedAt, err = delivery.ChangeAt(ctx, pool, read.seq); err != nil {
			return read, err
		}
	}
	if read.settings, err = delivery.GetSettings(ctx, pool); err != nil {
		return read, err
	}
	now := h.clock.Now()
	var body any
	if spec.kind == deliveryLivePipeline {
		read.at = now
		if spec.at != nil {
			read.at = *spec.at
		}
		if read.pipe, err = h.server.readPipelinePopulation(ctx, read.settings, read.at); err != nil {
			return read, err
		}
		body = deliveryPipelineCompute(read.settings, read.pipe, read.at, now)
		if spec.at == nil {
			read.expiry = pipelineNextExpiry(read.pipe, now)
		}
		read.from = measures.WindowStart(read.at)
		read.prs, read.runs = len(read.pipe.leadTimes.prs), len(read.pipe.checks.Runs)+len(read.pipe.deploys.Runs)
		read.unfetchable = read.pipe.leadTimes.unfetchable
	} else {
		from, to := spec.from, spec.to
		if spec.preset > 0 {
			from, to = now.Add(-spec.days()), now
		}
		if read.pop, err = h.server.readDeliveryPopulation(ctx, read.settings, from, to, spec.query, spec.kind == deliveryLiveTimeline); err != nil {
			return read, err
		}
		read.issues = map[string]struct{}{}
		read.sessions = map[string]string{}
		name := func(issue *string, sessions []string) {
			if issue != nil {
				read.issues[*issue] = struct{}{}
			}
			for _, session := range sessions {
				read.sessions[session] = delivery.SessionTitle(read.pop.titles, session)
			}
		}
		for _, pr := range read.pop.prs {
			name(pr.IssueKey, pr.Sessions)
		}
		for _, pr := range read.pop.waiting {
			name(pr.IssueKey, pr.Sessions)
		}
		if read.ancestors, err = delivery.IssueAncestors(ctx, pool, slices.Sorted(maps.Keys(read.issues)), parentDepthCap); err != nil {
			return read, err
		}
		body = deliveryLiveCompute(spec.kind, read.settings, read.pop, from, to, now)
		if spec.preset > 0 {
			read.expiry = nextExpiry(read.pop, spec.days(), now)
		}
		read.from = from
		read.prs, read.runs = len(read.pop.prs), len(read.pop.applyRuns)+len(read.pop.windowRuns)
		read.unfetchable = read.pop.unfetchable
	}
	read.body, err = encodeDeliveryBody(spec.kind, body)
	return read, err
}

// expire answers a view whose preset window, or whose pipeline day, moved past something it holds,
// reading nothing: the compute over the population as last read, narrowed to the window now, with
// the settings that read held. The population and its read's start stay as read, and the next
// expiry comes from the narrowed population.
func (h *deliveryHub) expire(view *deliveryView) {
	started := time.Now()
	h.mu.Lock()
	view.computing = true
	spec, settings, pop, pipe := view.spec, view.settings, view.pop, view.pipe
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "recompute begin", view: spec.key, cause: deliveryCauseExpiry})

	now := h.clock.Now()
	var body any
	var from, at, expiry time.Time
	var prs, runs int
	if spec.kind == deliveryLivePipeline {
		at = now
		narrowed := pipe.narrowedTo(settings, at)
		body = deliveryPipelineCompute(settings, narrowed, at, now)
		expiry = pipelineNextExpiry(narrowed, now)
		from = measures.WindowStart(at)
		prs, runs = len(narrowed.leadTimes.prs), len(narrowed.checks.Runs)+len(narrowed.deploys.Runs)
	} else {
		from = now.Add(-spec.days())
		narrowed := pop.narrowedTo(settings, from)
		body = deliveryLiveCompute(spec.kind, settings, narrowed, from, now, now)
		expiry = nextExpiry(narrowed, spec.days(), now)
		prs, runs = len(narrowed.prs), len(narrowed.applyRuns)+len(narrowed.windowRuns)
	}
	encoded, err := encodeDeliveryBody(spec.kind, body)

	h.mu.Lock()
	view.computing = false
	sent := 0
	switch {
	case h.views[spec.key] != view:
	case err != nil:
		slog.Error("dispatch: delivery live expiry could not encode its body", "view", spec.kind, "error", err)
		view.expiry = time.Time{}
	default:
		view.body, view.from, view.at, view.expiry = &encoded, from, at, expiry
		sent = h.sendLocked(view)
	}
	subscribers := len(view.subs)
	h.mu.Unlock()
	log.Printf("dispatch: delivery live recompute view=%s cause=%s read=%t elapsed_ms=%d prs=%d runs=%d subscribers=%d sent=%d",
		spec.kind, deliveryCauseExpiry, false, time.Since(started).Milliseconds(), prs, runs, subscribers, sent)
	h.note(deliveryHubNote{what: "recompute end", view: spec.key, cause: deliveryCauseExpiry, sent: sent})
}

// deliveryLiveCompute is the timeline's or the measures' body for [from, to).
func deliveryLiveCompute(kind string, settings delivery.DeliverySettings, pop deliveryPopulation, from, to, now time.Time) any {
	if kind == deliveryLiveTimeline {
		return deliveryTimelineCompute(settings, pop, from, to, now)
	}
	return deliveryMeasuresCompute(settings, pop, from, to, now)
}

// sendLocked answers every sub of view from its body, and counts the frames it sent.
func (h *deliveryHub) sendLocked(view *deliveryView) int {
	sent := 0
	for sub := range view.subs {
		if h.answerLocked(sub) {
			sent++
		}
	}
	return sent
}

// answerLocked sends sub what it needs to hold its view's body: the snapshot, when it holds none,
// or the delta against its revisions, when anything differs.
func (h *deliveryHub) answerLocked(sub *deliverySub) bool {
	view := sub.view
	sub.told = false
	if sub.needsSnapshot {
		sub.conn.Send(view.body.snapshot(sub.id, view.seq, view.changedAt))
		sub.revs, sub.needsSnapshot = view.body.revs, false
		return true
	}
	delta, changed := diffDeliveryBody(*view.body, sub.revs)
	if !changed {
		return false
	}
	delta.Sub, delta.Seq, delta.ChangedAt = sub.id, strconv.FormatInt(view.seq, 10), view.changedAt
	sub.conn.Send(delta)
	sub.revs = view.body.revs
	return true
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

// applyFreshness applies one delivery_freshness payload, the settings row's freshness columns as
// migration 0099's trigger writes them, reading nothing.
func (h *deliveryHub) applyFreshness(payload string) {
	var row struct {
		LastEventAt     *time.Time `json:"last_event_at"`
		LastReconcileAt *time.Time `json:"last_reconcile_at"`
		LastError       *string    `json:"last_error"`
	}
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		slog.Warn("dispatch: delivery live hub could not read a freshness notification", "error", err)
		return
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fresh.lastEventAt, h.fresh.lastReconcileAt, h.fresh.lastError = row.LastEventAt, row.LastReconcileAt, row.LastError
	h.judgeLocked(now)
	// The threshold moved with the reconcile time: the worker re-arms its timer.
	signal(h.wakeWork)
}

// verdictLocked is the freshness verdict as of now.
func (h *deliveryHub) verdictLocked(now time.Time) deliveryFreshnessVerdict {
	stale := []string{}
	if last := h.fresh.lastReconcileAt; last != nil && now.Sub(*last) > deliveryStaleAfter {
		stale = append(stale, deliveryReconciledSources...)
	}
	return deliveryFreshnessVerdict{lastError: h.fresh.lastError, stale: stale, unfetchable: h.fresh.unfetchable}
}

// judgeLocked sends every open page a freshness frame when the verdict changed since the last one,
// and nothing otherwise; nothing at all before the first recompute set it.
func (h *deliveryHub) judgeLocked(now time.Time) {
	if !h.known {
		return
	}
	verdict := h.verdictLocked(now)
	if verdict.unfetchable == h.sent.unfetchable && slices.Equal(verdict.stale, h.sent.stale) &&
		equalStringPointers(verdict.lastError, h.sent.lastError) {
		return
	}
	h.sent = verdict
	frame := DeliveryLiveFreshness{
		Type: "freshness",
		Freshness: delivery.DeliveryFreshnessView{
			LastEventAt: h.fresh.lastEventAt, LastReconcileAt: h.fresh.lastReconcileAt,
			LastError: h.fresh.lastError, UnfetchableCount: h.fresh.unfetchable,
		},
		Stale: verdict.stale,
	}
	for conn := range h.conns {
		conn.Send(frame)
	}
}

// parseDeliveryLiveView resolves a subscribed view as the matching GET route reads its query, or
// answers the error frame refusing it: INVALID_FRAME for a view of no known shape, INVALID_QUERY
// for one the route would refuse.
func parseDeliveryLiveView(view DeliveryLiveView) (deliveryLiveSpec, *DeliveryLiveError) {
	invalid := func(code, message string) (deliveryLiveSpec, *DeliveryLiveError) {
		refusal := deliveryLiveRefusal("", code, message)
		return deliveryLiveSpec{}, &refusal
	}
	spec := deliveryLiveSpec{kind: view.Kind}
	switch view.Kind {
	case deliveryLiveTimeline, deliveryLiveMeasures:
		if view.At != nil {
			return invalid("INVALID_FRAME", "a "+view.Kind+" view names a window, not an at")
		}
		if view.Window == nil {
			return invalid("INVALID_FRAME", "a "+view.Kind+" view names its window")
		}
		spec.query = url.Values{}
		if view.Query != nil {
			query, err := url.ParseQuery(*view.Query)
			if err != nil {
				return invalid("INVALID_QUERY", "query does not parse: "+err.Error())
			}
			spec.query = query
		}
		// The window names from and to; the query is everything else the GET route reads.
		spec.query.Del("from")
		spec.query.Del("to")
		window := view.Window
		switch {
		case window.Preset != nil && window.From == nil && window.To == nil:
			if !slices.Contains(deliveryLivePresets, *window.Preset) {
				return invalid("INVALID_FRAME", "window.preset must be 7, 14, 28 or 84")
			}
			spec.preset = *window.Preset
			spec.key = view.Kind + "|preset:" + strconv.Itoa(spec.preset) + "|" + spec.query.Encode()
		case window.Preset == nil && window.From != nil && window.To != nil:
			from, to, err := parseDeliveryWindow(url.Values{"from": {*window.From}, "to": {*window.To}})
			if err != nil {
				return invalid("INVALID_QUERY", err.Error())
			}
			spec.from, spec.to = from.UTC(), to.UTC()
			spec.key = view.Kind + "|" + spec.from.Format(time.RFC3339Nano) + "," + spec.to.Format(time.RFC3339Nano) + "|" + spec.query.Encode()
		default:
			return invalid("INVALID_FRAME", "a window is a preset or a from and to")
		}
	case deliveryLivePipeline:
		if view.Window != nil || view.Query != nil {
			return invalid("INVALID_FRAME", "a pipeline view names at most an at")
		}
		spec.key = "pipeline|now|"
		if view.At != nil {
			at, err := parseOptionalRFC3339(url.Values{"at": {*view.At}}, "at", time.Time{})
			if err != nil {
				return invalid("INVALID_QUERY", err.Error())
			}
			at = at.UTC()
			spec.at = &at
			spec.key = "pipeline|" + at.Format(time.RFC3339Nano) + "|"
		}
	default:
		return invalid("INVALID_FRAME", "view.kind must be timeline, measures or pipeline")
	}
	return spec, nil
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
