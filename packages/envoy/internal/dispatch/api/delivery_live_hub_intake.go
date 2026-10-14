package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

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
// is taken as touching timeline and measures views. Pipeline lead times read only merged pull
// requests (ListPullRequestsInWindow), so an unmerged pull request cannot move a Pipeline view.
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
