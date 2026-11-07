package docs

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// maxQueuedDocumentWriters is how many writers may wait for one document's writer gate behind the
// one holding it; the next is refused ErrPreconditionBusy, which the API answers 429
// EDIT_QUEUE_FULL with Retry-After. A queued writer holds no connection and no admission unit, so
// the bound is not there to save either: it caps how long the last writer in the queue waits -
// each writer ahead of it runs a whole transaction on the document first - and how much memory a
// flood of retries aimed at one document can pin. 32 transactions of tens of milliseconds each is
// about a second, the Retry-After a refused writer is told to wait.
const maxQueuedDocumentWriters = 32

// ErrGateWhileHoldingConnection is a caller asking for a document's writer gate while it holds a
// pooled connection: its transaction, a cursor, an acquired connection. The writer holding the
// gate may itself be waiting for a connection, or for a row lock that caller holds, so the wait
// could close a cycle no timeout but the request's own would break. The gate is refused at once,
// as the pools refuse a second connection (store.ErrNestedAcquire); the write takes the gate
// before it opens its transaction.
var ErrGateWhileHoldingConnection = errors.New(
	"a caller holding a pooled connection asked for a document's writer gate: take the gate before the transaction opens",
)

// ErrWriterGateNotHeld is a live write joining a document (joinLiveWrite) whose writer gate its
// context does not carry. Every live write takes the gate first (AcquireDocumentWriter), so
// writers queue for a document holding no connection; one that skipped it would queue for the
// writer slot holding its transaction's connection and the owner row, which is what the gate is
// there to prevent.
var ErrWriterGateNotHeld = errors.New("live write to a document whose writer gate the caller does not hold")

// errGateWhileHoldingGate is a caller holding writer gates asking for more. Gates are taken in one
// sorted call so no two writers each hold a gate the other waits for; a second call would take
// its documents out of that order.
var errGateWhileHoldingGate = errors.New(
	"a caller holding document writer gates asked for more: take every document a write changes in one call",
)

// documentWriterGate admits one writer at a time per document, in arrival order, in this process.
//
// A live write holds its transaction's connection and its document's owner row from the moment
// it opens; before the gate, every writer queued on a busy document - for the owner row in
// Postgres, then for the in-memory writer slot - did so holding a connection, and writers clustered
// on a few documents (an issue's spec under review) took the whole request pool. With the gate,
// the queue is here, ahead of the transaction, and holds nothing: the writer at the head opens its
// transaction, the rest wait for it in memory. The gate is outermost in the lock order (see
// liveWrite): nothing that holds a database connection or lock waits for it, which
// AcquireDocumentWriter refuses at run time (ErrGateWhileHoldingConnection).
//
// The gate is per process. Writers on two processes still meet only at the database, at the owner
// row, as before it.
type documentWriterGate struct {
	mu sync.Mutex
	// gates holds an entry for each document whose gate is held, with the writers waiting for it
	// in order - nil until one waits; a document no writer holds or waits for has none.
	gates map[string]*list.List
	// queued counts the writers waiting, over every document, for the doc_writer_queued gauge.
	queued atomic.Int64
}

// gateWaiter is one writer waiting for a document's gate; ready closes when the gate passes to it.
type gateWaiter struct {
	ready chan struct{}
}

// acquire takes artifactID's gate, waiting behind its holder and the writers already queued, for
// as long as ctx lives. park runs once the caller is going to wait, before it does.
func (g *documentWriterGate) acquire(ctx context.Context, artifactID string, park func()) error {
	g.mu.Lock()
	waiters, held := g.gates[artifactID]
	if !held {
		if g.gates == nil {
			g.gates = make(map[string]*list.List)
		}
		g.gates[artifactID] = nil
		g.mu.Unlock()
		return nil
	}
	if waiters == nil {
		waiters = list.New()
		g.gates[artifactID] = waiters
	}
	if waiters.Len() >= maxQueuedDocumentWriters {
		g.mu.Unlock()
		return ErrPreconditionBusy
	}
	waiter := &gateWaiter{ready: make(chan struct{})}
	element := waiters.PushBack(waiter)
	g.queued.Add(1)
	g.mu.Unlock()

	park()
	select {
	case <-waiter.ready:
		return nil
	case <-ctx.Done():
	}
	g.mu.Lock()
	select {
	case <-waiter.ready:
		// The gate passed to this writer as its context ended: it is this writer's to pass on.
		g.mu.Unlock()
		g.release(artifactID)
	default:
		waiters.Remove(element)
		g.queued.Add(-1)
		g.mu.Unlock()
	}
	return ctx.Err()
}

