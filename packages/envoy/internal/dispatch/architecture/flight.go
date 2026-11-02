// flight.go holds the shared sync the callers of one architecture source join: the counter Close
// waits on, what a flight hands every caller that joined it, and the flight itself — the contexts
// it runs on and its errSuperseded retry chain. The Importer type, Sync and Close stay in
// importer.go: they are the type's own surface.
package architecture

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// syncCount counts the flights running and waits for none to be left. It has
// the shape of backgroundWork in api/server.go, and for its reason: a
// sync.WaitGroup is not it, because a flight may start while a wait is
// already waiting, and Add after Wait began is what a WaitGroup forbids.
// begin refuses once Close has begun, so a flight whose goroutine starts
// after Close took its decision does nothing rather than write into a
// closing pool.
type syncCount struct {
	mu      sync.Mutex
	closing bool
	running int
	// idle is closed when running last fell to zero, and replaced when a
	// flight starts again.
	idle chan struct{}
}

func (c *syncCount) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return false
	}
	if c.running == 0 {
		c.idle = make(chan struct{})
	}
	c.running++
	return true
}

func (c *syncCount) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running--
	if c.running == 0 {
		close(c.idle)
	}
}

// stop refuses every later flight and waits for the running ones to end.
func (c *syncCount) stop() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	for {
		c.mu.Lock()
		running, idle := c.running, c.idle
		c.mu.Unlock()
		if running == 0 {
			return
		}
		<-idle
	}
}

// syncFlight is one shared sync of one source: what the flight hands every
// caller that joined it, beside the flight's error. Every write to it happens
// in the flight's goroutine before the result is sent and every read after
// that receive, so the channel orders them: no mutex is needed, and -race is
// quiet.
type syncFlight struct {
	// startedAt is when the flight began, its very first act, so that even a
	// flight which does nothing answers a time no earlier than its callers'
	// and so ends their loop.
	startedAt time.Time
	// headAt is when the flight issued its branch-head lookup — the moment the
	// lookup went out, never the moment it answered, because the head may have
	// moved in between. A retry round stamps it again.
	headAt time.Time
	source model.ArchitectureSource
}

// servedAt is how fresh the flight's answer is: when it asked GitHub for the
// head, or, for a flight that failed before asking, when it began.
func (f *syncFlight) servedAt() time.Time {
	if f.headAt.IsZero() {
		return f.startedAt
	}
	return f.headAt
}

// flight is the shared sync singleflight runs for one source: one set of
// GitHub calls and one errSuperseded retry chain, however many callers joined
// it. ctx is whichever caller started it, and only its values are kept.
func (i *Importer) flight(ctx context.Context, project, repo string) (*syncFlight, error) {
	flight := &syncFlight{startedAt: time.Now()}
	if !i.running.begin() {
		return flight, i.owner.Err()
	}
	defer i.running.done()
	// The starter is one of several callers and may leave before the others,
	// so its cancellation is dropped: one hang-up fails nobody else's sync.
	detached := context.WithoutCancel(ctx)
	// A sync opens its own transactions on the shared pool, so it is marked like an API
	// request: a read it makes while one is open must come from that transaction, never from a
	// second connection (store.ErrNestedAcquire). The mark has to be the flight's own, because
	// WithoutCancel keeps the starter's values: sharing an HTTP request's mark would make
	// SyncProject's next Sync a nested acquire, and WithTransactionTracking, a no-op when a
	// mark is already present, would hand that shared one back.
	marked, err := store.ForConcurrentRead(detached)
	if err != nil {
		return flight, err
	}
	base, abandon := context.WithCancel(marked)
	defer abandon()
	// Nothing but the process stopping ends a flight early.
	stop := context.AfterFunc(i.owner, abandon)
	defer stop()
	// The deadline is the flight's own, counted from when it began rather than
	// from when its first caller arrived.
	syncCtx, cancel := context.WithTimeout(base, syncTimeout)
	defer cancel()
	for {
		source, err := i.syncOnce(syncCtx, base, project, repo, flight)
		if !errors.Is(err, errSuperseded) {
			flight.source = source
			return flight, err
		}
	}
}
