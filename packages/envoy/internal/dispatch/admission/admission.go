// Package admission decides whether the server has room for a request before the request takes
// any, and answers the requests it could not serve: refused here with a retryable 429, out of
// time or out of database connections with a retryable 503, rather than with an error.
//
// Without it every request queued, with no bound, for one pool of connections: past capacity the
// queue grew until clients gave up, and almost nothing succeeded. A Limiter admits a request only
// while its class has room, lets it wait for room a bounded time in a bounded queue, and refuses
// it at once otherwise, so the requests it admits finish and the rest are told to retry.
package admission

import (
	"container/list"
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Class is the kind of request a Limiter counts separately: reads and writes each have a limit
// of their own, so writers parked on a lock - the commit order makes writes wait on each other -
// can never take every connection from the readers, nor a flood of reads every one from writes.
type Class int

const (
	// Read is a GET or HEAD.
	Read Class = iota
	// Write is every other method.
	Write
	classCount
)

// ClassOf is the class of a request with method.
func ClassOf(method string) Class {
	if method == http.MethodGet || method == http.MethodHead {
		return Read
	}
	return Write
}

func (c Class) String() string {
	if c == Read {
		return "read"
	}
	return "write"
}

// Reason is why a Limiter refused a request.
type Reason string

const (
	// QueueFull is a request that arrived with its class's wait queue already full.
	QueueFull Reason = "queue_full"
	// WaitExceeded is a request that waited in the queue for Limits.Wait without room coming
	// free.
	WaitExceeded Reason = "wait_exceeded"
)

// Refusal is a request a Limiter did not admit. Nothing of the request ran, so its client may
// retry it whatever its method.
type Refusal struct {
	Class  Class
	Reason Reason
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("admission refused a %s request: %s", r.Class, r.Reason)
}

// The limits. The request pool (store.sharedPoolSize) holds 48 connections, and an admitted
// request holds at most one of them at a time, or Weight of them for a route that reads side by
// side. 8 of the 48 are left to the pool's users that admission does not count: the websockets'
// identity checks (whileCallerResolves), the browsers' durable document appends, the /auth routes
// and the GitHub proxy, and the work a handler leaves running after it has answered. The other 40
// are split so that writes, which wait on each other at the commit order, hold at most 12
// (MaxWriteLimit), and reads at most 28 (MaxReadLimit).
//
// Each class's limit follows the CPUs the process may use (runtime.GOMAXPROCS, which follows the
// container's CPU limit) up to its cap: readsPerCPU reads and writesPerCPU writes per CPU, at least
// one CPU's worth. Two CPUs, the production task's, already reach both caps, 28 reads and 12 writes,
// and so does every larger pod: at 16 CPUs the limits are still 28 and 12, because the request
// pool's 48 connections bound what admission may let in, so a larger pod's limit comes from the
// pool, not its cores. One CPU gets 14 reads and 6 writes.
//
// INFRA-589's reproduction on 2 CPUs chose them, with INFRA-591's commit-order change in, each step
// offered with the same seed (1x is 4 writes/s and 60 reads/s for 180 s, 10x is 40 and 600), as
// committed events/s at 10x and requests refused at 1x (reads / writes):
//
//	reads  writes  wait    queue     10x commits  1x refused
//	8      8       75 ms   1x limit  4.67         629 / 34
//	16     8       75 ms   1x limit  3.01         889 / 118
//	28     8       75 ms   1x limit  2.47         454 / 111
//	8      8       500 ms  1x limit  4.57         384 / 12
//	8      8       1 s     1x limit  4.91         638 / 6
//	8      8       1 s     8x limit  5.32         724 / 30
//	16     8       1 s     2x limit               112 / 25
//	28     8       1 s     2x limit  2.81         0 / 2
//	28     12      1 s     2x limit  2.92         0 / 5, and 9 / 11 on a second run
//
// Fewer reads lift 10x commits on this box - 8 reads committed 4.46 events/s at 10x against 28
// reads' 3.09 before INFRA-591 too - but refuse 4-6% of the reads at 1x, a load the server must
// serve whole. At 1x on the 2-core reproduction reads stall in bursts, mostly in garbage collection
// and system calls, with all 28 read slots busy for over a second, and a 1x write can be in flight
// for over a second, so 28 reads, 12 writes, a 1 s wait (DefaultWait) and a queue of twice the limit
// (queueFactor) keep 1x refusals to 0-9 reads and 5-11 writes in 180 s.
const (
	MaxReadLimit  = 28
	MaxWriteLimit = 12
	readsPerCPU   = 14
	writesPerCPU  = 6
)

// readLimit and writeLimit are the limits for procs usable CPUs: perCPU each, at least one CPU's
// worth and at most the class's cap.
func readLimit(procs int) int  { return perCPULimit(procs, readsPerCPU, MaxReadLimit) }
func writeLimit(procs int) int { return perCPULimit(procs, writesPerCPU, MaxWriteLimit) }

func perCPULimit(procs, perCPU, ceiling int) int {
	return min(ceiling, perCPU*max(procs, 1))
}

// DefaultWait is how long a request waits in its class's queue for room before it is refused: 1 s.
// At 1x on INFRA-589's 2-core reproduction every read slot stays busy for over a second in a stall
// and a write can be in flight for over a second (the measurements above), so a shorter wait
// refuses requests at a load the server serves: with 8 reads a 75 ms wait refused 34 writes in
// 180 s at 1x and a 1 s wait 6, and with 28 reads, 12 writes and a queue of twice the limit a 1 s
// wait refused 0-9 reads and 5-11 writes. A refused request is answered once its wait has run out,
// so a refusal's p99 is about the wait.
const DefaultWait = time.Second

// queueFactor is how many requests each class's queue holds per unit of its limit: twice the
// limit, room for two more bursts of the class's full width behind the one running. A 1 s wait
// outlasts a full width's work, so a request two widths back still expects room within it: 16
// reads with a 75 ms wait and a queue of one limit refused 889 reads at 1x, and with a 1 s wait and
// a queue of two limits 112.
const queueFactor = 2

// Limits are a Limiter's concurrency limits per class, in units (a request takes its route's
// weight), and how long a request may wait for room; a zero Wait is DefaultWait.
type Limits struct {
	Read, Write int
	Wait        time.Duration
}

// DefaultLimits are the limits the server runs with: reads and writes by the CPUs the process may
// use (readLimit and writeLimit of runtime.GOMAXPROCS) and DefaultWait.
func DefaultLimits() Limits {
	procs := runtime.GOMAXPROCS(0)
	return Limits{Read: readLimit(procs), Write: writeLimit(procs), Wait: DefaultWait}
}

// Limiter admits requests per class under Limits. Each class runs at most its limit's units at
// once; a request that finds no room waits in that class's queue, first come first served, for at
// most Limits.Wait, and a request that finds the queue already holding queueFactor times the
// limit is refused at once. A Limiter is safe for concurrent use.
type Limiter struct {
	classes [classCount]*gate
}

// New returns a Limiter with limits. A limit below one is one.
func New(limits Limits) *Limiter {
	wait := limits.Wait
	if wait <= 0 {
		wait = DefaultWait
	}
	l := &Limiter{}
	for class, limit := range [classCount]int{Read: limits.Read, Write: limits.Write} {
		limit = max(limit, 1)
		l.classes[class] = &gate{class: Class(class), limit: limit, wait: wait}
	}
	return l
}

// Admit takes weight units of class for the caller, waiting in the class's queue when it has no
// room. It returns a *Refusal when the queue is full or the wait runs out, and ctx's error when
// ctx ends first. A weight below one is one, and one above the class's limit is the limit, so a
// heavy route can still run alone. The caller releases the Ticket once it is done.
func (l *Limiter) Admit(ctx context.Context, class Class, weight int) (*Ticket, error) {
	g := l.classes[class]
	units := min(max(weight, 1), g.limit)
	if err := g.acquire(ctx, units); err != nil {
		return nil, err
	}
	return &Ticket{gate: g, units: units, held: true}, nil
}

// ClassStats is what one class of a Limiter holds and has refused.
type ClassStats struct {
	Class Class
	// Limit is the class's limit in units; InFlight the units admitted requests hold now, and
	// Queued the requests waiting for room.
	Limit, InFlight, Queued int
	// QueueFull and WaitExceeded count the requests refused for each reason since the Limiter
	// was made.
	QueueFull, WaitExceeded uint64
	// LongestRefusedWait is the longest a request refused for waiting (WaitExceeded) was queued
	// before the limiter refused it: from its arrival in the queue to the time its wait's timer
	// reports firing. It is the admission's own share of a refusal's time, without the time the
	// refused request's goroutine then waits to run, which a loaded machine stretches.
	LongestRefusedWait time.Duration
}

// Stats reads each class's counts, reads first. The counts of one class are read together.
func (l *Limiter) Stats() []ClassStats {
	stats := make([]ClassStats, 0, classCount)
	for _, g := range l.classes {
		stats = append(stats, g.stats())
	}
	return stats
}

// Ticket is one admitted request's hold on its class. It rides on the request's context
// (WithTicket), so whatever the request waits on elsewhere can give the units back while it
// waits (Park) and take them back after (Resume). The methods of a nil Ticket do nothing, so a
// caller need not ask whether its request was admitted at all.
type Ticket struct {
	gate  *gate
	units int

	mu       sync.Mutex
	held     bool
	released bool
}

// Park gives the ticket's units back to its class while its holder waits for something else
// - a document's writer gate, where writers queued on one document would otherwise hold every
// write unit from writers to other documents. Resume takes them back. Parking a ticket that is
// parked or released does nothing.
func (t *Ticket) Park() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held {
		t.held = false
		t.gate.release(t.units)
	}
}

