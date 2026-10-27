package docs

// A document's compaction schedule: which documents are due a compaction (the due rule,
// checkCompaction), and the queue whose one worker compacts them, off every request goroutine
// (compactionQueue).

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// compactMinRows is how many rows older than compactTail a document has after its first row before
// a compaction folds them however young they are otherwise: a document being edited folds once its
// rows have left the tail in this many, not at each few.
const compactMinRows = 16

// compactMaxTailAge is the oldest a row after a document's first may grow before its document is
// due a compaction, however few rows it has: a document edited a little and left folds within this
// of its edit.
const compactMaxTailAge = 15 * time.Minute

// compactionTiming is how long the compaction queue waits between the steps of a room. Tests shorten
// it; production runs defaultCompactionTiming.
type compactionTiming struct {
	// firstCheck is how long a noted room waits for its first check. Nothing a note's own rows add
	// is due before they have left the tail, and a room that was due already waits at most this.
	firstCheck time.Duration
	// backoff is the wait before the first retry of a compaction that did not finish (dropped,
	// busy or failed), doubling at each retry up to backoffCap. It is also the shortest wait for a
	// re-check, so rows the database's clock puts at the boundary cannot spin the worker.
	backoff    time.Duration
	backoffCap time.Duration
}

var defaultCompactionTiming = compactionTiming{firstCheck: compactTail, backoff: time.Second, backoffCap: time.Minute}

// noteStoredUpdates tells the document's compaction queue that the room's rows from oldest to head
// are stored: an append's commit wrote them (appendUpdate, Ledger.Commit), or a load read them
// (PgVersioned.Load, LoadDocument). It queues the room for a check unless the queue already holds
// it, takes the queue's lock only for that, and never waits for a check or a compaction: every
// caller is a write or a read some request or room is waiting on.
//
// A room whose head is version 1 holds one row at most and has nothing to fold. The check reads the
// room's rows itself, since one commit or read sees only part of them - an append none below its
// own - so the range says nothing more to the queue.
func (s *Service) noteStoredUpdates(room string, head, oldest int64) {
	if head <= 1 {
		return
	}
	s.compactions.note(room)
}

// compactionCheck is what a check of a room read of its rows after the first, on the database's
// clock (checkCompaction).
type compactionCheck struct {
	// later is how many rows the room has after its first.
	later int64
	// due says the room is due a compaction: at least compactMinRows of those rows are older than
	// compactTail, or one is older than compactMaxTailAge.
	due bool
	// aged says one of them is older than compactTail, so a compaction would fold something.
	aged bool
	// untilDue is how long until the room is due, while it has rows after its first and is not.
	untilDue time.Duration
}

// checkCompaction reads, in one statement on the compaction pool, whether the room is due a
// compaction and, if not, when it will be: the earlier of its oldest row after the first leaving
// compactMaxTailAge and its compactMinRows-th oldest leaving compactTail. Both read created_at
// against the database's now(), the clock the compaction's fold point is chosen by (compactRule),
// and it reads the rows at or below the head that compaction folds under (headVersion).
func (s *Service) checkCompaction(ctx context.Context, room string) (compactionCheck, error) {
	pool, err := s.store.Pool.Compaction()
	if err != nil {
		return compactionCheck{}, err
	}
	var check compactionCheck
	var aged int64
	var seconds *float64
	if err := pool.QueryRow(ctx, `
		with later as (
			select created_at, row_number() over (order by created_at, version) as age_rank
			from doc_updates
			where artifact_id = $1 and version <= `+headVersion+`
				and version > (select min(version) from doc_updates where artifact_id = $1)
		)
		select count(*),
			count(*) filter (where created_at < now() - make_interval(secs => $2)),
			coalesce(min(created_at) < now() - make_interval(secs => $3), false),
			extract(epoch from least(
				min(created_at) + make_interval(secs => $3),
				min(created_at) filter (where age_rank = $4) + make_interval(secs => $2)
			) - now())::float8
		from later
	`, room, compactTail.Seconds(), compactMaxTailAge.Seconds(), int64(compactMinRows)).Scan(&check.later, &aged, &check.due, &seconds); err != nil {
		return compactionCheck{}, fmt.Errorf("check the document's compaction: %w", err)
	}
	check.aged = aged > 0
	check.due = check.due || aged >= compactMinRows
	if seconds != nil {
		check.untilDue = time.Duration(*seconds * float64(time.Second))
	}
	return check, nil
}

// compactionNextKind is what the compaction queue does with a room after one of its steps.
type compactionNextKind int

