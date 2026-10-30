package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// intake moves g's change-log cursor and applies the relay's events, until g ends.
//
// ready closes once the change log's cursor is read. The relay subscription the events arrive on
// waits for a safe horizon (firstRelayCursor), which a transaction holding an id anywhere on the
// Postgres instance holds back for as long as it stays open, and neither the worker's answers nor
// the change log depend on it, so it is taken on a goroutine of its own (seedEvents) and handed to
// this one (applyFeed). Until it is, the hub acts on no event; the subscription's head, read
// before the open P0 issues it arrives with, is what leaves no gap: every event at or below the
// head was committed and is final before the reads that follow it (those P0 issues, each view's
// recompute) run, and every event above it arrives on the feed. The listener's connection
// (onConnected) catches the change log up after its LISTEN for the same reason.
func (h *deliveryHub) intake(g *hubGeneration) {
	var feed relaySubscription
	var held, open bool
	defer func() {
		if held {
			feed.cancel()
		}
	}()
	for attempt := 1; !h.readChangeCursor(g); attempt++ {
		if !waitFor(g.ctx, pglisten.RedialBackoff(attempt)) {
			return
		}
	}
	close(g.ready)
	for {
		var events <-chan relayedEvent
		if open {
			events = feed.events
		}
		select {
		case <-g.ctx.Done():
			return
		case taken := <-g.feeds:
			// The overflowed or ended subscription is cancelled only once the new one stands, so
			// the relay's generation runs on between the two.
			if held {
				feed.cancel()
			}
			feed, held, open = taken.feed, true, true
			h.applyFeed(g, taken)
		case <-g.wakeChanges:
			h.catchUp(g)
		case <-g.connected:
			h.catchUp(g)
			h.note(deliveryHubNote{what: "connected"})
		case done := <-g.barrier:
			h.catchUp(g)
			close(done)
		case relayed, more := <-events:
			if !more {
				// The feed fell its buffer behind, or the relay's generation ended (a reset of the
				// event log): events may have gone by unapplied. Subscribe again, then read the
				// open P0 issues afresh and recompute every view, which reads what the missed
				// events changed (applyFeed).
				open = false
				h.note(deliveryHubNote{what: "feed closed"})
				h.seedEvents(g)
				continue
			}
			h.receive(g, relayed.Event)
		}
	}
}

// hubEventFeed is a relay subscription seedEvents took for the intake, with the open P0 issues as
// they stood once it had.
type hubEventFeed struct {
	feed relaySubscription
	p0   map[string]struct{}
}

// seedEvents subscribes g to the relay's feed on a goroutine of its own, run on g's context, and
// hands it to the intake on g.feeds; until the intake takes it the hub acts on no event. It is
// the generation's first subscription and the one after a feed closed.
//
// The subscribe waits for the relay's first cursor when it starts a generation, which an open
// write anywhere on the instance can hold back, which is why it does not run on the intake's
// goroutine: the hub's ready, its delivery changes and its views' answers do not wait for the
// event horizon. The open P0 issues are read after the subscription stands, so they hold every
// event at or below its head, none of which arrives on the feed.
//
// A subscribe or a read that fails is tried again after the listener's redial backoff. The hand
// over gives up when g ends, so a feed the intake will never take leaves no goroutine behind for
// running to wait on, and no subscription the relay would keep its generation running for.
func (h *deliveryHub) seedEvents(g *hubGeneration) {
	h.running.Add(1)
	go func() {
		defer h.running.Done()
		for attempt := 1; ; attempt++ {
			feed, err := h.server.eventRelay.subscribe(g.readContext(), deliveryHubFeedBuffer)
			var p0 map[string]struct{}
			if err == nil {
				if p0, err = delivery.OpenP0Keys(g.readContext(), h.server.deps.Store.Pool); err != nil {
					feed.cancel()
				}
			}
			if err == nil {
				select {
				case g.feeds <- hubEventFeed{feed: feed, p0: p0}:
				case <-g.ctx.Done():
					feed.cancel()
				}
				return
			}
			if g.ctx.Err() != nil {
				return
			}
			slog.Warn("dispatch: delivery live hub could not take the live event stream's feed; retrying", "error", err)
			if !waitFor(g.ctx, pglisten.RedialBackoff(attempt)) {
				return
			}
		}
	}()
}

