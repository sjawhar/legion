package cistore

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

// Publisher publishes a rendered summary envelope. *bus.Client satisfies this.
type Publisher interface {
	Publish(contracts.Envelope) error
}

// StartSummaryLoop runs the summary loop in a background goroutine until ctx is
// cancelled. Each pass emits one `pr.<n>.checks` envelope, carrying its SHA, for
// each commit of a pull request that has been quiet, all of whose recorded
// suites are complete and all of whose check runs are terminal - the pull
// request's current head or not: a head whose push carried GitHub's skip-checks
// trailer runs no CI, so the settlement that stands for it is the earlier
// head's, which can settle after the new head arrives. Consumers decide which
// SHA a settlement stands for. A record a head-gated listener left terminal and
// unsettled settles only within the handover grace (due, legacyBacklog). held is
// set at each pass to how many such records the pass held back (backlogHeld).
// The store's CAS makes the loop idempotent across replicas.
//
// Passes run only at the instants start + k*tick, where start is when the loop
// started. The first runs at k = 1 and looks at every cached record; each later
// one runs only at an instant by which a cached record changed (the store's
// change feed) or a record the loop waits on fell due (its debounce ran out, its
// claim went stale, or its handover grace ended). With nothing changed and
// nothing due the loop arms no timer. A record is attempted at the first instant
// at or after it falls due, and looked at again at the next instant after each
// attempt, so an attempt that did not settle it is retried there. A store
// feeds one loop at a time (attachSummary): the loop detaches from it once its
// context ends, and a loop started on the store after that starts afresh.
func StartSummaryLoop(ctx context.Context, store *Store, pub Publisher, debounce, tick time.Duration, held *metrics.Gauge, logger *logging.Logger) {
	if tick <= 0 {
		// A non-positive tick has no grid; it panics in the caller, as time.NewTicker does.
		panic("cistore: non-positive tick for StartSummaryLoop")
	}
	wake := store.attachSummary()
	start := time.Now()
	z := newSummarizer(store, debounce)
	backlog := &backlogHeld{gauge: held}
	go func() {
		// armed is the k of the instant start + k*tick the timer is set for, 0 while it is stopped.
		armed := int64(1)
		timer := time.NewTimer(tick)
		arm := func(k int64) {
			armed = k
			timer.Reset(time.Until(start.Add(time.Duration(k) * tick)))
		}
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				store.detachSummary()
				return
			case <-wake:
				if k := int64(time.Since(start)/tick) + 1; armed == 0 || k < armed {
					arm(k)
				}
			case <-timer.C:
				passed := armed
				armed = 0
				count, next, ok := z.pass(pub, logger, time.Now().UnixMilli())
				backlog.observe(count, logger)
				if ok {
					arm(max(passed+1, gridIndexAtOrAfter(start, tick, next)))
				}
			}
		}
	}()
}

// gridIndexAtOrAfter returns the least k for which start + k*tick is at or after the wall-clock
// millisecond at, which it places on the monotonic clock by its distance from now.
func gridIndexAtOrAfter(start time.Time, tick time.Duration, at int64) int64 {
	now := time.Now()
	since := now.Add(time.Duration(at-now.UnixMilli()) * time.Millisecond).Sub(start)
	k := int64(since / tick)
	if time.Duration(k)*tick < since {
		k++
	}
	return k
}

// backlogHeld reports the records a head-gated listener left that the summary loop holds back
// (heldBack): its gauge is set to each pass's count, and the first pass that holds any logs the
// count once, at INFO. The loop does not wait for the CI cache to load (cmd/listener starts it
// before the cache is ready), so that first count is a floor, and the line says "at least".
type backlogHeld struct {
	gauge  *metrics.Gauge
	logged bool
}

func (b *backlogHeld) observe(held int, logger *logging.Logger) {
	b.gauge.Set(int64(held))
	if held > 0 && !b.logged {
		b.logged = true
		logger.Info("checks held back a head-gated listener's unsettled records, at least", slog.Int("records", held))
	}
}

// claimFresh reports whether st carries a claim of its own generation made within 2*debounce of
// now, which no pass reclaims. A claim of an older generation is never fresh.
func claimFresh(st State, now int64, debounce time.Duration) bool {
	return st.Claim != nil &&
		st.Claim.Generation == st.Generation &&
		st.Claim.ClaimedAt >= now-(2*debounce).Milliseconds()
}

// verdict is what a pass does with a record (schedule).
type verdict int

const (
	// verdictIdle: the record is settled or not ready, and only a change makes it otherwise.
	verdictIdle verdict = iota
	// verdictWait: the record's verdict changes at a later millisecond.
	verdictWait
	// verdictHeld: the record is a head-gated listener's leftover (heldBack), which only a change
	// makes otherwise.
	verdictHeld
	// verdictAttempt: the pass tries to settle the record now.
	verdictAttempt
)