const (
	// compactionDone: the queue drops the room, until a note queues it again.
	compactionDone compactionNextKind = iota
	// compactionAgain: the room compacted, and is checked again at once.
	compactionAgain
	// compactionLater: the room is not due yet, and is checked again once it will be.
	compactionLater
	// compactionRetry: the room's compaction did not finish, and is retried after a backoff.
	compactionRetry
)

// compactionNext is what one step of a room decided: what next, and for compactionLater how long
// until the room is due.
type compactionNext struct {
	kind  compactionNextKind
	after time.Duration
}

// compactionStep is one step of the compaction queue's worker for a room: one check
// (checkCompaction) and, when it says so, one compaction under the tail rule that does not wait for
// the room's lock. A check compacts a room that is due; a retry, after a compaction that did not
// finish, compacts while the room still has a row older than compactTail after its first, and
// otherwise ends. A compaction that compacted is checked again; one another writer dropped, one that
// found the room's lock held, and an error are retried; one that found nothing to fold or rows that
// do not fold ends, the negative cache holding the latter (compactRoom).
func (s *Service) compactionStep(ctx context.Context, room string, retrying bool) compactionNext {
	check, err := s.checkCompaction(ctx, room)
	if err != nil {
		warnCompaction(ctx, "dispatch: check whether a document is due a compaction", room, err)
		return compactionNext{kind: compactionRetry}
	}
	switch {
	case retrying && !check.aged:
		return compactionNext{kind: compactionDone}
	case !retrying && !check.due && check.later == 0:
		return compactionNext{kind: compactionDone}
	case !retrying && !check.due:
		return compactionNext{kind: compactionLater, after: check.untilDue}
	}
	result, err := s.persistence.compactRoom(ctx, room, tailRule, false)
	if err != nil {
		warnCompaction(ctx, "dispatch: compact a document", room, err)
		return compactionNext{kind: compactionRetry}
	}
	switch result.outcome {
	case compactCompacted:
		return compactionNext{kind: compactionAgain}
	case compactDropped, compactBusy:
		return compactionNext{kind: compactionRetry}
	default:
		return compactionNext{kind: compactionDone}
	}
}

// warnCompaction logs a step's failure, unless it failed because the queue is stopping.
func warnCompaction(ctx context.Context, message, room string, err error) {
	if ctx.Err() == nil {
		slog.Warn(message, "room", room, "error", err)
	}
}

// compactionQueue is a Service's queue of the rooms it compacts, and its one worker (run), started
// with the Service (New). It holds a room once whatever notes it, ordered by when it is next
// checked, so it is bounded by the distinct rooms noted; the worker checks and compacts one room at
// a time, on the compaction pool. Shutdown stops it (shutDown, awaitStopped) and Quiesce pauses it
// (pause); a room either drops is left to a later note.
type compactionQueue struct {
	step   func(ctx context.Context, room string, retrying bool) compactionNext
	cancel context.CancelFunc
	// wake tells the worker the queue changed: a room was queued, or the queue stopped or resumed.
	wake chan struct{}
	// done is closed when the worker has returned.
	done chan struct{}
	// afterStep runs on the worker after each step with what the step decided and the backoff it
	// ran under, zero for a check. Nil outside tests; tests use it to see the steps a room takes.
	afterStep func(room string, retry time.Duration, next compactionNext)

	mu     sync.Mutex
	timing compactionTiming
	// rooms is every room the queue holds, the one in flight included; due orders those waiting.
	rooms map[string]*compactionEntry
	due   compactionHeap
	// current is the room in flight, and idle is closed once its step has ended.
	current *compactionEntry
	idle    chan struct{}
	// shut is set once Shutdown has begun: no note is taken and no step starts after it. paused is
	// set while Quiesce runs, and holds off both until Quiesce returns.
	shut   bool
	paused bool
}

// compactionEntry is one room the queue holds: when it is next checked, and the backoff its next
// step retries under, zero for a check.
type compactionEntry struct {
	room  string
	at    time.Time
	retry time.Duration
}

// startCompactionQueue creates a compaction queue whose worker runs step, and starts the worker.
func startCompactionQueue(step func(ctx context.Context, room string, retrying bool) compactionNext) *compactionQueue {
	ctx, cancel := context.WithCancel(store.WithTransactionTracking(context.Background()))
	q := &compactionQueue{
		step:   step,
		cancel: cancel,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		timing: defaultCompactionTiming,
		rooms:  make(map[string]*compactionEntry),
	}
	go q.run(ctx)
	return q
}

