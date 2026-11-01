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

	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// The limits. The request pool (store.SharedPoolSize) holds 48 connections, and an admitted
// request holds at most one of them at a time, or Weight of them for a route that reads side by
// side. unadmittedConnections of the 48 are left to the pool's users that admission does not
// count: the websockets' identity checks (whileCallerResolves), the browsers' durable document
// appends, the /auth routes and the GitHub proxy, and the work a handler leaves running after it
// has answered. The other 40 are all admission may hand out at once, whatever the mix of classes:
// MaxAdmittedUnits, the pool's own size less those, so a pool that shrinks takes this cap down
// with it and every unit admission hands out stays one the pool can serve. Under that combined
// cap writes, which wait on each other at the commit order, hold at most 24 (MaxWriteLimit) and
// reads at most 28 (MaxReadLimit): the two add to 52, so a class reaches its own cap only while
// the other is quiet.
//
// Each class's limit follows the CPUs the process may use (runtime.GOMAXPROCS, which follows the
// container's CPU limit) up to its cap: readsPerCPU reads and writesPerCPU writes per CPU, at least
// one CPU's worth. Two CPUs, the production task's, already reach both caps, 28 reads and 24 writes,
// and so does every larger pod: at 16 CPUs the limits are still 28 and 24, because the request
// pool's 48 connections bound what admission may let in, so a larger pod's limit comes from the
// pool, not its cores. One CPU gets 14 reads and 12 writes.
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
// The combined cap is what keeps the two class caps from oversubscribing the request pool: 28
// reads and 24 writes are 52 units against the 40 the pool has for admitted requests. Capped by
// class alone, a 10x step is not refused here at all and queues on the pool instead - 29 waiters
// measured - where each request ends as a 503 DATABASE_BUSY after the 3 s acquire bound rather
// than as the 429 a refusal answers at once. The combined cap puts that refusal back here: a
// class may take its whole cap only while the other is quiet, and every unit admission hands out
// is one the pool can serve.
//
// Why writes 24: edits, issue creates and ask creates weigh 2 (their related-item check reads
// twice, side by side), and at 1x on the same reproduction writes in flight reach 19-24 units. In
// adjacent pairs on one pod (the same seed and offer), 28 reads and 12 writes refused at 1x 4, 32
// and 0 writes and 0, 320 and 0 reads in three runs; 28 reads, 24 writes and a combined 40 refused
// nothing in all three. At 10x the same pair refused only with 429 and dropped no connection:
// 12 writes committed 4.09 events/s and served 14.2% of the requests, 24 writes under the combined
// 40 committed 8.34 and served 8.7%, since writes then hold 24 of the 40 units and reads the rest;
// the request pool's waiters peaked at 1 against the 29 that 24 writes without the combined cap
// left on it.
//
// Of the 40, reads keep ReservedReadUnits (22) while they are waiting for room: a write is
// admitted only while the units it takes, beside the reserved read units the reads have not
// taken yet, are within the 40, so under read pressure writes hold at most 40 - 22 = 18 and the
// reads reach their 22. The room is held back only while a read is queued for it, so with the
// reads light nothing is reserved and writes still reach their whole cap of 24, which 1x needs:
// its write units in flight reach 19-24, and the 28 reads and 24 writes above refused nothing
// of it in three runs.
//
// What it is for: at 10x the writes took 24 of the 40 units and the reads, left about 16, were
// served about 50/s, below the 60/s the 1x step offers them. Holding the writes to 18 leaves
// the reads the 22 units that rate asks for, and the writes half again the 12 the base ran with.
//
// Measured in adjacent pairs on one pod against the base (28 reads, 12 writes, no combined cap):
// at 10x the reservation served 65 reads/s (base 86, 1x offers 60) and committed 6.02 events/s
// (base 4.12), with reads holding at most 22 units and writes 18, refusing only with 429; at 1x it
// refused 0, 1 and 3 writes and 0, 0 and 17 reads in three runs (base 1, 0, 1 and 0, 0, 0), the 17
// in one 1.5-second stall that slowed every issue read at once.
const (
	MaxReadLimit  = 28
	MaxWriteLimit = 24
	// unadmittedConnections is how many of the request pool's connections are left to the users
	// admission does not count, the ones the comment above lists.
	unadmittedConnections = 8
	MaxAdmittedUnits      = store.SharedPoolSize - unadmittedConnections
	ReservedReadUnits     = 22
	readsPerCPU           = 14
	writesPerCPU          = 12
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

// Limits are a Limiter's concurrency limits in units (a request takes its route's weight): one
// per class, one over the two together, how much of that one is kept for reads, and how long a
// request may wait for room; a zero Wait is DefaultWait.
type Limits struct {
	Read, Write int
	// Total is the units both classes may hold together. Zero is no combined cap beyond the
	// classes' own, Read plus Write, so a caller that raises one class - a test that wants
	// sixteen writes at once - gets the room it asked for without naming a total;
	// DefaultLimits sets MaxAdmittedUnits. A Total below a class's limit is allowed: that
	// class then reaches its own limit only while the other class is empty, and never past
	// the total.
	Total int
	Wait  time.Duration
	// ReservedReads is how many of Total's units writes leave to the reads while a read is
	// waiting for room: a write is admitted only while its units, beside the reserved units
	// the reads have not taken yet, are within Total, so the reads reach the reservation
	// under write pressure and the writes hold at most Total minus it. Zero is no
	// reservation; DefaultLimits sets ReservedReadUnits. New clamps it to the read limit and
	// to Total.
	ReservedReads int
}

// DefaultLimits are the limits the server runs with: reads and writes by the CPUs the process may
// use (readLimit and writeLimit of runtime.GOMAXPROCS), MaxAdmittedUnits over the two of them,
// ReservedReadUnits of those kept for the reads, and DefaultWait.
func DefaultLimits() Limits {
	procs := runtime.GOMAXPROCS(0)
	return Limits{
		Read: readLimit(procs), Write: writeLimit(procs), Total: MaxAdmittedUnits,
		ReservedReads: ReservedReadUnits, Wait: DefaultWait,
	}
}

// Limiter admits requests per class under Limits. A read is admitted while its class and both
// classes together are under their limits; a write while its class is and its units fit under
// Limits.Total beside the read units Limits.ReservedReads still keeps for waiting reads. One
// that finds no room waits in its class's queue, in the order the requests arrived (Arrive),
// for at most Limits.Wait over the whole request, and one that finds that queue already holding
// queueFactor times the class's limit is refused at once. A Limiter is safe for concurrent use.
type Limiter struct {
	// mu guards every count and queue below: the combined cap is one number over both
	// classes, so granting reads and moves the two together.
	mu      sync.Mutex
	used    int    // the units held across the classes
	arrived uint64 // arrivals so far: a waiter's place in the order the two queues are served in
	classes [classCount]*gate

	total         int // the combined cap, the read reservation under it, and wait, all set once by New
	reservedReads int
	wait          time.Duration
}

// New returns a Limiter with limits. A limit below one is one, a Total of zero is Read plus
// Write (no combined cap beyond the classes' own), and a ReservedReads is kept within zero and
// the smaller of the read limit and the total, which is all the reads could ever hold.
func New(limits Limits) *Limiter {
	l := &Limiter{wait: limits.Wait}
	if l.wait <= 0 {
		l.wait = DefaultWait
	}
	for class, limit := range [classCount]int{Read: limits.Read, Write: limits.Write} {
		limit = max(limit, 1)
		l.classes[class] = &gate{class: Class(class), limit: limit}
		l.total += limit
	}
	if limits.Total > 0 {
		l.total = limits.Total
	}
	l.reservedReads = min(max(limits.ReservedReads, 0), l.classes[Read].limit, l.total)
	return l
}

// Arrive gives the request arriving now its place in the order requests reach the Limiter, with
// the class and weight it will ask room for: a weight below one is one, and one above the
// class's limit or above the combined cap is that cap, so a heavy route can still run alone.
//
// A request that must do something before it asks for room - a write reading its body off the
// connection, which it does holding no unit (the API's drain) - takes its place here first and
// is then queued where it arrived, ahead of everything that arrived while it read, rather than
// behind it.
func (l *Limiter) Arrive(class Class, weight int) *Arrival {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.arrived++
	return &Arrival{
		limiter: l, class: class, seq: l.arrived,
		units: min(max(weight, 1), l.classes[class].limit, l.total),
	}
}

// Arrival is the place one request holds in a Limiter's arrival order, with the class and units
// it asks for and how long it has waited for them so far. A request asks for room more than
// once when it gives its units back to wait for something else (Ticket.Park) and takes them back
// after (Ticket.Resume): each ask queues it at the place it arrived, and the Limiter's wait is
// its wait over all of them, so the queue costs it one wait however often it asks.
type Arrival struct {
	limiter *Limiter
	class   Class
	units   int
	seq     uint64
	// waited is how long the request has spent in the queue so far. Only the request's own
	// goroutine touches it: a request is in the queue once at a time.
	waited time.Duration
}

// Admit takes the arrival's units, waiting in its class's queue, at the place it arrived, for
// what is left of the Limiter's wait. It returns a *Refusal when the queue is full or the wait
// runs out, and ctx's error when ctx ends first. The caller releases the Ticket once it is done.
func (a *Arrival) Admit(ctx context.Context) (*Ticket, error) {
	if err := a.limiter.acquire(ctx, a); err != nil {
		return nil, err
	}
	return &Ticket{arrival: a, held: true}, nil
}

// AdmitNow takes the arrival's units while the Limiter has room for them and nobody is queued
// for its class, and answers nil where the request would have to wait for them - which is where
// its caller has something to do before it waits, a write its body to read off the connection.
func (a *Arrival) AdmitNow() *Ticket {
	if !a.limiter.acquireNow(a) {
		return nil
	}
	return &Ticket{arrival: a, held: true}
}

// Admit takes weight units of class for a request arriving now, waiting in the class's queue
// when it has no room: Arrive and Arrival.Admit in one, for a caller with nothing to do between
// the two.
func (l *Limiter) Admit(ctx context.Context, class Class, weight int) (*Ticket, error) {
	return l.Arrive(class, weight).Admit(ctx)
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
	// before the limiter refused it: its whole time in the queue, over every time it asked for
	// room, up to the time its wait's timer reports firing. It is the admission's own share of a
	// refusal's time, without the time the refused request's goroutine then waits to run, which
	// a loaded machine stretches.
	LongestRefusedWait time.Duration
}

// Stats reads each class's counts, reads first. Every count of both classes is read together.
func (l *Limiter) Stats() []ClassStats {
	stats := make([]ClassStats, 0, classCount)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, g := range l.classes {
		stats = append(stats, g.stats())
	}
	return stats
}

// Totals is what a Limiter holds across both classes.
type Totals struct {
	// Limit is the combined cap in units (Limits.Total), InFlight the units the admitted
	// requests of both classes hold under it now, and ReservedReads the units of the cap
	// writes leave to the reads while a read waits for room (Limits.ReservedReads).
	Limit, InFlight, ReservedReads int
}

// Totals reads the combined cap, the read room reserved under it and what is held under it, the
// three together.
func (l *Limiter) Totals() Totals {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Totals{Limit: l.total, InFlight: l.used, ReservedReads: l.reservedReads}
}

// Ticket is one admitted request's hold on its class. It rides on the request's context
// (WithTicket), so whatever the request waits on elsewhere can give the units back while it
// waits (Park) and take them back after (Resume). The methods of a nil Ticket do nothing, so a
// caller need not ask whether its request was admitted at all.
type Ticket struct {
	// arrival is the place the request holds in the Limiter's arrival order, which it keeps
	// for as long as it lives: a ticket that parks its units asks for them again there.
	arrival *Arrival

	mu       sync.Mutex
	held     bool
	released bool
}

// Park gives the ticket's units back to its class, and to the combined cap, while its holder
// waits for something else - a document's writer gate, where writers queued on one document
// would otherwise hold every write unit from writers to other documents. Resume takes them back.
// Parking a ticket that is parked or released does nothing.
func (t *Ticket) Park() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held {
		t.held = false
		t.arrival.limiter.release(t.arrival.class, t.arrival.units)
	}
}