// schedule returns the verdict on st at now, in wall-clock milliseconds, and for verdictWait the
// first millisecond at which the verdict changes. A record is attempted exactly when it is due
// and carries no fresh claim, and held exactly when heldBack holds it.
func schedule(st State, now int64, debounce time.Duration) (verdict, int64) {
	if st.SettledEmitted || !settlementReady(st) {
		return verdictIdle, 0
	}
	if d := st.LastEventAt + debounce.Milliseconds(); now < d {
		return verdictWait, d
	}
	if legacyBacklog(st, now, debounce) {
		return verdictHeld, 0
	}
	if claimFresh(st, now, debounce) {
		at := st.Claim.ClaimedAt + (2 * debounce).Milliseconds() + 1
		if g := st.LastEventAt + (debounce + handoverGrace).Milliseconds(); st.Schema == 0 && g < at {
			at = g
		}
		return verdictWait, at
	}
	return verdictAttempt, 0
}

// queueEntry is a record's entry in the summarizer's queue: the pass at or after at (wall-clock
// milliseconds) looks at key again.
type queueEntry struct {
	at  int64
	key string
}

// deadlineQueue is a min-heap of queue entries ordered by (at, key), with one entry per key;
// index holds each queued key's position. It implements heap.Interface.
type deadlineQueue struct {
	entries []queueEntry
	index   map[string]int
}

func (q *deadlineQueue) Len() int { return len(q.entries) }

func (q *deadlineQueue) Less(i, j int) bool {
	a, b := q.entries[i], q.entries[j]
	return a.at < b.at || a.at == b.at && a.key < b.key
}

func (q *deadlineQueue) Swap(i, j int) {
	q.entries[i], q.entries[j] = q.entries[j], q.entries[i]
	q.index[q.entries[i].key] = i
	q.index[q.entries[j].key] = j
}

func (q *deadlineQueue) Push(x any) {
	entry := x.(queueEntry)
	q.index[entry.key] = len(q.entries)
	q.entries = append(q.entries, entry)
}

func (q *deadlineQueue) Pop() any {
	last := len(q.entries) - 1
	entry := q.entries[last]
	q.entries[last] = queueEntry{}
	q.entries = q.entries[:last]
	delete(q.index, entry.key)
	return entry
}

// set queues key at at, in place of its entry if it has one.
func (q *deadlineQueue) set(key string, at int64) {
	if i, ok := q.index[key]; ok {
		q.entries[i].at = at
		heap.Fix(q, i)
		return
	}
	heap.Push(q, queueEntry{at: at, key: key})
}

// remove drops key's entry, if it has one.
func (q *deadlineQueue) remove(key string) {
	if i, ok := q.index[key]; ok {
		heap.Remove(q, i)
	}
}

// summarizer is the summary loop's view of one store: the records waiting on time, queued by the
// millisecond their verdict changes, and the records it holds back. Every other cached record is
// idle until the store's change feed names it. A store feeds one summarizer at a time
// (attachSummary).
type summarizer struct {
	store    *Store
	debounce time.Duration

	// mu guards the fields below. No pass holds it, or the store's lock, while it settles a record.
	mu    sync.Mutex
	queue deadlineQueue
	held  map[string]struct{}
}

func newSummarizer(store *Store, debounce time.Duration) *summarizer {
	return &summarizer{
		store:    store,
		debounce: debounce,
		queue:    deadlineQueue{index: map[string]int{}},
		held:     map[string]struct{}{},
	}
}

// pass runs one pass at now (wall-clock milliseconds). It files each record the change feed names
// by its verdict, takes every queued record due by now, and attempts the due ones in key order,
// queueing each attempted record at now so the next pass looks at it again. It returns how many
// records it holds back (heldBack) and, when the queue is not empty, the earliest queued
// millisecond.
func (z *summarizer) pass(pub Publisher, logger *logging.Logger, now int64) (held int, next int64, ok bool) {
	z.mu.Lock()
	keys, reset := z.store.takeChanges()
	if reset {
		z.queue = deadlineQueue{index: map[string]int{}}
		z.held = map[string]struct{}{}
	}
	var batch []string
	for key := range keys {
		if _, attempt := z.fileLocked(key, now); attempt {
			batch = append(batch, key)
		}
	}
	for z.queue.Len() > 0 && z.queue.entries[0].at <= now {
		batch = append(batch, heap.Pop(&z.queue).(queueEntry).key)
	}
	z.mu.Unlock()

	sort.Strings(batch)
	for _, key := range batch {
		z.mu.Lock()
		st, attempt := z.fileLocked(key, now)
		z.mu.Unlock()
		if !attempt {
			continue
		}
		z.settle(pub, logger, key, st, now)
		z.mu.Lock()
		delete(z.held, key)
		z.queue.set(key, now)
		z.mu.Unlock()
	}

	z.mu.Lock()
	defer z.mu.Unlock()
	if z.queue.Len() > 0 {
		next, ok = z.queue.entries[0].at, true
	}
	return len(z.held), next, ok
}

