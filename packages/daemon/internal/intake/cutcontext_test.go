package intake

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The one row of this file is the other half of ackprogress_test.go's
// TestTheProgressSignalsHandlerIsCutAtItsLimit, and the two are worth reading together. That row
// holds its handler on the context consumeMessage hands it (heldOnce.hold, which waits on
// ctx.Done()) and proves that such a handling is cut at the limit: for a step that honours its
// context, the deadline is the end of the handling. This row holds its handler on a sleep that
// never reads that context, the shape slowFacts has (backlog_test.go:17-22), and proves that such
// a handling is cut only when the sleep returns of its own accord, measurably later than the
// deadline.
//
// That is Acceptance A2 of dispatch://INFRA-1574, the claim behind the two sentences the issue
// puts in consume.go: what the intake cut guarantees, and what it relies on. consumeMessage runs
// the handling under context.WithTimeout(ctx, spec.HandlerLimit) and then waits for it —
// `outcome := handleMessage(work, …)`. The deadline cancels a context; it ends no goroutine. So
// what the limit guarantees is that a handling which watches its context is cut at the limit, and
// what the limit relies on is exactly that watch: every step of handleMessage taking the context
// it is given and giving up on it. A step that ignores it keeps the message in the daemon's hands
// for as long as it runs, and the `an intake handler outran its limit and was cut` warning and
// the nak that returns the message to the stream both wait for it.
//
// The row is worth having because nothing else in the tree says so, and the tree reads the other
// way round without it. A reader of consumeMessage can take "a handler that hangs is cut at that
// limit" for a promise the daemon keeps whatever the handler does, and the neighbouring row,
// whose handler honours its context, agrees with that reading. This row is the one that does not,
// so a change which made the sentences promise more than the code gives — a wedged step ended,
// rather than a cancelled one cut — fails here.

// cutHandlerLimit is the limit this row gives one fact's handling, and ignoredSleep is how long
// its step then sleeps without ever reading the context that limit cancels. The limit is a few
// hundred milliseconds, as the neighbouring row's is (ackprogress_test.go:601), so the whole row
// costs a couple of seconds; the sleep is five times it, so the gap this row measures between the
// deadline and the handler's return is seconds rather than the milliseconds a loaded machine's
// scheduling can add. The limit is twice the acknowledgement window consumerSpec sets, which
// makes the redelivery after the cut too late to be read as the window's own.
const (
	cutHandlerLimit = 400 * time.Millisecond
	ignoredSleep    = 5 * cutHandlerLimit
)

// cutHandlerLine is the warning consumeMessage writes for a handling its limit cut. The nak that
// sends the cut fact back to the stream is named by the package's own nakFailure
// (ackprogress_test.go:460), the line nakResolve writes just before it. The row asks for both
// twice: that neither had been written while the step was still running, and that both were once
// it returned.
const cutHandlerLine = "an intake handler outran its limit and was cut"

// ignoredContextStep is this row's handler: a fact whose work is a sleep on the clock, the shape
// slowFacts has (backlog_test.go:17-22), which never reads the context it is handed. It is this
// file's own rather than slowFacts because the row's whole question is one of when, so the step
// has to report the deadline it was given, when its sleep returned, and what the daemon had
// written about the message by that moment.
//
// Only the first delivery sleeps. The cut naks that message, JetStream delivers it again, and
// that second delivery is how the row sees the message go back to the stream; a second sleep
// would only hold it there for another limit.
type ignoredContextStep struct {
	sleep time.Duration
	logs  *lockedBuffer

	mu           sync.Mutex
	entered      []time.Time
	deadline     time.Time
	hadDeadline  bool
	returned     time.Time
	logsAtReturn string
}

// Apply is the step the limit cannot end. It reads the context's deadline and then ignores the
// context entirely: the sleep runs to its own length whether the deadline passed during it or
// not, which is what makes this handler the one the limit's guarantee does not hold for.
func (s *ignoredContextStep) Apply(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
	s.mu.Lock()
	s.entered = append(s.entered, time.Now())
	first := len(s.entered) == 1
	s.mu.Unlock()
	if !first {
		return Result{}, nil
	}
	// The deadline is read to report it, never waited on. Reading it is what lets the row make
	// its measurements against the handling's own deadline, the instant consumeMessage put on the
	// context, instead of against a time the test body worked out from its own clock.
	deadline, hadDeadline := ctx.Deadline()
	s.sleepIgnoringTheCancelledContext()
	// What the daemon had written is read here, from inside the handler, rather than in the test
	// body: the row's claim is about this moment, the one where the deadline is long past and the
	// step is only now returning, and no read the test body makes afterwards can stand for it.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline, s.hadDeadline = deadline, hadDeadline
	s.logsAtReturn = s.logs.String()
	s.returned = time.Now()
	return Result{}, nil
}

// sleepIgnoringTheCancelledContext is the step itself, in a function of its own so that what the
// row rests on is one line a reader cannot mistake: time.Sleep takes no context, so the cut that
// cancels the handling's context reaches nothing here.
func (s *ignoredContextStep) sleepIgnoringTheCancelledContext() {
	time.Sleep(s.sleep)
}

// deliveries is when each delivery of the fact reached the step, which is how the row sees the
// redelivery the nak after the cut brings.
func (s *ignoredContextStep) deliveries() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.entered...)
}