// Resume takes a parked ticket's units back, waiting in its class's queue at the place the
// request arrived - ahead of every request that arrived after it - for what is left of the one
// wait the queue costs it, and returns Admit's refusal or ctx's error when it cannot; the ticket
// then stays parked, and its holder answers the refusal as a refused request. Resuming a ticket
// that is not parked does nothing.
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
	if err := t.arrival.limiter.acquire(ctx, t.arrival); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		// Released while it waited: the units it took are nobody's.
		t.arrival.limiter.release(t.arrival.class, t.arrival.units)
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
		t.arrival.limiter.release(t.arrival.class, t.arrival.units)
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
// arrival order, so a heavy request at the head is not overtaken by light ones behind it. Its
// Limiter's mutex guards its counts and its queue, since the combined cap ties the classes
// together; only the refusal counters, which no decision reads, stand on their own.
type gate struct {
	class   Class
	limit   int
	used    int
	waiters list.List // of *waiter, in arrival order

	queueFull, waitExceeded atomic.Uint64
	longestRefusedWait      atomic.Int64
}

// waiter is one queued acquire; ready closes once its units are granted. seq is its place among
// the arrivals of both classes, which orders the two queue heads against each other.
type waiter struct {
	units int
	seq   uint64
	ready chan struct{}
}