// fileLocked reads key's cached record and files it by its verdict at now: a wait in the queue at
// the millisecond the verdict changes, a held record in held, an idle or uncached one in neither.
// For an attempt it returns the record and true, and files the key in neither.
func (z *summarizer) fileLocked(key string, now int64) (State, bool) {
	z.queue.remove(key)
	delete(z.held, key)
	st, cached := z.store.cachedState(key)
	if !cached {
		return State{}, false
	}
	switch v, at := schedule(st, now, z.debounce); v {
	case verdictWait:
		z.queue.set(key, at)
	case verdictHeld:
		z.held[key] = struct{}{}
	case verdictAttempt:
		return st, true
	}
	return State{}, false
}

// settle tries to settle the record cached under key at now: it reclaims a stale claim, claims the
// record, and publishes and marks its settlement, releasing the claim when the publication fails.
func (z *summarizer) settle(pub Publisher, logger *logging.Logger, key string, cached State, now int64) {
	store, debounce := z.store, z.debounce
	staleBefore := now - (2 * debounce).Milliseconds()
	if cached.Claim != nil {
		claimGeneration := cached.Claim.Generation
		if claimFresh(cached, now, debounce) {
			return
		}
		reclaimed, err := store.ReclaimSettlement(key, claimGeneration, staleBefore)
		if err != nil {
			logger.Warn("checks reclaim failed", slog.String("error", err.Error()), slog.String("sha", cached.SHA))
			return
		}
		if !reclaimed {
			return
		}
	}

	state, claimed, err := store.ClaimSettlement(key, cached.Hash(), cached.Generation, now, debounce)
	if err != nil {
		logger.Warn("checks claim failed", slog.String("error", err.Error()), slog.String("sha", cached.SHA))
		return
	}
	if !claimed {
		return
	}
	env, err := settlementEnvelope(state, contracts.NowMillis())
	if err != nil {
		logger.Error("checks payload failed", slog.String("error", err.Error()))
		return
	}
	if err := env.Validate(); err != nil {
		logger.Error("checks invalid envelope", slog.String("error", err.Error()))
		if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
			logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
		}
		return
	}
	held, err := store.ClaimStillHeld(key, state.Generation, state.Hash())
	if err != nil {
		logger.Warn("checks claim verification failed", slog.String("error", err.Error()), slog.String("sha", state.SHA))
		return
	}
	if !held {
		return
	}
	if err := pub.Publish(env); err != nil {
		if errors.Is(err, bus.ErrRefused) {
			// NATS refuses this settlement at every attempt, so the commit never settles from here: its
			// record is marked overflowed, as one past its bounds is, and the refusal logged once.
			logger.Error("checks settlement refused",
				slog.String("error", err.Error()),
				slog.String("topic", env.Topic),
				slog.String("sha", state.SHA),
			)
			if err := store.markOverflowed(key); err != nil {
				logger.Warn("checks overflow not recorded", slog.String("error", err.Error()), slog.String("sha", state.SHA))
			}
			return
		}
		logger.Warn("checks publish failed",
			slog.String("error", err.Error()),
			slog.String("topic", env.Topic),
			slog.String("sha", state.SHA),
		)
		if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
			logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
		}
		return
	}
	if _, err := store.MarkSettled(key, state.Generation); err != nil {
		logger.Warn("checks mark-settled failed", slog.String("error", err.Error()))
	}
}

// settlementEnvelope is the checks envelope that settles state, issued at issuedAt.
func settlementEnvelope(state State, issuedAt int64) (contracts.Envelope, error) {
	sum := renderSummary(state)
	payload, err := json.Marshal(sum)
	if err != nil {
		return contracts.Envelope{}, err
	}
	return contracts.Envelope{
		EventID:       id.New(),
		Source:        "github",
		SourceEventID: id.New(),
		Topic:         contracts.GithubSubject(state.Owner, state.Repo, "pr."+state.Number+".checks"),
		DedupeKey: fmt.Sprintf(
			"github.checks.%s/%s.pr.%s.%s.g%d",
			state.Owner,
			state.Repo,
			state.Number,
			state.SHA,
			state.Generation,
		),
		IssuedAt:       issuedAt,
		PayloadSummary: settledPayloadSummary(sum),
		Payload:        string(payload),
		TraceID:        id.New(),
	}, nil
}

func settledPayloadSummary(sum Summary) string {
	summary := fmt.Sprintf(
		"checks settled on %s#%s @ %s: %d passed, %d failed, %d cancelled, %d skipped",
		sum.Repo, sum.Number, sha7(sum.SHA), sum.Passed.Count, sum.Failed.Count,
		sum.Cancelled.Count, sum.Skipped.Count,
	)
	if sum.Failed.Count > 0 {
		summary += "; failing: " + strings.Join(sum.Failed.Checks, ", ")
	}
	if sum.SupersededSettlement == "true" {
		summary += " (re-settled)"
	}
	return contracts.OneLineSummary(summary)
}

func sha7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
