package docs

import (
	"context"
	"sync"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// elementMeasurer counts the elements of the renderings rooms publish for the room budget, which
// charges a room by them (roomCharge), off every room's update observer. The count is a whole
// goldmark parse of the rendering (pmdoc.MeasureDocument): run in the observer, whose turn every
// browser in the room waits behind before ygo relays the keystroke (AGENTS.md:527-530), it cost
// about 89 ms of every keystroke that changed a 524 KiB rendering and raised the keystroke's p50 by
// 29 to 36 per cent (BenchmarkKeystrokeBesideReads). The rendering's bytes are a len the observer
// publishes itself (renderedReplica.publishSize), which is all the peer update hook weighs; only
// the count comes here, and for a room at most once each roomCountInterval after its load's own.
//
// There is one for the whole service (Service.measurer), and it runs one count at a time: the
// parse's own heap, up to about 130 MiB for a mebibyte of the heaviest shape (pmdoc/elements.go:31-37),
// is paid once for the pod rather than once for each room typed into at the same moment, and the
// pod's memory is what this count is for. A room has at most one rendering waiting
// (renderedReplica.uncounted), which its next publication replaces rather than queues behind, so a
// room typed into faster than it is counted is counted at its latest rendering, never at each one.
// A room whose last count is younger than the interval waits out the rest of it before it joins
// the queue (enqueueAfter); the queue serves rooms in the order they joined it.
//
// What it holds of a room, and for how long: queue, and the timer a room waits out the interval
// on, hold each waiting room's replica weakly, so a retired room's replica - a whole copy of its
// document - is collected with that document whatever is queued, and its entry is dropped unread
// when its turn comes. A waiting rendering is held by the replica itself, so it goes with the
// replica too. The measurer takes the rendering off the replica when the count starts and holds
// that string, and the replica weakly, for the one count alone; it holds the replica strongly only
// to take the rendering and the room's struct total and to store the result, never across the
// parse. The struct total holds the room's live document weakly, and the walk of it holds the
// document and its encoded state only while it runs. So what of a room it can keep past the room's
// retirement is the rendering of the count it is in, and the document while that count's walk
// runs, until that count ends.
//
// The one count at a time is a turn the counts take (turn), and the same turn runs the decode-cap
// confirmations of the server's writes outside applyLive (Service.pastDecodeCap), for the same
// reason: each decodes a whole room's state into a fresh copy of the document. A confirmation must
// answer before its write goes on, where a count is queued and stored later, so it does not join
// the queue: it takes the turn on its writer's goroutine and waits for it (decodes). Both hold
// turn, a mutex rather than a place in the queue, because a queued confirmation would need the
// worker started for a job no replica owns and a channel back to its writer, while the mutex keeps
// the measurer's zero value ready and leaves stop as it is: a confirmation is never queued, so a
// stop has nothing of it to drop, and one in flight holds nothing but its own copy, which goes when
// it returns.
type elementMeasurer struct {
	// turn is held for each count's parse and struct walk and each confirmation's decode, so that
	// no two run at once. No one holding it waits on a room: a count parses a string it already
	// holds, and a struct walk or a confirmation takes only the lock of the document it encodes,
	// which its caller does not hold.
	turn sync.Mutex

	mu sync.Mutex
	// queue lists, once each, the replicas whose latest rendering waits to be counted.
	queue []weak.Pointer[renderedReplica]
	// idle is non-nil while a worker runs, and closed once it has stopped, so Shutdown (stop)
	// and a test (waitIdle) can wait for the count in flight rather than sleep past it.
	idle chan struct{}
	// delayed counts the rooms waiting out the interval before they join the queue.
	delayed int
	// stopped is set by Shutdown (stop): no worker starts after it, and the one running stops
	// after the count it is in.
	stopped bool
	// interval replaces roomCountInterval when it is set. Zero outside tests; tests set it short,
	// to count a rendering as it is published or to watch the interval hold.
	interval time.Duration
	// beforeCount runs in the worker once it has taken a rendering and before it parses it. Nil
	// outside tests; tests use it to count the parses and to hold one while renderings arrive.
	beforeCount func(markdown string)
}

// minInterval is the least time between two counts of one room: roomCountInterval, or a test's.
func (m *elementMeasurer) minInterval() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.interval != 0 {
		return m.interval
	}
	return roomCountInterval
}