// Resume takes a parked ticket's units back, under the same queue and wait bounds Admit applies,
// and returns Admit's refusal or ctx's error when it cannot; the ticket then stays parked, and
// its holder answers the refusal as a refused request. Resuming a ticket that is not parked does
// nothing.
func (t *Ticket) Resume(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	parked := !t.held && !t.released
	t.mu.Unlock()
	if !parked {
		return nil
	}
	if err := t.gate.acquire(ctx, t.units); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		// Released while it waited: the units it took are nobody's.
		t.gate.release(t.units)
		return nil
	}
	t.held = true
	return nil
}

// Release gives the ticket's units back for good. It runs once; every later call, and a call on
// a parked ticket after its units went back, does nothing.
func (t *Ticket) Release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	if t.held {
		t.held = false
		t.gate.release(t.units)
	}
}

type ticketKey struct{}

// WithTicket returns ctx carrying ticket.
func WithTicket(ctx context.Context, ticket *Ticket) context.Context {
	return context.WithValue(ctx, ticketKey{}, ticket)
}

// TicketFrom is the ticket ctx carries, or nil for a request admission did not count.
func TicketFrom(ctx context.Context) *Ticket {
	ticket, _ := ctx.Value(ticketKey{}).(*Ticket)
	return ticket
}

// gate is one class's units and wait queue: a weighted semaphore whose waiters are served in
// arrival order, so a heavy request at the head is not overtaken by light ones behind it.
type gate struct {
	class Class
	limit int
	wait  time.Duration

	mu      sync.Mutex
	used    int
	waiters list.List // of *waiter, in arrival order

	queueFull, waitExceeded atomic.Uint64
	longestRefusedWait      atomic.Int64
}

