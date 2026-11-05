package supervise

// The wake pacing as a consumer sees it: a daemon that must wake many claims at once lets them out
// at the rate its operator set, one claim's wait never costs another its turn, and the requests a
// person is waiting on never wait at all. These tests drive the real limiter (internal/wakepace)
// through supervise.Deps.WakePace, which is how the daemon wires it, on the package's own
// advanceable clock: nothing here sleeps.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

const (
	// wakeRate is the shipped default, eight wakes a minute, and wakeInterval what one costs at it.
	wakeRate     = 8
	wakeInterval = time.Minute / wakeRate
	// wakePart is the jitter every paced release of these tests draws: half an interval, so a
	// test reads an exact schedule out of a jittered limiter.
	wakePart = wakeInterval / 2
)

// secondToken and thirdToken are the other claims a paced claim is paced against: the pacing is a
// property of the daemon's whole queue, so no test of it has one claim only.
const (
	secondToken claim.Token = "legion-LEGION-209-tester"
	thirdToken  claim.Token = "legion-LEGION-209-reviewer"
)

// pacedHarness is a harness whose machines reserve from one limiter of rate wakes a minute in
// bursts of one, every paced release spread by half an interval. A rate of zero is the pacing off.
func pacedHarness(t *testing.T, rate int) *harness {
	t.Helper()
	h := newBareHarness(t)
	limiter := wakepace.New(rate, 1, func() time.Duration { return wakePart })
	h.deps.WakePace = WakePace{Reserve: limiter.Reserve, Rate: rate, Burst: 1}
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// wakeClaim is another claim of the same daemon in state, with the session a resume relaunches.
func wakeClaim(token claim.Token, state ClaimState) Claim {
	c := queuedClaim()
	c.Token, c.State = token, state
	if state != StateQueued {
		c.Generation = 1
		c.Session, c.SessionFile = session, sessionFile
	}
	if state == StateSuspended {
		c.SuspendedBy = SuspendedByDaemon
	}
	return c
}

// machineFor builds a second machine of the harness's daemon: the same store, clock, runtime and
// wake limiter, over the claim given.
func (h *harness) machineFor(c Claim) *Machine {
	h.t.Helper()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		h.t.Fatal(err)
	}
	m, err := NewMachine(h.ctx, h.deps, c)
	if err != nil {
		h.t.Fatalf("new machine for %s: %v", c.Token, err)
	}
	return m
}

// postTo hands m one event and waits for the decision it starts.
func (h *harness) postTo(m *Machine, ev Event) error {
	h.t.Helper()
	err := m.Handle(h.ctx, ev)
	m.Wait()
	return err
}

// mustPostTo is postTo for an event the claim must take.
func (h *harness) mustPostTo(m *Machine, ev Event) {
	h.t.Helper()
	if err := h.postTo(m, ev); err != nil {
		h.t.Fatalf("handle %T on %s: %v", ev, m.Claim().Token, err)
	}
}

// starts is how many processes the runtime was asked to start for token, spawned or resumed.
func (h *harness) starts(token claim.Token) int {
	h.t.Helper()
	started := 0
	for _, method := range []string{"Spawn", "Resume"} {
		for _, call := range h.rt.CallsOf(method) {
			if call.Spec.Claim == token {
				started++
			}
		}
	}
	return started
}

func (h *harness) wantStarts(token claim.Token, want int) {
	h.t.Helper()
	if got := h.starts(token); got != want {
		h.t.Fatalf("the runtime was asked to start %s %d times, want %d", token, got, want)
	}
}

// wantWakeLine is the one line a wake writes as it runs (wakeRan), for this claim and no other.
func (h *harness) wantWakeLine(token claim.Token, fields ...string) string {
	h.t.Helper()
	lines := h.logs.lines(append([]string{"supervise: wake ran", "claim=" + string(token)}, fields...)...)
	if len(lines) != 1 {
		h.t.Fatalf("wake lines of %s matching %v = %d (%s), want 1", token, fields, len(lines),
			strings.Join(h.logs.lines("supervise: wake ran"), " | "))
	}
	return lines[0]
}

