package admission

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// patient is a wait no test reaches: a test that wants a waiter to stay queued gives it this, so
// only the test's own release or cancellation moves it.
const patient = time.Minute

func newLimiter(read, write int, wait time.Duration) *Limiter {
	return New(Limits{Read: read, Write: write, Wait: wait})
}

func admit(t *testing.T, l *Limiter, class Class, weight int) *Ticket {
	t.Helper()
	ticket, err := l.Admit(context.Background(), class, weight)
	if err != nil {
		t.Fatalf("admit a %s of weight %d: %v", class, weight, err)
	}
	return ticket
}

func classStats(l *Limiter, class Class) ClassStats { return l.Stats()[class] }

// waitForQueued returns once class has queued requests waiting.
func waitForQueued(t *testing.T, l *Limiter, class Class, queued int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for classStats(l, class).Queued != queued {
		if time.Now().After(deadline) {
			t.Fatalf("%s queue holds %d, want %d", class, classStats(l, class).Queued, queued)
		}
		time.Sleep(time.Millisecond)
	}
}

// admitInBackground starts an Admit and answers the channel its outcome arrives on, once the
// request is queued.
func admitInBackground(t *testing.T, l *Limiter, ctx context.Context, class Class, weight int) <-chan admitted {
	t.Helper()
	queuedBefore := classStats(l, class).Queued
	outcome := make(chan admitted, 1)
	go func() {
		ticket, err := l.Admit(ctx, class, weight)
		outcome <- admitted{ticket, err}
	}()
	waitForQueued(t, l, class, queuedBefore+1)
	return outcome
}

type admitted struct {
	ticket *Ticket
	err    error
}

func receive(t *testing.T, outcome <-chan admitted) admitted {
	t.Helper()
	select {
	case result := <-outcome:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("a queued request was never answered")
		return admitted{}
	}
}

func assertPending(t *testing.T, outcome <-chan admitted) {
	t.Helper()
	select {
	case result := <-outcome:
		t.Fatalf("a request that should still wait was answered: %+v", result)
	default:
	}
}

func refusalReason(err error) Reason {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

// Each queued request takes the whole limit, so the next is admitted only once the one before it
// is released: the order they are admitted in is the order the queue serves them.
func TestQueuedRequestsAreAdmittedInArrivalOrder(t *testing.T) {
	l := newLimiter(3, 1, patient) // the queue holds six requests
	held := admit(t, l, Read, 3)
	order := make(chan int, 3)
	for index := range 3 {
		outcome := admitInBackground(t, l, context.Background(), Read, 3)
		go func() {
			result := <-outcome
			if result.err != nil {
				t.Errorf("queued request %d: %v", index, result.err)
				return
			}
			order <- index
			result.ticket.Release()
		}()
	}
	held.Release()
	for want := range 3 {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("request %d admitted in place %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d was never admitted", want)
		}
	}
}

// A heavy request at the head of the queue is not overtaken by a light one behind it that would
// fit: first come, first served.
func TestAHeavyRequestAtTheHeadIsNotOvertaken(t *testing.T) {
	l := newLimiter(2, 1, patient)
	first := admit(t, l, Read, 1)
	heavy := admitInBackground(t, l, context.Background(), Read, 2)
	light := admitInBackground(t, l, context.Background(), Read, 1)
	assertPending(t, light)
	first.Release()
	heavyTicket := receive(t, heavy)
	if heavyTicket.err != nil {
		t.Fatalf("heavy request: %v", heavyTicket.err)
	}
	if stats := classStats(l, Read); stats.InFlight != 2 {
		t.Fatalf("in flight %d with the heavy request admitted, want its 2 units", stats.InFlight)
	}
	assertPending(t, light)
	heavyTicket.ticket.Release()
	if result := receive(t, light); result.err != nil {
		t.Fatalf("light request: %v", result.err)
	}
}