// queue puts queued in g's waiters, behind every waiter that arrived before it and ahead of
// every waiter that arrived after. A request joins the queue out of arrival order where it had
// something to do before it asked for room - a write reading its body off the connection
// (Limiter.Arrive) - or where it gave its units back and asked for them again (Ticket.Resume):
// either way the queue serves it where it arrived, so the requests that arrived while it was
// busy do not overtake it. l.mu is held.
func (g *gate) queue(queued *waiter) *list.Element {
	for element := g.waiters.Back(); element != nil; element = element.Prev() {
		if element.Value.(*waiter).seq < queued.seq {
			return g.waiters.InsertAfter(queued, element)
		}
	}
	return g.waiters.PushFront(queued)
}

// acquireNow takes a's units if they fit with nobody queued for its class, and reports whether
// it did: the caller of a request that would have to wait does something else first.
func (l *Limiter) acquireNow(a *Arrival) bool {
	g := l.classes[a.class]
	l.mu.Lock()
	defer l.mu.Unlock()
	if g.waiters.Len() != 0 || !l.fits(g, a.units) {
		return false
	}
	g.used += a.units
	l.used += a.units
	return true
}

// acquire takes a's units, waiting in its class's queue at the place a arrived for what is left
// of l.wait: a request that queues more than once - it parked its units and asked for them
// again (Ticket.Resume) - waits one wait over all of its asks.
func (l *Limiter) acquire(ctx context.Context, a *Arrival) error {
	g := l.classes[a.class]
	l.mu.Lock()
	if g.waiters.Len() == 0 && l.fits(g, a.units) {
		g.used += a.units
		l.used += a.units
		l.mu.Unlock()
		return nil
	}
	// The queue holds queueFactor times the class's limit (queueFactor says why).
	if g.waiters.Len() >= queueFactor*g.limit {
		l.mu.Unlock()
		g.queueFull.Add(1)
		return &Refusal{Class: a.class, Reason: QueueFull}
	}
	queued := &waiter{units: a.units, seq: a.seq, ready: make(chan struct{})}
	element := g.queue(queued)
	if element == g.waiters.Front() {
		// It arrived before the waiters it found queued, so the room the queues were last
		// offered was offered to a head that is no longer the head.
		l.grant()
	}
	l.mu.Unlock()

	queuedAt := time.Now()
	timer := time.NewTimer(max(l.wait-a.waited, 0))
	defer timer.Stop()
	var failure error
	select {
	case <-queued.ready:
		a.waited += time.Since(queuedAt)
		return nil
	case <-ctx.Done():
		a.waited += time.Since(queuedAt)
		failure = ctx.Err()
	case fired := <-timer.C:
		a.waited += fired.Sub(queuedAt)
		failure = &Refusal{Class: a.class, Reason: WaitExceeded}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-queued.ready:
		// Granted in the moment the wait ended. A caller whose wait ran out takes it, since it
		// is here; one whose context ended gives it straight back, so no unit is left held.
		if ctx.Err() == nil {
			return nil
		}
		g.used -= a.units
		l.used -= a.units
		l.grant()
		return ctx.Err()
	default:
	}
	front := g.waiters.Front() == element
	g.waiters.Remove(element)
	if front {
		// The head may have been all that held back the waiters behind it - and, when the
		// combined cap was what held the head, the other class's waiters too.
		l.grant()
	}
	if _, refused := failure.(*Refusal); refused {
		g.waitExceeded.Add(1)
		if int64(a.waited) > g.longestRefusedWait.Load() {
			g.longestRefusedWait.Store(int64(a.waited))
		}
	}
	return failure
}