// A daemon that has woken nobody holds nobody back: the first launch runs as it is raised, inside
// the decision that raised it.
func TestALoneWakeLaunchesAtOnce(t *testing.T) {
	h := pacedHarness(t, wakeRate)

	h.must(RequestSpawn{Claim: h.token})

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 1)
}

// The line a wake writes as it runs is what the operator counts the wakes of a minute from, so it
// is one shape whether the wake waited or not: the claim, what kind of wake it was, whether the
// pacing held it and for how long, whether it was paced at all, and the pace it was held to.
func TestTheLineAWakeWritesNamesTheWaitAndThePace(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})
	h.clock.Advance(wakeInterval + wakePart)
	second.Wait()

	if line := h.wantWakeLine(h.token, "kind=launch", "waited=false", "paced=true", "rate=8", "burst=1"); strings.Contains(line, "waitedFor=") {
		t.Errorf("the wake that did not wait named a waitedFor: %s", line)
	}
	h.wantWakeLine(secondToken, "kind=launch", "waited=true",
		"waitedFor="+(wakeInterval+wakePart).String(), "paced=true", "rate=8", "burst=1")
}

// The second claim of the same moment waits one interval and the part the jitter drew, and then
// launches: that wait is the whole of what keeps a restart's launches inside the organization's
// per-minute cap.
func TestASecondClaimsWakeWaitsOneIntervalAndAPart(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})

	h.wantStarts(secondToken, 0)
	if state := second.Claim().State; state != StateLaunching {
		t.Fatalf("the waiting claim is %s, want %s", state, StateLaunching)
	}
	h.clock.Advance(wakeInterval + wakePart - time.Nanosecond)
	second.Wait()
	h.wantStarts(secondToken, 0)

	h.clock.Advance(time.Nanosecond)
	second.Wait()

	h.wantStarts(secondToken, 1)
	h.wantWakeLine(secondToken, "waited=true")
}

// A start request repeated against a claim whose wake is still waiting starts no second launch and
// takes no second token: the claim behind it keeps the turn it already holds.
func TestASecondStartOfAWaitingClaimTakesNoSecondToken(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))
	third := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})
	if err := h.postTo(second, RequestSpawn{Claim: secondToken}); err == nil {
		t.Fatal("a second spawn of a claim already launching was taken, want a refusal")
	}
	h.mustPostTo(third, RequestSpawn{Claim: thirdToken})

	h.clock.Advance(2*wakeInterval + 2*wakePart)
	second.Wait()
	third.Wait()

	h.wantStarts(secondToken, 1)
	h.wantStarts(thirdToken, 1)
	h.wantWakeLine(secondToken, "waitedFor="+(wakeInterval+wakePart).String())
	h.wantWakeLine(thirdToken, "waitedFor="+(2*wakeInterval+2*wakePart).String())
}

// A wake whose claim may no longer launch by the time its turn comes launches nothing — the stop
// that retired it let the wake go with the process — and the claim behind it still wakes at the
// moment it was given.
func TestAWakeDroppedBeforeItsTurnLaunchesNothingAndTheNextKeepsItsMoment(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))
	third := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})
	h.mustPostTo(third, RequestSpawn{Claim: thirdToken})
	h.mustPostTo(second, RequestStop{Claim: secondToken})

	h.clock.Advance(2*wakeInterval + 2*wakePart)
	second.Wait()
	third.Wait()

	if state := second.Claim().State; state != StateRetired {
		t.Fatalf("the stopped claim is %s, want %s", state, StateRetired)
	}
	h.wantStarts(secondToken, 0)
	h.wantStarts(thirdToken, 1)
	h.wantWakeLine(thirdToken, "waitedFor="+(2*wakeInterval+2*wakePart).String())
}