// enqueueAfter lists replica once wait has passed, the rest of the interval since its room's last
// count, or at once when none is left. The timer holds the replica weakly, as the queue does, and
// one that fires after the measurer stopped lists nothing.
func (m *elementMeasurer) enqueueAfter(replica weak.Pointer[renderedReplica], wait time.Duration) {
	if wait <= 0 {
		m.enqueue(replica)
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.delayed++
	m.mu.Unlock()
	time.AfterFunc(wait, func() {
		m.mu.Lock()
		m.delayed--
		m.mu.Unlock()
		m.enqueue(replica)
	})
}

// enqueue lists replica as having a rendering to count, and starts the worker if none runs. The
// worker is started for the work and exits once the queue is empty, so a service with nothing to
// count runs no goroutine for it, and one never shut down (a test's) leaves none behind.
func (m *elementMeasurer) enqueue(replica weak.Pointer[renderedReplica]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.queue = append(m.queue, replica)
	if m.idle == nil {
		m.idle = make(chan struct{})
		go m.run(m.idle)
	}
}

// run counts the queued renderings one after another until the queue is empty or the measurer
// stops, then closes idle.
func (m *elementMeasurer) run(idle chan struct{}) {
	defer close(idle)
	for {
		replica, ok := m.next()
		if !ok {
			return
		}
		m.count(replica)
	}
}

// next takes the queue's first replica, or reports that the worker is done: the queue is empty, or
// the measurer has stopped. Done, it gives up idle under mu, so an enqueue from then on starts a
// worker of its own.
func (m *elementMeasurer) next() (weak.Pointer[renderedReplica], bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || len(m.queue) == 0 {
		m.queue = nil
		m.idle = nil
		return weak.Pointer[renderedReplica]{}, false
	}
	replica := m.queue[0]
	m.queue[0] = weak.Pointer[renderedReplica]{}
	m.queue = m.queue[1:]
	return replica, true
}

// count counts the rendering replica has waiting, stores the count where the room budget reads it,
// and re-bases the room's charge on it. In the same turn it counts the room's struct total afresh
// (structTotal.recountLive), an encode of the live document's state and a walk of it, which
// re-bases the total the peer update hook refuses past the decode cap on and nothing else. A
// replica already collected, or one whose waiting rendering a later publication withdrew (one the
// room could not read), has nothing to count. The parse and the walk run in the measurer's turn.
func (m *elementMeasurer) count(replica weak.Pointer[renderedReplica]) {
	markdown, publication, updates := takeUncounted(replica)
	if markdown == nil {
		return
	}
	if m.beforeCount != nil {
		m.beforeCount(*markdown)
	}
	var structs *structTotal
	if held := replica.Value(); held != nil {
		structs = held.structs
	}
	m.turn.Lock()
	elements := countElements(*markdown)
	structs.recountLive()
	m.turn.Unlock()
	if held := replica.Value(); held != nil {
		held.storeCounted(int64(len(*markdown)), elements, publication, updates)
	}
}

// decodes reports whether doc's whole state decodes as one update (stateDecodes), once it has
// waited for the measurer's turn: the decode-cap confirmation of a server write outside applyLive
// (Service.pastDecodeCap), which holds a whole copy of doc while it runs.
func (m *elementMeasurer) decodes(doc *crdt.Doc) bool {
	m.turn.Lock()
	defer m.turn.Unlock()
	return stateDecodes(doc)
}

// stop ends the measurer for Shutdown: no worker starts after it, the queue is dropped, a room
// waiting out the interval joins nothing, and the worker running, if any, stops after the count it
// is in, which stop waits for within ctx. That is all a stop needs: the measurer writes nothing but
// a replica's atomic and its room's charge, and holds nothing but what a count reads, so a count
// cut off by ctx ending loses only a number no room will read again.
func (m *elementMeasurer) stop(ctx context.Context) {
	m.mu.Lock()
	m.stopped = true
	m.queue = nil
	idle := m.idle
	m.mu.Unlock()
	if idle == nil {
		return
	}
	select {
	case <-idle:
	case <-ctx.Done():
	}
}

// takeUncounted takes the rendering replica has waiting to be counted, with the publication that
// made it and the update bytes its room had taken then, marks the replica as no longer queued, so
// its next rendering queues it again, and starts the room's interval from this count. It holds the
// replica only for that.
func takeUncounted(replica weak.Pointer[renderedReplica]) (*string, uint64, int64) {
	r := replica.Value()
	if r == nil {
		return nil, 0, 0
	}
	r.sizeMu.Lock()
	defer r.sizeMu.Unlock()
	markdown := r.uncounted
	r.uncounted = nil
	r.queued = false
	if markdown != nil {
		r.countedAt = time.Now()
	}
	return markdown, r.uncountedAt, r.uncountedUpdates
}

// storeCounted stores elements, the count of the bytes-long rendering the replica's publication-th
// publication made when the room had taken updates bytes of updates, and re-bases the room's charge
// on its price (copyHeap, roomCharge.measured), unless a later publication has already stored a
// count of its own: the room's load, or a rendering the room could not read. Those are newer than
// this count, and a count landing after them would put an older document's weight back on the
// room. The update bytes it clears are those the room had taken when the counted rendering was
// published, never the bytes taken since, which the rendering does not hold.
func (r *renderedReplica) storeCounted(bytes, elements int64, publication uint64, updates int64) {
	r.sizeMu.Lock()
	defer r.sizeMu.Unlock()
	if publication > r.settled {
		r.elements.Store(elements)
		r.charge.measured(copyHeap(bytes, elements), updates)
	}
}

// countElements is the element half of markdown's measurement as the room budget charges it:
// elementsPastCounting for markdown goldmark stopped counting, which the budget prices from its
// bytes at the densest price instead (copyHeap). MeasureDocument answers such markdown with no
// elements at all, and charged as that, the heaviest room in the pod would look free.
func countElements(markdown string) int64 {
	size := pmdoc.MeasureDocument(markdown)
	if !size.Counted {
		return elementsPastCounting
	}
	return int64(size.Elements)
}