// release gives units of class back and grants the queues whatever now fits.
func (l *Limiter) release(class Class, units int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.classes[class].used -= units
	l.used -= units
	l.grant()
}

// fits reports whether units may start in g now: g's class and the combined cap, the reserved
// read room counted, both have room, and no queue head is held up by the combined cap alone.
// Such a head arrived before this request and waits for room of the whole Limiter, which this
// request would take from it. l.mu is held.
func (l *Limiter) fits(g *gate, units int) bool {
	return l.hasRoom(g, units) && l.heldByTotal() == nil
}

// hasRoom reports whether g's class and the combined cap hold units on top of what they hold
// now, the room reserved for the reads counted against a write. l.mu is held.
func (l *Limiter) hasRoom(g *gate, units int) bool {
	return g.used+units <= g.limit && l.used+units+l.reservedFor(g.class) <= l.total
}

// reservedFor is the room of the combined cap class may not take: for a write, the read units
// reserved that the reads have not taken yet, while a read is waiting for them. A read takes
// the reserved room itself, and reserving it against nobody would only idle the units, so it is
// held back only while the read queue has someone in it. l.mu is held.
func (l *Limiter) reservedFor(class Class) int {
	reads := l.classes[Read]
	if class == Read || reads.waiters.Len() == 0 {
		return 0
	}
	return max(l.reservedReads-reads.used, 0)
}