// applyFeed takes the feed seedEvents stood up, with the open P0 issues read with it, and marks
// every view dirty: a view computed before it read the database at a moment the hub cannot place
// against the feed's head, so it may have missed an event at or below it, which no feed will carry.
func (h *deliveryHub) applyFeed(g *hubGeneration, taken hubEventFeed) {
	now := h.clock.Now()
	h.mu.Lock()
	g.p0 = taken.p0
	h.dirtyAllLocked(g, now)
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "events seeded", eventID: taken.feed.head})
}

// readChangeCursor reads where the delivery change log stands.
func (h *deliveryHub) readChangeCursor(g *hubGeneration) bool {
	ctx, pool := g.readContext(), h.server.deps.Store.Pool
	seq, err := delivery.LatestChangeSeq(ctx, pool)
	var at *time.Time
	if err == nil && seq > 0 {
		at, err = delivery.ChangeAt(ctx, pool, seq)
	}
	if err != nil {
		if g.ctx.Err() == nil {
			slog.Warn("dispatch: delivery live hub could not read where the change log stands; retrying", "error", err)
		}
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	g.changeSeq, g.changeAt = seq, timeOrZero(at)
	return true
}

// catchUp catches the change log up; a read that fails is tried again a second later.
func (h *deliveryHub) catchUp(g *hubGeneration) {
	if err := h.catchUpChanges(g); err != nil && g.ctx.Err() == nil {
		slog.Warn("dispatch: delivery live hub could not read the delivery changes; retrying", "error", err)
		time.AfterFunc(time.Second, func() { signal(g.wakeChanges) })
	}
}

// catchUpChanges reads every delivery change after the cursor, marking the views each can touch.
// Before each read it checks for a reset log: one holding no row as far as the cursor, or one whose
// row at the cursor was recorded at another time than the hub read (a TRUNCATE ... RESTART IDENTITY
// refilled past it, e2e/seed.ts:150). A reset moves the cursor to the newest row and marks every
// view dirty.
func (h *deliveryHub) catchUpChanges(g *hubGeneration) error {
	ctx, pool := g.readContext(), h.server.deps.Store.Pool
	for {
		h.mu.Lock()
		seq, at := g.changeSeq, g.changeAt
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
			defer h.mu.Unlock()
			g.changeSeq, g.changeAt = latest, timeOrZero(latestAt)
			g.epoch++
			h.dirtyAllLocked(g, now)
			return nil
		}
		changes, err := delivery.ChangesSince(ctx, pool, seq, deliveryLivePage)
		if err != nil {
			return err
		}
		now := h.clock.Now()
		h.mu.Lock()
		for _, change := range changes {
			for _, view := range g.views {
				if change.Seq > max(view.seq, view.readingSeq) && view.touchedBy(change, now) {
					h.markDirtyLocked(g, view, now, deliveryCauseChange)
				}
			}
			g.changeSeq, g.changeAt = change.Seq, change.At
		}
		h.mu.Unlock()
		if len(changes) < deliveryLivePage {
			return nil
		}
	}
}

// receive applies one event of the relay's feed. The relay publishes every committed event once,
// in id order, so the hub keeps no cursor of the event log: a gap in what it receives is a feed
// that closed, which intake answers.
func (h *deliveryHub) receive(g *hubGeneration, event model.Event) {
	now := h.clock.Now()
	h.mu.Lock()
	h.applyEventLocked(g, event, now)
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "event", eventID: event.ID})
}