func TestAWeightAboveTheLimitRunsAlone(t *testing.T) {
	l := newLimiter(1, 1, patient)
	ticket := admit(t, l, Write, 2)
	if stats := classStats(l, Write); stats.InFlight != 1 {
		t.Fatalf("in flight %d, want the whole limit of 1", stats.InFlight)
	}
	ticket.Release()
	if stats := classStats(l, Write); stats.InFlight != 0 {
		t.Fatalf("in flight %d after release, want 0", stats.InFlight)
	}
}

// Reads and writes are limited apart: a full write class leaves reads their room.
func TestEachClassHasItsOwnLimit(t *testing.T) {
	l := newLimiter(1, 1, 10*time.Millisecond)
	write := admit(t, l, Write, 1)
	defer write.Release()
	admit(t, l, Read, 1).Release()
	if _, err := l.Admit(context.Background(), Write, 1); refusalReason(err) != WaitExceeded {
		t.Fatalf("a second write: %v, want a wait_exceeded refusal", err)
	}
}

// A class's queue holds twice as many requests as its limit; the next request is refused at once.
func TestAFullQueueRefusesAtOnce(t *testing.T) {
	l := newLimiter(1, 2, patient)
	held := []*Ticket{admit(t, l, Write, 1), admit(t, l, Write, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 4 { // the queue holds twice the limit's two requests
		admitInBackground(t, l, ctx, Write, 1)
	}
	started := time.Now()
	_, err := l.Admit(context.Background(), Write, 1)
	if refusalReason(err) != QueueFull {
		t.Fatalf("admit past a full queue: %v, want a queue_full refusal", err)
	}
	if waited := time.Since(started); waited > 100*time.Millisecond {
		t.Fatalf("a full queue refused after %v, want at once", waited)
	}
	stats := classStats(l, Write)
	if stats.QueueFull != 1 || stats.WaitExceeded != 0 || stats.Queued != 4 {
		t.Fatalf("write stats %+v, want one queue_full refusal and the four still queued", stats)
	}
	if read := classStats(l, Read); read.QueueFull != 0 {
		t.Fatalf("read stats %+v, want the refusal counted against writes only", read)
	}
	cancel()
	waitForQueued(t, l, Write, 0)
	for _, ticket := range held {
		ticket.Release()
	}
}

// A request waits DefaultWait, 1 s, for room and is then refused, so its refusal is answered once
// its wait has run out and not much later.
func TestAWaitPastItsBoundIsRefused(t *testing.T) {
	l := newLimiter(1, 1, 0) // a zero wait is DefaultWait
	held := admit(t, l, Read, 1)
	started := time.Now()
	_, err := l.Admit(context.Background(), Read, 1)
	waited := time.Since(started)
	if refusalReason(err) != WaitExceeded {
		t.Fatalf("admit behind a held request: %v, want a wait_exceeded refusal", err)
	}
	if waited < DefaultWait || waited >= DefaultWait+250*time.Millisecond {
		t.Fatalf("refused after %v, want after its wait of %v and within 250ms of it", waited, DefaultWait)
	}
	stats := classStats(l, Read)
	if stats.WaitExceeded != 1 || stats.Queued != 0 || stats.InFlight != 1 {
		t.Fatalf("read stats %+v, want one wait_exceeded refusal, nothing queued, the held request in flight", stats)
	}
	held.Release()
	admit(t, l, Read, 1).Release()
}

// A request whose client goes away while it waits leaves the queue and takes no unit with it:
// the units it would have held go to the request behind it.
func TestACancelledWaitLeaksNoUnits(t *testing.T) {
	l := newLimiter(1, 2, patient) // the queue holds four requests
	held := admit(t, l, Write, 2)
	ctx, cancel := context.WithCancel(context.Background())
	gone := admitInBackground(t, l, ctx, Write, 1)
	behind := admitInBackground(t, l, context.Background(), Write, 1)
	cancel()
	if result := receive(t, gone); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled wait: %+v, want context.Canceled", result)
	}
	waitForQueued(t, l, Write, 1)
	held.Release()
	result := receive(t, behind)
	if result.err != nil {
		t.Fatalf("request behind the cancelled one: %v", result.err)
	}
	result.ticket.Release()
	if stats := classStats(l, Write); stats.InFlight != 0 || stats.Queued != 0 || stats.QueueFull+stats.WaitExceeded != 0 {
		t.Fatalf("write stats %+v, want nothing held, queued or refused", stats)
	}
}

func TestReleaseRunsOnce(t *testing.T) {
	l := newLimiter(2, 1, patient)
	ticket := admit(t, l, Read, 1)
	other := admit(t, l, Read, 1)
	ticket.Release()
	ticket.Release()
	if stats := classStats(l, Read); stats.InFlight != 1 {
		t.Fatalf("in flight %d after releasing one ticket twice, want the other's 1", stats.InFlight)
	}
	other.Release()
	ticket.Park()
	if err := ticket.Resume(context.Background()); err != nil {
		t.Fatalf("resume a released ticket: %v", err)
	}
	if stats := classStats(l, Read); stats.InFlight != 0 {
		t.Fatalf("in flight %d after parking and resuming a released ticket, want 0", stats.InFlight)
	}
	var none *Ticket
	none.Park()
	none.Release()
	if err := none.Resume(context.Background()); err != nil {
		t.Fatalf("resume a nil ticket: %v", err)
	}
}

// A parked ticket gives its units to the next request while its holder waits elsewhere, and
// takes them back through the queue, under the same bounds as Admit.
func TestAParkedTicketGivesItsUnitsBackUntilItResumes(t *testing.T) {
	const wait = 50 * time.Millisecond
	l := newLimiter(1, 1, wait)
	parked := admit(t, l, Write, 1)
	parked.Park()
	parked.Park()
	other := admit(t, l, Write, 1)

	if err := parked.Resume(context.Background()); refusalReason(err) != WaitExceeded {
		t.Fatalf("resume while the other request holds the unit: %v, want a wait_exceeded refusal", err)
	}
	parked.Release() // a refused resume leaves the ticket parked: its release gives nothing back
	if stats := classStats(l, Write); stats.InFlight != 1 {
		t.Fatalf("in flight %d, want the other request's 1", stats.InFlight)
	}
	other.Release()

	again := newLimiter(1, 1, patient)
	ticket := admit(t, again, Write, 1)
	ticket.Park()
	holder := admit(t, again, Write, 1)
	done := make(chan error, 1)
	go func() { done <- ticket.Resume(context.Background()) }()
	waitForQueued(t, again, Write, 1)
	holder.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume once the unit came free: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked ticket never resumed")
	}
	if stats := classStats(again, Write); stats.InFlight != 1 {
		t.Fatalf("in flight %d after resume, want 1", stats.InFlight)
	}
	ticket.Release()
	if stats := classStats(again, Write); stats.InFlight != 0 {
		t.Fatalf("in flight %d after the resumed ticket's release, want 0", stats.InFlight)
	}
}