// heldByTotal is the earliest waiter at the head of a class's queue that fits its own class but
// not the combined cap itself, or nil when neither head is one. A write the read reservation
// alone holds up is not one: the reads it waits for must be able to take that room, so it holds
// back the writes queued behind it and nothing else. l.mu is held.
func (l *Limiter) heldByTotal() *waiter {
	var earliest *waiter
	for _, g := range l.classes {
		front := g.waiters.Front()
		if front == nil {
			continue
		}
		head := front.Value.(*waiter)
		if g.used+head.units <= g.limit && l.used+head.units > l.total &&
			(earliest == nil || head.seq < earliest.seq) {
			earliest = head
		}
	}
	return earliest
}

// grant hands room to the waiters at the heads of the two class queues, the earlier arrival of
// the two first, while each fits. A head the combined cap itself alone holds up stops the pass
// for both classes: the room it waits for is the whole Limiter's, and the later arrivals of the
// other class would take it a unit at a time, so a weight-2 head is never starved by weight-1
// requests beside it. A head its own class's limit, or the read reservation, holds up stops only
// its own queue. l.mu is held.
func (l *Limiter) grant() {
	for {
		held := l.heldByTotal()
		var next *waiter
		var from *gate
		for _, g := range l.classes {
			front := g.waiters.Front()
			if front == nil {
				continue
			}
			head := front.Value.(*waiter)
			if !l.hasRoom(g, head.units) {
				continue
			}
			if next == nil || head.seq < next.seq {
				next, from = head, g
			}
		}
		if next == nil || (held != nil && held.seq < next.seq) {
			return
		}
		from.used += next.units
		l.used += next.units
		from.waiters.Remove(from.waiters.Front())
		close(next.ready)
	}
}

// stats is the class's counts. Its Limiter's mutex is held.
func (g *gate) stats() ClassStats {
	return ClassStats{
		Class: g.class, Limit: g.limit, InFlight: g.used, Queued: g.waiters.Len(),
		QueueFull: g.queueFull.Load(), WaitExceeded: g.waitExceeded.Load(),
		LongestRefusedWait: time.Duration(g.longestRefusedWait.Load()),
	}
}
