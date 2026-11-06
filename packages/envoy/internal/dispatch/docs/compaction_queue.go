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

// noteStoredUpdates tells the document's compaction queue that the room's rows through head are
// stored. Every committed append and every load of a document notes its room here, and these are
// all of them: an append on its own transaction (PgVersioned.appendUpdate), a room's own flush
// (PgVersioned.AppendRoomFlush), a joined transaction's live writes and flushes once it commits
// (Ledger.commit, through noteAppended), a settlement's and the block-id backfill's flushes and
// repairs once their transaction commits (commitSettlement), and a load once its read has ended
// (PgVersioned.loadStored). It queues the room for a check unless the queue already holds it, and
// has a room whose step is in flight queued again once that step ends (compactionQueue.note); it
// takes the queue's lock only for that, and never waits for a check or a compaction: every caller
// is a write or a read some request or room is waiting on.
//
// A room whose head is version 1 holds one row at most and has nothing to fold. The check reads the
// room's rows itself, since one commit or read sees only part of them, so the head says nothing
// more to the queue.
func (s *Service) noteStoredUpdates(room string, head int64) {
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
	var pastMaxAge bool
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
	`, room, compactTail.Seconds(), compactMaxTailAge.Seconds(), int64(compactMinRows)).Scan(&check.later, &aged, &pastMaxAge, &seconds); err != nil {
		return compactionCheck{}, fmt.Errorf("check the document's compaction: %w", err)
	}
	check.aged = aged > 0
	check.due = pastMaxAge || aged >= compactMinRows
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

// compactionTry is what one try at a room's compaction came to (tryCompaction), from which the
// queue's step and the sweep's pass each decide what they do with the room.
type compactionTry int

const (
	// tryNotDue: the check said not to compact the room, and no compaction ran; the check says
	// whether the room has rows after its first and when it will be due.
	tryNotDue compactionTry = iota
	// tryCompacted: the compaction folded the rows below its fold point into it.
	tryCompacted
	// tryLater: the check or the compaction failed, or the compaction found the room's lock held
	// (compactBusy) or its rows changed by another writer (compactDropped): the room is tried again
	// later.
	tryLater
	// tryDone: the compaction found nothing to fold, or rows that do not fold, which the negative
	// cache then holds (compactRoom).
	tryDone
)

// compactionAttempt is one try at a room's compaction: what it came to, the check it made, and how
// many rows its compaction deleted.
type compactionAttempt struct {
	try     compactionTry
	check   compactionCheck
	deleted int
}

// tryCompaction checks the room (checkCompaction) and, when the check says so, compacts it once
// under the tail rule without waiting for its lock (compactRoom), logging a failure of either. A
// try compacts a room that is due; a retry, after a compaction that did not finish, compacts while
// the room still has a row older than compactTail after its first.
func (s *Service) tryCompaction(ctx context.Context, room string, retrying bool) compactionAttempt {
	check, err := s.checkCompaction(ctx, room)
	if err != nil {
		warnCompaction(ctx, warnCompactionCheck, room, err)
		return compactionAttempt{try: tryLater}
	}
	if (retrying && !check.aged) || (!retrying && !check.due) {
		return compactionAttempt{try: tryNotDue, check: check}
	}
	result, err := s.persistence.compactRoom(ctx, room, tailRule, false)
	if err != nil {
		warnCompaction(ctx, warnCompactionCompact, room, err)
		return compactionAttempt{try: tryLater, check: check}
	}
	switch result.outcome {
	case compactCompacted:
		return compactionAttempt{try: tryCompacted, check: check, deleted: result.deleted}
	case compactDropped, compactBusy:
		return compactionAttempt{try: tryLater, check: check}
	default:
		return compactionAttempt{try: tryDone, check: check}
	}
}

// compactionStep is one step of the compaction queue's worker for a room: one try at its compaction
// (tryCompaction), a retry when the step before it did not finish. A compaction that compacted is
// checked again; one another writer dropped, one that found the room's lock held, and a failure are
// retried; one that found nothing to fold or rows that do not fold ends, the negative cache holding
// the latter (compactRoom). A room the try did not compact - not due, or for a retry no row after
// its first older than compactTail - ends when it has no row after its first and is checked again
// once it will be due otherwise, so a retry that comes too soon still comes back to rows that will
// come due.
func (s *Service) compactionStep(ctx context.Context, room string, retrying bool) compactionNext {
	attempt := s.tryCompaction(ctx, room, retrying)
	switch attempt.try {
	case tryCompacted:
		return compactionNext{kind: compactionAgain}
	case tryLater:
		return compactionNext{kind: compactionRetry}
	case tryDone:
		return compactionNext{kind: compactionDone}
	}
	if attempt.check.later == 0 {
		return compactionNext{kind: compactionDone}
	}
	return compactionNext{kind: compactionLater, after: attempt.check.untilDue}
}

// warnCompactionCheck and warnCompactionCompact are the messages a failed check and a failed
// compaction log, from the queue's steps and the sweep's pass alike (tryCompaction).
const (
	warnCompactionCheck   = "dispatch: check whether a document is due a compaction"
	warnCompactionCompact = "dispatch: compact a document"
)

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
	// renoted says a note of the room came while its step was in flight: the step may have read the
	// room before the noted rows were stored, so its end queues the room again (settle).
	renoted bool
	// index is the entry's place in the queue's heap (compactionHeap), -1 while it is not there.
	index int
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

// note queues room for its first check, firstCheck from now, unless the queue takes no notes. A
// note of the room in flight marks it (renoted), so its step's end queues it again however the step
// ended. A note of a room waiting in the queue brings its check forward to firstCheck from now when
// it was later: the rows the note announces come of age then, and a room checked quiet minutes ago
// may be waiting out its oldest row's compactMaxTailAge while a burst of appends stacks rows the
// compactMinRows rule would fold first.
func (q *compactionQueue) note(room string) {
	q.mu.Lock()
	if q.shut || q.paused {
		q.mu.Unlock()
		return
	}
	at := time.Now().Add(q.timing.firstCheck)
	if held := q.rooms[room]; held != nil {
		// A held room is either in flight or in the heap: take pops it and makes it current under
		// one lock, settle pushes it back or deletes it, and dropQueuedLocked deletes every room
		// but the current one before it empties the heap.
		moved := false
		switch {
		case held == q.current:
			held.renoted = true
		case at.Before(held.at):
			held.at = at
			heap.Fix(&q.due, held.index)
			moved = true
		}
		q.mu.Unlock()
		if moved {
			q.signal()
		}
		return
	}
	entry := &compactionEntry{room: room, at: at}
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
// the queue has shut or paused since, which drops it. A room noted while its step ran (renoted) is
// not dropped, whatever the step decided: it is queued for a check no later than firstCheck from
// now, the wait a first note gives it, or sooner as the step decided.
func (q *compactionQueue) settle(entry *compactionEntry, next compactionNext) {
	q.mu.Lock()
	defer q.mu.Unlock()
	close(q.idle)
	q.current, q.idle = nil, nil
	renoted := entry.renoted
	entry.renoted = false
	if q.shut || q.paused || (next.kind == compactionDone && !renoted) {
		if q.rooms[entry.room] == entry {
			delete(q.rooms, entry.room)
		}
		return
	}
	timing := q.timing
	if next.kind == compactionDone {
		next = compactionNext{kind: compactionLater, after: timing.firstCheck}
	}
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
	if renoted {
		after = min(after, timing.firstCheck)
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
func (h compactionHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}

func (h *compactionHeap) Push(x any) {
	entry := x.(*compactionEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}

func (h *compactionHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	entry.index = -1
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return entry
}
