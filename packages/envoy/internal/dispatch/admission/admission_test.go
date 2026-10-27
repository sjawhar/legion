package admission

import (
	"context"
	"errors"
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
	l := newLimiter(2, 1, patient)
	held := admit(t, l, Read, 2)
	order := make(chan int, 3)
	for index := range 3 {
		outcome := admitInBackground(t, l, context.Background(), Read, 2)
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

func TestAFullQueueRefusesAtOnce(t *testing.T) {
	l := newLimiter(1, 1, patient)
	held := admit(t, l, Write, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 2 { // the queue holds twice the limit
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
	if stats.QueueFull != 1 || stats.WaitExceeded != 0 || stats.Queued != 2 {
		t.Fatalf("write stats %+v, want one queue_full refusal and the two still queued", stats)
	}
	if read := classStats(l, Read); read.QueueFull != 0 {
		t.Fatalf("read stats %+v, want the refusal counted against writes only", read)
	}
	cancel()
	waitForQueued(t, l, Write, 0)
	held.Release()
}

func TestAWaitPastItsBoundIsRefused(t *testing.T) {
	const wait = 50 * time.Millisecond
	l := newLimiter(1, 1, wait)
	held := admit(t, l, Read, 1)
	started := time.Now()
	_, err := l.Admit(context.Background(), Read, 1)
	waited := time.Since(started)
	if refusalReason(err) != WaitExceeded {
		t.Fatalf("admit behind a held request: %v, want a wait_exceeded refusal", err)
	}
	if waited < wait {
		t.Fatalf("refused after %v, before its wait of %v", waited, wait)
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
	l := newLimiter(1, 1, patient)
	held := admit(t, l, Write, 1)
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