// waiter is one queued acquire; ready closes once its units are granted.
type waiter struct {
	units int
	ready chan struct{}
}

// acquire takes units, waiting in the queue for at most g.wait.
func (g *gate) acquire(ctx context.Context, units int) error {
	g.mu.Lock()
	if g.waiters.Len() == 0 && g.used+units <= g.limit {
		g.used += units
		g.mu.Unlock()
		return nil
	}
	// The queue holds queueFactor times the class's limit (queueFactor says why).
	if g.waiters.Len() >= queueFactor*g.limit {
		g.mu.Unlock()
		g.queueFull.Add(1)
		return &Refusal{Class: g.class, Reason: QueueFull}
	}
	queued := &waiter{units: units, ready: make(chan struct{})}
	element := g.waiters.PushBack(queued)
	g.mu.Unlock()

	queuedAt := time.Now()
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	var failure error
	var waited time.Duration
	select {
	case <-queued.ready:
		return nil
	case <-ctx.Done():
		failure = ctx.Err()
	case fired := <-timer.C:
		failure = &Refusal{Class: g.class, Reason: WaitExceeded}
		waited = fired.Sub(queuedAt)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-queued.ready:
		// Granted in the moment the wait ended. A caller whose wait ran out takes it, since it
		// is here; one whose context ended gives it straight back, so no unit is left held.
		if ctx.Err() == nil {
			return nil
		}
		g.used -= units
		g.grant()
		return ctx.Err()
	default:
	}
	front := g.waiters.Front() == element
	g.waiters.Remove(element)
	if front {
		// The head may have been all that held back the waiters behind it.
		g.grant()
	}
	if _, refused := failure.(*Refusal); refused {
		g.waitExceeded.Add(1)
		if int64(waited) > g.longestRefusedWait.Load() {
			g.longestRefusedWait.Store(int64(waited))
		}
	}
	return failure
}

// release gives units back and grants the queue whatever now fits.
func (g *gate) release(units int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used -= units
	g.grant()
}

// grant hands room to the waiters at the head of the queue, in order, while each fits. g.mu is
// held.
func (g *gate) grant() {
	for element := g.waiters.Front(); element != nil; element = g.waiters.Front() {
		next := element.Value.(*waiter)
		if g.used+next.units > g.limit {
			return
		}
		g.used += next.units
		g.waiters.Remove(element)
		close(next.ready)
	}
}

func (g *gate) stats() ClassStats {
	g.mu.Lock()
	inFlight, queued := g.used, g.waiters.Len()
	g.mu.Unlock()
	return ClassStats{
		Class: g.class, Limit: g.limit, InFlight: inFlight, Queued: queued,
		QueueFull: g.queueFull.Load(), WaitExceeded: g.waitExceeded.Load(),
		LongestRefusedWait: time.Duration(g.longestRefusedWait.Load()),
	}
}