// A suspension of a claim whose wake is waiting is answered as a wait, as one of a claim mid-launch
// is, and lands at the wake's turn: the claim is suspended there rather than launched.
func TestASuspendWhileTheWakeWaitsLandsAtItsTurn(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})
	err := h.postTo(second, RequestSuspend{Claim: secondToken, Reason: "the operator drains the daemon", By: SuspendedByOperator})

	if !suspensionHeld(err) {
		t.Fatalf("the suspension of a waiting claim answered %v, want ErrSuspendHeld", err)
	}
	h.clock.Advance(wakeInterval + wakePart)
	second.Wait()

	if state := second.Claim().State; state != StateSuspended {
		t.Fatalf("the suspended claim is %s, want %s", state, StateSuspended)
	}
	h.wantStarts(secondToken, 0)
}

// The operator's resume never waits: it drops the wake the pacing holds, launches the claim at
// once, and takes nothing from the rate — the claim behind it keeps the turn it already holds.
func TestAnExemptResumeOfAWaitingClaimLaunchesAtOnce(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateSuspended))
	third := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestRuleResume{Claim: secondToken, Reason: "it owns its issue's phase"})
	h.wantStarts(secondToken, 0)

	h.mustPostTo(second, RequestResume{Claim: secondToken})

	h.wantStarts(secondToken, 1)
	if state := second.Claim().State; state != StateLaunching {
		t.Fatalf("the resumed claim is %s, want %s", state, StateLaunching)
	}
	h.wantWakeLine(secondToken, "waited=false", "paced=false")
	h.mustPostTo(third, RequestSpawn{Claim: thirdToken})
	h.clock.Advance(2*wakeInterval + 2*wakePart)
	third.Wait()
	h.wantWakeLine(thirdToken, "waitedFor="+(2*wakeInterval+2*wakePart).String())
}

// An operator's forget-session of a waiting claim is decided rather than refused — parked
// launching for its turn, the claim would answer "the claim has a live process or launch", which
// it has not — and the wake it dropped keeps its turn, since the forget-session launched nothing:
// the claim comes up at the moment it was given, spawning fresh rather than resuming the session
// the operator dropped.
func TestAnOperatorsForgetSessionOfAWaitingClaimIsDecidedAndTheWakeKeepsItsTurn(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateSuspended))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestRuleResume{Claim: secondToken, Reason: "it owns its issue's phase"})
	h.mustPostTo(second, RequestForgetSession{Claim: secondToken, Session: session})

	if got := second.Claim(); got.Session != "" || got.SessionFile != "" {
		t.Fatalf("the claim records session %q at %q, want neither", got.Session, got.SessionFile)
	}
	if got := second.Claim(); got.State != StateLaunching || got.Locator != nil {
		t.Fatalf("the claim is %s recording %+v, want %s recording no process: its wake still waits",
			got.State, got.Locator, StateLaunching)
	}
	if stored := h.store.load(secondToken); stored.State != StateLaunching || stored.Session != "" {
		t.Fatalf("the stored claim is %s recording session %q, want %s recording none", stored.State, stored.Session, StateLaunching)
	}

	h.clock.Advance(wakeInterval + wakePart)
	second.Wait()

	h.wantStarts(secondToken, 1)
	for _, resumed := range h.rt.CallsOf("Resume") {
		if resumed.Spec.Claim == secondToken {
			t.Fatalf("the claim resumed %+v, want a fresh spawn: the operator dropped its session", resumed.Spec)
		}
	}
}