// applyEventLocked marks the views one event can touch: an issue write or claim the views whose
// issues or their ancestors hold the issue, and every timeline and measures view when the issue is
// a P0 before or after it (the measures' unowned P0 list); an architecture import every view; and
// a session's write every view that shows the session under another title than the one it wrote
// under. Those are every writer of a field the Delivery page shows (Task 0 Step 18: route has one
// writer, PATCH /api/v1/issues/{key}, whose event is issue.updated or issue.closed). The event is
// the relay's, shared with every other subscriber, and is only read.
//
// A view whose read is under way (reading) still holds its last answer's issues, ancestors and
// sessions (none before its first), so an issue or session event also records on it what the
// event touched, for recompute to check against the answer its read gives (recheckHeardLocked).
func (h *deliveryHub) applyEventLocked(g *hubGeneration, event model.Event, now time.Time) {
	var issueKey string
	if event.IssueKey != nil {
		issueKey = *event.IssueKey
	}
	switch event.Type {
	case "issue.created", "issue.updated", "issue.closed":
		var issue struct {
			Key      string `json:"key"`
			Status   string `json:"status"`
			Priority *int   `json:"priority"`
		}
		// An issue event's payload is the JSON the log holds (readEventRows).
		payload, _ := event.Payload.(json.RawMessage)
		key := issueKey
		if err := json.Unmarshal(payload, &issue); err == nil && issue.Key != "" {
			key = issue.Key
		}
		_, before := g.p0[key]
		after := issue.Priority != nil && *issue.Priority == 0 && issue.Status != "done"
		if after {
			g.p0[key] = struct{}{}
		} else {
			delete(g.p0, key)
		}
		h.dirtyIssueLocked(g, key, before || after, now)
	case "issue.claimed", "issue.released":
		// A claim carries no priority (IssueClaimEventPayload), so p0 answers for it.
		_, p0 := g.p0[issueKey]
		h.dirtyIssueLocked(g, issueKey, p0, now)
	case "architecture.synced":
		h.dirtyAllLocked(g, now)
	}
	if event.Actor.Kind == "session" && event.Actor.ID != "" {
		// The title delivery.ResolveSessionTitles reads: the newest event's, or the id when it
		// carries none.
		title := event.Actor.ID
		if origin := event.Actor.Origin; origin != nil && origin.SessionTitle != "" {
			title = origin.SessionTitle
		}
		for _, view := range g.views {
			if view.showsSessionRetitled(event.Actor.ID, title) {
				h.markDirtyLocked(g, view, now, deliveryCauseChange)
			}
			if view.reading {
				if view.heardSessions == nil {
					view.heardSessions = map[string]string{}
				}
				view.heardSessions[event.Actor.ID] = title
			}
		}
	}
}

func (h *deliveryHub) dirtyIssueLocked(g *hubGeneration, key string, p0 bool, now time.Time) {
	for _, view := range g.views {
		if view.showsIssue(key, p0) {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
		}
		if view.reading {
			if view.heardIssues == nil {
				view.heardIssues = map[string]bool{}
			}
			view.heardIssues[key] = view.heardIssues[key] || p0
		}
	}
}

// recheckHeardLocked marks view dirty when an event heard during its read touches the answer
// recompute has just installed, by the rules applyEventLocked marks with, and clears the record.
func (h *deliveryHub) recheckHeardLocked(g *hubGeneration, view *deliveryView, now time.Time) {
	for key, p0 := range view.heardIssues {
		if view.showsIssue(key, p0) {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
			break
		}
	}
	for session, title := range view.heardSessions {
		if view.showsSessionRetitled(session, title) {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
			break
		}
	}
	view.reading, view.heardIssues, view.heardSessions = false, nil, nil
}

// showsIssue is whether a write to the issue key, a P0 before or after it when p0, can move the
// view's answer: the view's issues or their ancestors hold it, or it is a timeline or measures
// view and the issue is a P0.
func (view *deliveryView) showsIssue(key string, p0 bool) bool {
	_, named := view.issues[key]
	_, ancestor := view.ancestors[key]
	return named || ancestor || (p0 && view.spec.kind != deliveryLivePipeline)
}

// showsSessionRetitled is whether the view shows session under another title than title.
func (view *deliveryView) showsSessionRetitled(session, title string) bool {
	shown, ok := view.sessions[session]
	return ok && shown != title
}

func (h *deliveryHub) dirtyAllLocked(g *hubGeneration, now time.Time) {
	for _, view := range g.views {
		h.markDirtyLocked(g, view, now, deliveryCauseChange)
	}
}

// markDirtyLocked marks view for a recompute and wakes g's worker. A subscribe's mark waits for no
// quiet.
func (h *deliveryHub) markDirtyLocked(g *hubGeneration, view *deliveryView, now time.Time, cause string) {
	if !view.dirty {
		view.dirty, view.firstDirty, view.cause = true, now, cause
	}
	view.lastDirty = now
	if cause == deliveryCauseSubscribe {
		view.immediate, view.cause = true, cause
	}
	signal(g.wakeWork)
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