// firstReturn is what the first delivery recorded: the deadline it was handed and whether it was
// handed one at all, when its sleep returned, and the log as it stood at that moment. Every value
// is zero until that sleep has returned.
func (s *ignoredContextStep) firstReturn() (deadline time.Time, hadDeadline bool, returned time.Time, logs string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadline, s.hadDeadline, s.returned, s.logsAtReturn
}

// This row proves what the intake cut relies on, by driving one fact of the daemon's own Dispatch
// consumer through a handler whose step ignores the context the cut cancels. The limit passes
// while that step is still sleeping, and at the moment the step returns the daemon has written
// neither the cut warning nor the nak: both follow the handler rather than the deadline, because
// consumeMessage waits for handleMessage to return. Once it has returned, both are there and the
// message is delivered again.
//
// The times the row compares are the ones the handling itself recorded — the deadline the handler
// was handed and the instant its sleep returned — rather than a sleep in the test body. A row
// that slept would show only that the warning turns up eventually after a wait longer than the
// limit, which is just as true of a cut that lands at the deadline, and so would prove nothing
// about which of the two ended the handling.
func TestTheCutEndsAHandlerOnceItsStepHonoursTheCancelledContext(t *testing.T) {
	pool := migratedPool(t)
	js, _ := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	spec.HandlerLimit = cutHandlerLimit

	step := &ignoredContextStep{sleep: ignoredSleep, logs: logs}
	stop := startConsume(t, js, spec, pool, step)
	defer stop()
	// One fact, published once the consumers are open, since a durable created where the pool
	// keeps no position for it starts at the next message (openConsumer): every line the row
	// reads is this fact's, and the one redelivery it sees is the cut's.
	publish(t, js, "notifications.dispatch.issue.CAPTURE-350.issue.updated", envelopeJSON(t, "cut-350", "dispatch",
		`{"id":1,"issue_key":"CAPTURE-350","seq":1,"notify":true,"type":"issue.updated","actor":{"kind":"user","id":"someone"},`+
			`"payload":{"key":"CAPTURE-350","status":"backlog","title":"a fact whose step ignores its context","parent":null,"rank":"A","labels":[],"resolved_repo":"sjawhar/legion"}}`))

	testwait.Eventually(t, "the fact whose step ignored its context was cut and delivered again", func() bool {
		return len(step.deliveries()) >= 2
	})
	// The consumers stop on that second delivery, so the row reads a log nothing is still writing
	// and leaves the fact alone rather than working it a third time.
	stop()

	deadline, hadDeadline, returned, atReturn := step.firstReturn()
	if !hadDeadline {
		t.Fatalf("the handler was handed a context with no deadline, so there was no limit for its step to outrun: consumeMessage runs every handling under context.WithTimeout(ctx, spec.HandlerLimit)")
	}
	// First, that the deadline passed while the step was still running, which is what makes the
	// rest of the row a measurement of the cut and not of the handler's own end. The step was
	// entered after the deadline was set and before it passed, so a step that ran its sleep
	// through the deadline returns at least ignoredSleep - HandlerLimit after it; one the limit
	// had ended would return at the deadline.
	late := returned.Sub(deadline)
	if late < ignoredSleep-spec.HandlerLimit {
		t.Fatalf("the step that ignores its context returned %s after the handling's %s deadline, want at least %s: it sleeps %s, so anything less means the step was ended rather than left to run, and the row measures nothing",
			late, spec.HandlerLimit, ignoredSleep-spec.HandlerLimit, ignoredSleep)
	}
	// Second, that at that moment — the deadline long past, the step only now returning — the
	// daemon had done nothing about the message. This is the assertion the claim rests on: the
	// warning and the nak are not the deadline's, they are the handler's return's.
	if line := logLine(atReturn, cutHandlerLine); line != "" {
		t.Errorf("the daemon said an intake handler outran its limit and was cut while that handler's step was still running, %s past its deadline: %s", late, line)
	}
	if line := logLine(atReturn, nakFailure); line != "" {
		t.Errorf("the daemon naked the message while its handler's step was still running, %s past its deadline: %s", late, line)
	}
	// Third, that both happened once the step returned, and that the message went back to the
	// stream then: the cut is real, it was only waiting.
	written := logs.String()
	cut := logLine(written, cutHandlerLine)
	if cut == "" {
		t.Fatalf("no line says an intake handler outran its limit and was cut, and one ran %s past its %s limit, in:\n%s", late, spec.HandlerLimit, written)
	}
	for _, attribute := range []string{"consumer=", "event_id=", "fact=intake.DispatchIssue", "limit=" + spec.HandlerLimit.String()} {
		if !strings.Contains(cut, attribute) {
			t.Errorf("the cut handler's line carries no %s, which is what names the cut: %s", attribute, cut)
		}
	}
	if naked := logLine(written, nakFailure); naked == "" {
		t.Errorf("no line says the cut fact was naked, so nothing returned it to the stream, in:\n%s", written)
	}
	if again := step.deliveries()[1]; !again.After(returned) {
		t.Errorf("the cut fact was delivered again %s before its step returned, want after it: the nak that returns the message to the stream follows the handling, so a delivery before it is not the one this row is counting",
			returned.Sub(again))
	}
}