// A request that took its place in the arrival order and only then asked for room - a write
// reading its body off the connection first - is queued where it arrived: the requests that
// arrived while it was busy, and are queued already, do not overtake it.
func TestAnEarlierArrivalIsQueuedAheadOfLaterOnesAlreadyWaiting(t *testing.T) {
	l := newLimiter(1, 1, patient)
	held := admit(t, l, Write, 1)
	early := l.Arrive(Write, 1)
	if ticket := early.AdmitNow(); ticket != nil {
		t.Fatal("a write was admitted at once while the only unit was held")
	}
	later := admitInBackground(t, l, context.Background(), Write, 1)

	earliest := make(chan admitted, 1)
	go func() {
		ticket, err := early.Admit(context.Background())
		earliest <- admitted{ticket, err}
	}()
	waitForQueued(t, l, Write, 2)
	held.Release()

	first := receive(t, earliest)
	if first.err != nil {
		t.Fatalf("the write that arrived first: %v, want it admitted first", first.err)
	}
	assertPending(t, later)
	first.ticket.Release()
	if result := receive(t, later); result.err != nil {
		t.Fatalf("the write that arrived second: %v, want it admitted after the first", result.err)
	}
}

// A parked ticket takes its units back at the place its request arrived, ahead of the writes
// that arrived after it and are queued already: a write that waits for a document's writer gate
// gave its units back, and the writes that took them meanwhile are not ahead of it in the queue.
func TestAResumedTicketIsGrantedAheadOfLaterArrivals(t *testing.T) {
	l := newLimiter(1, 1, patient)
	parked := admit(t, l, Write, 1)
	parked.Park()
	holder := admit(t, l, Write, 1)
	later := admitInBackground(t, l, context.Background(), Write, 1)

	resumed := make(chan error, 1)
	go func() { resumed <- parked.Resume(context.Background()) }()
	waitForQueued(t, l, Write, 2)
	holder.Release()

	select {
	case err := <-resumed:
		if err != nil {
			t.Fatalf("resume once the unit came free: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked ticket never resumed, though it arrived before the queued write")
	}
	assertPending(t, later)
	parked.Release()
	if result := receive(t, later); result.err != nil {
		t.Fatalf("the write that arrived after the parked one: %v, want it admitted after it", result.err)
	}
}

// The queue costs a request one wait however often it asks for room: a write queued for most of
// its wait, admitted, parked at a document's writer gate and refused on resuming is refused
// within what is left of that one wait, not given a whole second one. Twice the wait is what had
// a write refused long after it was admitted.
func TestAQueuedThenParkedRequestWaitsOnceOverAll(t *testing.T) {
	const wait = time.Second
	const queued = 600 * time.Millisecond
	l := newLimiter(1, 1, wait)
	held := admit(t, l, Write, 1)
	outcome := admitInBackground(t, l, context.Background(), Write, 1)
	time.Sleep(queued)
	held.Release()
	admittedAfterWaiting := receive(t, outcome)
	if admittedAfterWaiting.err != nil {
		t.Fatalf("the write queued for %v: %v, want it admitted", queued, admittedAfterWaiting.err)
	}

	admittedAfterWaiting.ticket.Park()
	other := admit(t, l, Write, 1)
	defer other.Release()
	resuming := time.Now()
	err := admittedAfterWaiting.ticket.Resume(context.Background())
	waited := time.Since(resuming)
	if refusalReason(err) != WaitExceeded {
		t.Fatalf("resume with the unit held: %v, want a wait_exceeded refusal", err)
	}
	if rest := wait - queued; waited > rest+250*time.Millisecond {
		t.Fatalf("the parked write waited %v more after %v in the queue, want no more than the %v left of its %v wait",
			waited, queued, rest, wait)
	}
	if longest := classStats(l, Write).LongestRefusedWait; longest < wait-250*time.Millisecond || longest > wait+250*time.Millisecond {
		t.Fatalf("the refused write is recorded as having waited %v, want its whole %v wait over both asks", longest, wait)
	}
}

func TestTheTicketRidesOnTheContext(t *testing.T) {
	l := newLimiter(1, 1, patient)
	ticket := admit(t, l, Read, 1)
	defer ticket.Release()
	if got := TicketFrom(WithTicket(context.Background(), ticket)); got != ticket {
		t.Fatalf("TicketFrom = %p, want %p", got, ticket)
	}
	if got := TicketFrom(context.Background()); got != nil {
		t.Fatalf("TicketFrom a context without one = %p, want nil", got)
	}
}

func TestClassOfMethods(t *testing.T) {
	for method, want := range map[string]Class{"GET": Read, "HEAD": Read, "POST": Write, "PUT": Write, "PATCH": Write, "DELETE": Write} {
		if got := ClassOf(method); got != want {
			t.Errorf("ClassOf(%s) = %s, want %s", method, got, want)
		}
	}
}

// The server reads 14 at once and writes 12 at once per CPU it may use, at least one CPU's worth
// and at most MaxReadLimit (28) and MaxWriteLimit (24), so two CPUs already reach both caps and 16
// get no more; the two together hold at most MaxAdmittedUnits (40), and each waits DefaultWait, 1 s.
func TestTheDefaultLimitsFollowTheCPUs(t *testing.T) {
	for procs, want := range map[int][2]int{0: {14, 12}, 1: {14, 12}, 2: {28, 24}, 3: {28, 24}, 16: {28, 24}, 64: {28, 24}} {
		if got := [2]int{readLimit(procs), writeLimit(procs)}; got != want {
			t.Errorf("readLimit, writeLimit(%d) = %v, want %v", procs, got, want)
		}
	}
	limits := DefaultLimits()
	procs := runtime.GOMAXPROCS(0)
	if limits.Read != readLimit(procs) || limits.Write != writeLimit(procs) {
		t.Errorf("DefaultLimits() = %+v, want reads %d and writes %d for GOMAXPROCS %d", limits, readLimit(procs), writeLimit(procs), procs)
	}
	if limits.Total != MaxAdmittedUnits {
		t.Errorf("DefaultLimits() = %+v, want the two classes capped at %d units together", limits, MaxAdmittedUnits)
	}
	if limits.Wait != time.Second {
		t.Errorf("DefaultLimits() = %+v, want a 1 s wait", limits)
	}
}

// The class caps add up to more than the combined cap, so a class reaches its own cap only while
// the other is quiet: with every read unit held a write class of 24 has 12 units left, and with
// no read held it has all 24.
func TestTheCombinedCapBoundsTheTwoClassesTogether(t *testing.T) {
	l := New(Limits{Read: MaxReadLimit, Write: MaxWriteLimit, Total: MaxAdmittedUnits, Wait: 20 * time.Millisecond})
	reads := admit(t, l, Read, MaxReadLimit)
	writes := make([]*Ticket, 0, MaxWriteLimit)
	for range MaxAdmittedUnits - MaxReadLimit {
		writes = append(writes, admit(t, l, Write, 1))
	}
	if _, err := l.Admit(context.Background(), Write, 1); refusalReason(err) != WaitExceeded {
		t.Fatalf("write %d of a class of %d, with the reads holding %d of %d units: %v, want a wait_exceeded refusal",
			MaxAdmittedUnits-MaxReadLimit+1, MaxWriteLimit, MaxReadLimit, MaxAdmittedUnits, err)
	}
	if stats := classStats(l, Write); stats.InFlight != MaxAdmittedUnits-MaxReadLimit || stats.Limit != MaxWriteLimit {
		t.Fatalf("write stats %+v, want %d of its %d units in flight", stats, MaxAdmittedUnits-MaxReadLimit, MaxWriteLimit)
	}
	if got := (Totals{Limit: MaxAdmittedUnits, InFlight: MaxAdmittedUnits}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}

	reads.Release()
	for _, ticket := range writes {
		ticket.Release()
	}
	writes = writes[:0]
	for range MaxWriteLimit {
		writes = append(writes, admit(t, l, Write, 1))
	}
	if _, err := l.Admit(context.Background(), Write, 1); refusalReason(err) != WaitExceeded {
		t.Fatalf("write %d with no read held: %v, want a wait_exceeded refusal past the class's %d", MaxWriteLimit+1, err, MaxWriteLimit)
	}
	if got := (Totals{Limit: MaxAdmittedUnits, InFlight: MaxWriteLimit}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}
	for _, ticket := range writes {
		ticket.Release()
	}
}

// A request its own class has room for, which only the combined cap holds up, is granted by the
// other class's release.
func TestAWaitOnTheCombinedCapEndsWhenTheOtherClassReleases(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 5, Wait: patient})
	read := admit(t, l, Read, 3)
	write := admit(t, l, Write, 2)
	waiting := admitInBackground(t, l, context.Background(), Write, 2)
	assertPending(t, waiting)
	read.Release()
	result := receive(t, waiting)
	if result.err != nil {
		t.Fatalf("the write waiting on the combined cap: %v", result.err)
	}
	if got := (Totals{Limit: 5, InFlight: 4}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}
	write.Release()
	result.ticket.Release()
}