// An operator's retry of a waiting claim the gate found suspended is refused, as it is of any
// suspended claim — a suspended claim is resumed, not retried — and the refusal costs the claim
// nothing: the wake it dropped is taken up again and runs at the moment it already held.
func TestAnOperatorsRetryRefusedByTheParkedClaimLeavesItsWakeItsTurn(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateSuspended))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestRuleResume{Claim: secondToken, Reason: "it owns its issue's phase"})
	h.clock.Advance(wakeInterval / 4)
	second.Wait()

	err := h.postTo(second, RequestOperatorRetry{Claim: secondToken})

	var refused *RefusedError
	if !errors.As(err, &refused) || refused.State != StateSuspended {
		t.Fatalf("the operator's retry answered %v, want a refusal naming %s", err, StateSuspended)
	}
	h.clock.Advance(wakeInterval + wakePart - wakeInterval/4 - time.Nanosecond)
	second.Wait()
	h.wantStarts(secondToken, 0)

	h.clock.Advance(time.Nanosecond)
	second.Wait()

	h.wantStarts(secondToken, 1)
	h.wantWakeLine(secondToken, "waited=true", "waitedFor="+(wakeInterval+wakePart).String())
}

// A wake over a claim whose process still runs leaves the claim exactly as it was — its state and
// its locator — so a restart meanwhile finds a running claim rather than a launch with no process.
// The relaunch happens at the wake's turn.
func TestAWakeOverARunningProcessLeavesTheClaimAlone(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	running := h.locator()

	h.observe(runtime.StaleAddress)

	h.wantState(StateReady)
	if got := h.claim().Locator; got == nil || *got != running {
		t.Fatalf("the claim records %+v, want the process it still runs, %+v", got, running)
	}
	if stored := h.store.load(h.token); stored.State != StateReady || stored.Locator == nil {
		t.Fatalf("the stored claim is %s recording %+v, want %s recording its process", stored.State, stored.Locator, StateReady)
	}

	h.advance(wakeInterval + wakePart)

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
}

// A wake of a claim with no process parks it launching with no process and writes it there: the
// outbox's start delivers its task after the launch call, and a claim left failed would be a named
// ignore for that task while a claim left suspended would launch a second time.
func TestAProcesslessWakeParksTheClaimLaunchingAndWritesIt(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})

	stored := h.store.load(secondToken)
	if stored.State != StateLaunching || stored.Locator != nil {
		t.Fatalf("the stored claim is %s recording %+v, want %s recording no process", stored.State, stored.Locator, StateLaunching)
	}
	if err := h.postTo(second, RequestDeliver{Claim: secondToken, Task: "implement the plan"}); err != nil {
		t.Fatalf("a task delivered to a parked claim: %v", err)
	}
	if second.Claim().Pending == nil {
		t.Fatal("the task delivered to a parked claim was not queued")
	}
}

// Rate 0 is the pacing off: every claim launches as it is raised, however many are raised at once.
func TestRateZeroLaunchesEveryClaimAtOnce(t *testing.T) {
	h := pacedHarness(t, 0)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))
	third := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})
	h.mustPostTo(third, RequestSpawn{Claim: thirdToken})

	for _, token := range []claim.Token{h.token, secondToken, thirdToken} {
		h.wantStarts(token, 1)
		h.wantWakeLine(token, "waited=false", "rate=0")
	}
	h.clock.Advance(time.Minute)
	for _, token := range []claim.Token{h.token, secondToken, thirdToken} {
		h.wantStarts(token, 1)
	}
}

// The project controller launches at once whatever the queue holds: a death, a stale address or a
// displaced pod reaches its launch with no request to mark exempt, and the controller is the one
// session the operator's own read of a running wave relies on.
func TestTheControllersClaimLaunchesAtOnce(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	token := claim.ControllerToken("legion")
	controller := h.machineFor(Claim{Token: token, Project: "legion", Role: claim.RoleController, State: StateQueued})

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(controller, RequestSpawn{Claim: token})

	h.wantStarts(token, 1)
	h.wantWakeLine(token, "kind=launch", "waited=false", "paced=false")
}

// suspendHeld is whether err is the answer a suspension held for later gives its caller.
func suspensionHeld(err error) bool { return errors.Is(err, ErrSuspendHeld) }
