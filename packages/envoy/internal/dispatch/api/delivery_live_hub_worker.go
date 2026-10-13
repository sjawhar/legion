package api

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

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
	h.note(deliveryHubNote{what: "recompute begin", view: spec.key, cause: cause})

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
	h.finishRecompute(spec, cause, started, read.prs, read.runs, subscribers, sent)
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
	h.finishRecompute(spec, deliveryCauseExpiry, started, prs, runs, subscribers, sent)
}

// finishRecompute records one recompute's shared log line and test observation.
func (h *deliveryHub) finishRecompute(spec deliveryLiveSpec, cause string, started time.Time, prs, runs, subscribers, sent int) {
	log.Printf("dispatch: delivery live recompute view=%s cause=%s elapsed_ms=%d prs=%d runs=%d subscribers=%d sent=%d",
		spec.kind, cause, time.Since(started).Milliseconds(), prs, runs, subscribers, sent)
	h.note(deliveryHubNote{what: "recompute end", view: spec.key, cause: cause, sent: sent})
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