// A head the combined cap holds up is not overtaken by a later, lighter request of the other
// class, which would fit both its own class and the cap: the room belongs to the head.
func TestAHeadHeldByTheCombinedCapIsNotOvertakenByTheOtherClass(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 4, Wait: patient})
	read := admit(t, l, Read, 3)
	heavy := admitInBackground(t, l, context.Background(), Write, 2)
	light := admitInBackground(t, l, context.Background(), Read, 1)
	assertPending(t, heavy)
	assertPending(t, light)
	if got := (Totals{Limit: 4, InFlight: 3}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v: the last unit is the queued write's", l.Totals(), got)
	}
	read.Release()
	granted := receive(t, heavy)
	if granted.err != nil {
		t.Fatalf("the write at the head: %v", granted.err)
	}
	behind := receive(t, light)
	if behind.err != nil {
		t.Fatalf("the read behind it: %v", behind.err)
	}
	behind.ticket.Release()
	granted.ticket.Release()
	if got := (Totals{Limit: 4}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}
}

// A parked ticket gives its units back to the combined cap as well as to its class, so a request
// the cap alone held up runs while its holder waits elsewhere.
func TestAParkedTicketFreesCombinedRoom(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 4, Wait: 20 * time.Millisecond})
	read := admit(t, l, Read, 2)
	parked := admit(t, l, Write, 2)
	if _, err := l.Admit(context.Background(), Write, 2); refusalReason(err) != WaitExceeded {
		t.Fatalf("a write its class has room for, past the combined cap: %v, want a wait_exceeded refusal", err)
	}
	parked.Park()
	if got := (Totals{Limit: 4, InFlight: 2}); l.Totals() != got {
		t.Fatalf("totals %+v after parking, want %+v", l.Totals(), got)
	}
	other := admit(t, l, Write, 2)
	other.Release()
	if err := parked.Resume(context.Background()); err != nil {
		t.Fatalf("resume once the room came back: %v", err)
	}
	if got := (Totals{Limit: 4, InFlight: 4}); l.Totals() != got {
		t.Fatalf("totals %+v after resuming, want %+v", l.Totals(), got)
	}
	read.Release()
	parked.Release()
	if got := (Totals{Limit: 4}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// With the reads idle the writes take their whole class cap; with the reads asking for all 28 of
// their units the reservation hands the room back to them, one released write at a time, until
// the reads hold ReservedReadUnits and the writes the rest of the combined cap - and only then
// is the write that has been waiting all along granted.
func TestTheReservationKeepsReadRoomFromWritesWhileReadsWait(t *testing.T) {
	l := New(Limits{
		Read: MaxReadLimit, Write: MaxWriteLimit, Total: MaxAdmittedUnits,
		ReservedReads: ReservedReadUnits, Wait: patient,
	})
	writes := make([]*Ticket, 0, MaxWriteLimit)
	for range MaxWriteLimit {
		writes = append(writes, admit(t, l, Write, 1))
	}
	if stats := classStats(l, Write); stats.InFlight != MaxWriteLimit {
		t.Fatalf("write stats %+v with no read held, want its whole cap of %d units in flight", stats, MaxWriteLimit)
	}
	reads := make([]*Ticket, 0, MaxReadLimit)
	for range MaxAdmittedUnits - MaxWriteLimit {
		reads = append(reads, admit(t, l, Read, 1))
	}
	// A write asks for the next unit ahead of every read that queues after it, and the reads
	// ask for the 28 their class holds.
	waitingWrite := admitInBackground(t, l, context.Background(), Write, 1)
	readWaiters := make([]<-chan admitted, 0, MaxReadLimit-(MaxAdmittedUnits-MaxWriteLimit))
	for range MaxReadLimit - (MaxAdmittedUnits - MaxWriteLimit) {
		readWaiters = append(readWaiters, admitInBackground(t, l, context.Background(), Read, 1))
	}
	const toTheReads = MaxWriteLimit - (MaxAdmittedUnits - ReservedReadUnits)
	for index := range toTheReads {
		writes[index].Release()
		granted := receive(t, readWaiters[index])
		if granted.err != nil {
			t.Fatalf("the read waiting for the reserved room: %v", granted.err)
		}
		reads = append(reads, granted.ticket)
		assertPending(t, waitingWrite)
	}
	if stats := classStats(l, Write); stats.InFlight != MaxAdmittedUnits-ReservedReadUnits {
		t.Fatalf("write stats %+v with the reads waiting, want the %d units the reservation leaves them",
			stats, MaxAdmittedUnits-ReservedReadUnits)
	}
	if stats := classStats(l, Read); stats.InFlight != ReservedReadUnits {
		t.Fatalf("read stats %+v, want the reserved %d units in flight", stats, ReservedReadUnits)
	}
	if got := (Totals{Limit: MaxAdmittedUnits, InFlight: MaxAdmittedUnits, ReservedReads: ReservedReadUnits}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}

	// The reads hold their reservation now, so the next unit a write gives up is the waiting
	// write's and not theirs.
	writes[toTheReads].Release()
	granted := receive(t, waitingWrite)
	if granted.err != nil {
		t.Fatalf("the write waiting under the reservation: %v", granted.err)
	}
	if stats := classStats(l, Write); stats.InFlight != MaxAdmittedUnits-ReservedReadUnits {
		t.Fatalf("write stats %+v once the reads hold the reservation, want %d units in flight",
			stats, MaxAdmittedUnits-ReservedReadUnits)
	}

	granted.ticket.Release()
	for _, ticket := range writes[toTheReads+1:] {
		ticket.Release()
	}
	for _, outcome := range readWaiters[toTheReads:] {
		result := receive(t, outcome)
		if result.err != nil {
			t.Fatalf("a read still queued once the writes were done: %v", result.err)
		}
		reads = append(reads, result.ticket)
	}
	for _, ticket := range reads {
		ticket.Release()
	}
	if got := (Totals{Limit: MaxAdmittedUnits, ReservedReads: ReservedReadUnits}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// A write the reservation alone holds up is not in the way of the reads the room is kept for:
// the read queued behind it takes the unit the write may not, though the write arrived first.
func TestAWriteHeldByTheReservationDoesNotHoldBackTheReads(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 4, ReservedReads: 2, Wait: patient})
	writes := []*Ticket{admit(t, l, Write, 1), admit(t, l, Write, 1), admit(t, l, Write, 1)}
	read := admit(t, l, Read, 1)
	waitingWrite := admitInBackground(t, l, context.Background(), Write, 1)
	waitingRead := admitInBackground(t, l, context.Background(), Read, 1)

	writes[0].Release()
	granted := receive(t, waitingRead)
	if granted.err != nil {
		t.Fatalf("the read queued behind the held write: %v", granted.err)
	}
	assertPending(t, waitingWrite)
	if got := (Totals{Limit: 4, InFlight: 4, ReservedReads: 2}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v: the freed unit is the reads' reserved one", l.Totals(), got)
	}

	// Nothing of the reads waits now, so the write takes the next unit itself.
	writes[1].Release()
	late := receive(t, waitingWrite)
	if late.err != nil {
		t.Fatalf("the write once no read waited for the reserved room: %v", late.err)
	}
	read.Release()
	granted.ticket.Release()
	late.ticket.Release()
	writes[2].Release()
	if got := (Totals{Limit: 4, ReservedReads: 2}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// A write the reservation holds up is granted once the reads drop below the reservation and ask
// for nothing more: the room is reserved against the reads that want it, not idled.
func TestAWriteHeldByTheReservationRunsWhenTheReadsDropBelowIt(t *testing.T) {
	l := New(Limits{Read: 8, Write: 8, Total: 8, ReservedReads: 5, Wait: patient})
	writes := make([]*Ticket, 0, 4)
	for range 4 {
		writes = append(writes, admit(t, l, Write, 1))
	}
	reads := make([]*Ticket, 0, 4)
	for range 4 {
		reads = append(reads, admit(t, l, Read, 1))
	}
	waitingWrite := admitInBackground(t, l, context.Background(), Write, 1)
	waitingRead := admitInBackground(t, l, context.Background(), Read, 1)

	// The first read's unit goes back to the reads, which hold less than the reserved 5.
	reads[0].Release()
	granted := receive(t, waitingRead)
	if granted.err != nil {
		t.Fatalf("the read waiting for the reserved room: %v", granted.err)
	}
	assertPending(t, waitingWrite)

	// With no read waiting the reservation keeps nothing back, so the write takes the room the
	// reads left, past the 3 units the reservation would leave it.
	reads[1].Release()
	late := receive(t, waitingWrite)
	if late.err != nil {
		t.Fatalf("the write once the reads dropped below the reservation: %v", late.err)
	}
	if stats := classStats(l, Write); stats.InFlight != 5 {
		t.Fatalf("write stats %+v with the reads light, want 5 units in flight", stats)
	}
	if stats := classStats(l, Read); stats.InFlight != 3 {
		t.Fatalf("read stats %+v, want the 3 units the reads kept", stats)
	}
	if got := (Totals{Limit: 8, InFlight: 8, ReservedReads: 5}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}

	for _, ticket := range append(writes, reads[2], reads[3], granted.ticket, late.ticket) {
		ticket.Release()
	}
	if got := (Totals{Limit: 8, ReservedReads: 5}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// The server keeps ReservedReadUnits of the combined cap for the reads, and New takes a
// reservation of zero as none and holds one past the read limit or the cap down to what the
// reads could hold at all.
func TestTheDefaultLimitsReserveReadRoom(t *testing.T) {
	if limits := DefaultLimits(); limits.ReservedReads != ReservedReadUnits {
		t.Errorf("DefaultLimits() = %+v, want %d of its units reserved for reads", limits, ReservedReadUnits)
	}
	if got := New(DefaultLimits()).Totals(); got.ReservedReads != ReservedReadUnits {
		t.Errorf("totals %+v of a default limiter, want %d of its units reserved for reads", got, ReservedReadUnits)
	}
	for _, limits := range []struct {
		name  string
		given Limits
		want  int
	}{
		{"none unless it is asked for", Limits{Read: 8, Write: 4, Total: 10}, 0},
		{"never below none", Limits{Read: 8, Write: 4, Total: 10, ReservedReads: -3}, 0},
		{"never past the read limit", Limits{Read: 6, Write: 4, Total: 10, ReservedReads: 8}, 6},
		{"never past the combined cap", Limits{Read: 8, Write: 4, Total: 5, ReservedReads: 7}, 5},
	} {
		t.Run(limits.name, func(t *testing.T) {
			if got := New(limits.given).Totals().ReservedReads; got != limits.want {
				t.Errorf("New(%+v) reserved %d read units, want %d", limits.given, got, limits.want)
			}
		})
	}
}
