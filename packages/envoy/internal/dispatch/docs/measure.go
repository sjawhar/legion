package docs

import (
	"context"
	"sync"
	"weak"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// elementMeasurer counts the elements of the renderings rooms publish past the element gate
// (elementGateBytes) for the peer update hook, off every room's update observer. The count is a
// whole goldmark parse of the rendering (pmdoc.MeasureDocument): run in the observer, whose turn
// every browser in the room waits behind before ygo relays the keystroke (AGENTS.md:527-530), it
// cost about 89 ms of every keystroke that changed a 524 KiB rendering and raised the keystroke's
// p50 by 29 to 36 per cent (BenchmarkKeystrokeBesideReads). The rendering's bytes are a len the
// observer publishes itself (renderedReplica.publishSize); only the count comes here.
//
// There is one for the whole service (Service.measurer), and it runs one count at a time: the
// parse's own heap, up to about 130 MiB for a mebibyte of the heaviest shape (pmdoc/elements.go:31-37),
// is paid once for the pod rather than once for each room typed into at the same moment, and the
// pod's memory is what this bound is for. A room has at most one rendering waiting
// (renderedReplica.uncounted), which its next publication replaces rather than queues behind, so a
// room typed into faster than it is counted is counted at its latest rendering, never at each one.
// Rooms are served in the order their first uncounted rendering arrived.
//
// What it holds of a room, and for how long: queue holds each waiting room's replica weakly, so a
// retired room's replica - a whole copy of its document - is collected with that document whatever
// is queued, and its entry is dropped unread when its turn comes. A waiting rendering is held by
// the replica itself, so it goes with the replica too. The measurer takes the rendering off the
// replica when the count starts and holds that string, and the replica weakly, for the one count
// alone; it holds the replica strongly only to take the rendering and to store the result, never
// across the parse. So the one thing of a room it can keep past the room's retirement is the
// rendering of the count it is in, until that count ends.
type elementMeasurer struct {
	mu sync.Mutex
	// queue lists, once each, the replicas whose latest rendering waits to be counted.
	queue []weak.Pointer[renderedReplica]
	// idle is non-nil while a worker runs, and closed once it has stopped, so Shutdown (stop)
	// and a test (waitIdle) can wait for the count in flight rather than sleep past it.
	idle chan struct{}
	// stopped is set by Shutdown (stop): no worker starts after it, and the one running stops
	// after the count it is in.
	stopped bool
	// beforeCount runs in the worker once it has taken a rendering and before it parses it. Nil
	// outside tests; tests use it to count the parses and to hold one while renderings arrive.
	beforeCount func(markdown string)
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

// count counts the rendering replica has waiting and stores the count where the hook loads it. A
// replica already collected, or one whose waiting rendering a later publication withdrew (one under
// the gate, or one the room could not read), has nothing to count.
func (m *elementMeasurer) count(replica weak.Pointer[renderedReplica]) {
	markdown, publication := takeUncounted(replica)
	if markdown == nil {
		return
	}
	if m.beforeCount != nil {
		m.beforeCount(*markdown)
	}
	elements := countElements(*markdown)
	if held := replica.Value(); held != nil {
		held.storeCounted(elements, publication)
	}
}

// stop ends the measurer for Shutdown: no worker starts after it, the queue is dropped, and the
// worker running, if any, stops after the count it is in, which stop waits for within ctx. That is
// all a stop needs: the measurer writes nothing but a replica's atomic and holds nothing but what a
// count reads, so a count cut off by ctx ending loses only a number no room will read again.
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
// made it, and marks the replica as no longer queued, so its next rendering past the gate queues it
// again. It holds the replica only for that.
func takeUncounted(replica weak.Pointer[renderedReplica]) (*string, uint64) {
	r := replica.Value()
	if r == nil {
		return nil, 0
	}
	r.sizeMu.Lock()
	defer r.sizeMu.Unlock()
	markdown := r.uncounted
	r.uncounted = nil
	r.queued = false
	return markdown, r.uncountedAt
}

// storeCounted stores elements, the count of the rendering the replica's publication-th publication
// made, unless a later publication has already stored a count of its own: one under the gate, the
// room's load, or a rendering the room could not read. Those are newer than this count, and a
// count landing after them would put an older document's weight back under the hook.
func (r *renderedReplica) storeCounted(elements int64, publication uint64) {
	r.sizeMu.Lock()
	defer r.sizeMu.Unlock()
	if publication > r.settled {
		r.elements.Store(elements)
	}
}

// countElements is the element half of markdown's measurement as the hook weighs it:
// elementsPastCounting for markdown goldmark stopped counting.
func countElements(markdown string) int64 {
	size := pmdoc.MeasureDocument(markdown)
	if !size.Counted {
		return elementsPastCounting
	}
	return int64(size.Elements)
}