// note queues room for its first check, unless the queue holds it already or takes no notes.
func (q *compactionQueue) note(room string) {
	q.mu.Lock()
	if q.shut || q.paused || q.rooms[room] != nil {
		q.mu.Unlock()
		return
	}
	entry := &compactionEntry{room: room, at: time.Now().Add(q.timing.firstCheck)}
	q.rooms[room] = entry
	heap.Push(&q.due, entry)
	q.mu.Unlock()
	q.signal()
}

func (q *compactionQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// run is the worker: it waits for the earliest room's time, runs its step, and queues the room
// again or drops it as the step decided, until the queue is shut.
func (q *compactionQueue) run(ctx context.Context) {
	defer close(q.done)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		entry, wait, ok := q.take()
		if !ok {
			return
		}
		if entry == nil {
			if wait > 0 {
				timer.Reset(wait)
			}
			select {
			case <-q.wake:
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		next := q.step(ctx, entry.room, entry.retry > 0)
		if q.afterStep != nil {
			q.afterStep(entry.room, entry.retry, next)
		}
		q.settle(entry, next)
	}
}

// take takes the earliest room off the queue when its time has come. Otherwise it returns no room
// and how long until that time, zero when no room waits or the queue is paused; and false once the
// queue is shut.
func (q *compactionQueue) take() (*compactionEntry, time.Duration, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shut {
		return nil, 0, false
	}
	if q.paused || len(q.due) == 0 {
		return nil, 0, true
	}
	entry := q.due[0]
	if wait := time.Until(entry.at); wait > 0 {
		return nil, wait, true
	}
	heap.Pop(&q.due)
	q.current, q.idle = entry, make(chan struct{})
	return entry, 0, true
}

// settle ends the step of entry, the room in flight: it queues the room again as next says, unless
// the queue has shut or paused since, which drops it.
func (q *compactionQueue) settle(entry *compactionEntry, next compactionNext) {
	q.mu.Lock()
	defer q.mu.Unlock()
	close(q.idle)
	q.current, q.idle = nil, nil
	if next.kind == compactionDone || q.shut || q.paused {
		if q.rooms[entry.room] == entry {
			delete(q.rooms, entry.room)
		}
		return
	}
	timing := q.timing
	var after time.Duration
	switch next.kind {
	case compactionAgain:
		entry.retry = 0
	case compactionLater:
		entry.retry = 0
		after = max(next.after, timing.backoff)
	case compactionRetry:
		if entry.retry == 0 {
			entry.retry = timing.backoff
		} else {
			entry.retry = min(2*entry.retry, timing.backoffCap)
		}
		after = entry.retry
	}
	entry.at = time.Now().Add(after)
	heap.Push(&q.due, entry)
}

// dropQueuedLocked drops every room the queue holds but the one in flight, whose step drops it as
// it ends. Its caller holds q.mu.
func (q *compactionQueue) dropQueuedLocked() {
	for room, entry := range q.rooms {
		if entry != q.current {
			delete(q.rooms, room)
		}
	}
	q.due = nil
}

// shutDown begins Shutdown's stop of the queue: it takes no note from here and starts no step, and
// drops what it holds. A step in flight runs on, and the worker returns once it ends
// (awaitStopped).
func (q *compactionQueue) shutDown() {
	q.mu.Lock()
	q.shut = true
	q.dropQueuedLocked()
	q.mu.Unlock()
	q.signal()
}

// awaitStopped waits for the worker to return after shutDown, letting a step in flight finish
// within ctx and cancelling it once ctx ends. It always waits for the worker, so nothing of the
// queue uses the store's pools once it returns.
func (q *compactionQueue) awaitStopped(ctx context.Context) {
	select {
	case <-q.done:
	case <-ctx.Done():
		q.cancel()
		<-q.done
	}
}

// pause holds the queue for Quiesce: it takes no note and starts no step, waits within ctx for the
// step in flight to end, and drops what the queue holds. The returned resume takes notes again.
func (q *compactionQueue) pause(ctx context.Context) (resume func()) {
	q.mu.Lock()
	q.paused = true
	idle := q.idle
	q.mu.Unlock()
	if idle != nil {
		select {
		case <-idle:
		case <-ctx.Done():
		}
	}
	q.mu.Lock()
	q.dropQueuedLocked()
	q.mu.Unlock()
	return func() {
		q.mu.Lock()
		q.paused = false
		q.mu.Unlock()
		q.signal()
	}
}

// compactionHeap orders the rooms waiting in the queue by when each is next checked.
type compactionHeap []*compactionEntry

func (h compactionHeap) Len() int           { return len(h) }
func (h compactionHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h compactionHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *compactionHeap) Push(x any)        { *h = append(*h, x.(*compactionEntry)) }

func (h *compactionHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return entry
}