// release passes artifactID's gate to the writer at the head of its queue, or drops the document
// when none waits.
func (g *documentWriterGate) release(artifactID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	waiters := g.gates[artifactID]
	if waiters == nil || waiters.Len() == 0 {
		delete(g.gates, artifactID)
		return
	}
	front := waiters.Front()
	waiters.Remove(front)
	g.queued.Add(-1)
	close(front.Value.(*gateWaiter).ready)
}

// documentWriterHold is the set of documents whose writer gates a context carries, sorted, until
// its release runs.
type documentWriterHold struct {
	artifactIDs []string
	released    atomic.Bool
}

type documentWriterKey struct{}

func (h *documentWriterHold) live() bool { return h != nil && !h.released.Load() }

// holdsDocumentWriter reports whether ctx carries artifactID's writer gate, not yet released.
func holdsDocumentWriter(ctx context.Context, artifactID string) bool {
	hold, _ := ctx.Value(documentWriterKey{}).(*documentWriterHold)
	if !hold.live() {
		return false
	}
	_, found := slices.BinarySearch(hold.artifactIDs, artifactID)
	return found
}

// AcquireDocumentWriter takes the writer gate (documentWriterGate) of every document in
// artifactIDs for one write, and returns ctx carrying them, which the write opens its transaction
// and joins its documents under (joinLiveWrite refuses a document whose gate the context does not
// carry), and the release that hands each gate to its next writer, which the caller runs once its
// transaction has committed and published, or rolled back, and before any post-commit work of
// its own. The release runs once however often it is called, so a caller that releases at its
// commit can defer it for its earlier returns too.
//
// The documents are deduplicated and taken in sorted order, so two writes to overlapping documents
// never each hold one the other waits for. A document whose gate already has
// maxQueuedDocumentWriters writers waiting refuses the write with ErrPreconditionBusy, and a
// caller holding a pooled connection is refused at once with ErrGateWhileHoldingConnection.
//
// A writer that has to wait gives its request's admission units back first
// (admission.Ticket.Park), so writes queued on one busy document do not hold every write unit
// from writes to other documents, and takes them back once it holds every gate it asked for
// (Resume). A Resume admission refuses releases the gates and returns admission's refusal, which
// the API answers as a request it had no room for. A writer whose context ends while it waits
// leaves the queue and returns the context's error.
func (s *Service) AcquireDocumentWriter(ctx context.Context, artifactIDs ...string) (context.Context, func(), error) {
	if store.ConnectionHeld(ctx) {
		return nil, nil, ErrGateWhileHoldingConnection
	}
	ids := slices.Compact(slices.Sorted(slices.Values(artifactIDs)))
	if len(ids) == 0 {
		return ctx, func() {}, nil
	}
	if hold, _ := ctx.Value(documentWriterKey{}).(*documentWriterHold); hold.live() {
		return nil, nil, errGateWhileHoldingGate
	}
	ticket := admission.TicketFrom(ctx)
	releaseTaken := func(taken []string) {
		for index := len(taken) - 1; index >= 0; index-- {
			s.writers.release(taken[index])
		}
	}
	for index, artifactID := range ids {
		if err := s.writers.acquire(ctx, artifactID, ticket.Park); err != nil {
			releaseTaken(ids[:index])
			if errors.Is(err, ErrPreconditionBusy) {
				return nil, nil, fmt.Errorf("%w: document %s", err, artifactID)
			}
			return nil, nil, err
		}
	}
	if err := ticket.Resume(ctx); err != nil {
		releaseTaken(ids)
		return nil, nil, err
	}
	hold := &documentWriterHold{artifactIDs: ids}
	release := sync.OnceFunc(func() {
		hold.released.Store(true)
		releaseTaken(ids)
	})
	return context.WithValue(ctx, documentWriterKey{}, hold), release, nil
}

// QueuedDocumentWriters is how many writers are waiting for a document's writer gate, over every
// document of this process.
func (s *Service) QueuedDocumentWriters() int64 {
	return s.writers.queued.Load()
}
