package supervise

// The wake pacing as a consumer sees it: a daemon that must wake many claims at once lets them out
// at the rate its operator set, one claim's wait never costs another its turn, and the requests a
// person is waiting on never wait at all. These tests drive the real limiter (internal/wakepace)
// through supervise.Deps.WakePace, which is how the daemon wires it, on the package's own
// advanceable clock: nothing here sleeps.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
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
// bursts of one, every paced release spread by half an interval. A rate of zero is the pacing
// off, and the machines are handed no Reserve at all, which is what the daemon hands them at that
// rate rather than a limiter that paces nothing (the daemon's run, openSupervision): a harness
// that handed one would take a path production never takes, and the line its wakes write would
// say paced=true of a pacing that is off (reserveWake, wakeRan).
func pacedHarness(t *testing.T, rate int) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.WakePace = WakePace{Rate: rate, Burst: 1}
	if rate > 0 {
		h.deps.WakePace.Reserve = wakepace.New(rate, 1, func(time.Duration) time.Duration { return wakePart }).Reserve
	}
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

// wantWaitingWake is the wake the machine holds, of the kind given (WakeWaiting). A test of what
// reaches a claim during its wait reads the moment here and reads it again after, because a wake
// charged a second time is a fresh token at the back of the queue rather than the turn this claim
// already holds (reserveWake, wantWakeDueAt).
func (h *harness) wantWaitingWake(kind WakeKind) WakeWaiting {
	h.t.Helper()
	held := h.m.View().WakeWaiting
	if held == nil || held.Kind != kind {
		h.t.Fatalf("the claim names wake %+v, want a %s wake waiting its turn", held, kind)
	}
	return *held
}

// wantWakeDueAt is the claim still holding the wake it held, at the moment it was given.
func (h *harness) wantWakeDueAt(want WakeWaiting) {
	h.t.Helper()
	if held := h.m.View().WakeWaiting; held == nil || *held != want {
		h.t.Fatalf("the claim names wake %+v, want the one it held, %+v: nothing it met took a second token", held, want)
	}
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

// R2 of dispatch://INFRA-976/spec: a wake that waits blocks nothing and loses nothing, and its
// claim "still decides a suspend, a stop, a tree close". A roll or a restart puts hundreds of
// claims in the queue, so a claim far down it waits tens of minutes, and an operator's drain of
// such a claim must not wait with it: the claim records no process, so the suspension has no
// process to protect and nothing to wait for. It must be decided where it arrives, and written,
// since the hold lives in memory alone (holdSuspension) and no outbox row carries an operator's
// suspend — a restart inside the wait would forget it and the boot would relaunch the claim it
// was told to drain. The wakes behind it keep the turns they already hold, as they do behind
// every other wake the pacing drops.
func TestASuspendOfAWaitingProcesslessClaimIsDecidedWhereItArrives(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateSuspended))
	third := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestRuleResume{Claim: secondToken, Reason: "it owns its issue's phase"})
	h.mustPostTo(third, RequestSpawn{Claim: thirdToken})

	err := h.postTo(second, RequestSuspend{Claim: secondToken, Reason: "the operator drains the daemon", By: SuspendedByOperator})

	if suspensionHeld(err) || err != nil {
		t.Fatalf("the suspension of a waiting claim answered %v, want nil: the claim decides it while its wake waits", err)
	}
	if got := second.Claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByOperator {
		t.Fatalf("the claim is %s suspended by %q, want %s by %q while its wake still waits",
			got.State, got.SuspendedBy, StateSuspended, SuspendedByOperator)
	}
	if stored := h.store.load(secondToken); stored.State != StateSuspended || stored.SuspendedBy != SuspendedByOperator {
		t.Fatalf("the stored claim is %s suspended by %q, want %s by %q: a restart must not relaunch a drained claim",
			stored.State, stored.SuspendedBy, StateSuspended, SuspendedByOperator)
	}

	h.clock.Advance(2*wakeInterval + 2*wakePart)
	second.Wait()
	third.Wait()

	h.wantStarts(secondToken, 0)
	h.wantStarts(thirdToken, 1)
	h.wantWakeLine(thirdToken, "waitedFor="+(2*wakeInterval+2*wakePart).String())
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

// Rate 0 is the pacing off: every claim launches as it is raised, however many are raised at
// once, and each line says the pacing could not have held it (paced=false), which is what the
// operator counts a minute's wakes against the rate from (wakeRan).
func TestRateZeroLaunchesEveryClaimAtOnce(t *testing.T) {
	h := pacedHarness(t, 0)
	second := h.machineFor(wakeClaim(secondToken, StateQueued))
	third := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})
	h.mustPostTo(third, RequestSpawn{Claim: thirdToken})

	for _, token := range []claim.Token{h.token, secondToken, thirdToken} {
		h.wantStarts(token, 1)
		h.wantWakeLine(token, "waited=false", "paced=false", "rate=0")
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

// waveToken is the token of the nth claim of a wave: a daemon that must wake forty claims at
// once is what the pacing exists for, and each of them is a claim of its own.
func waveToken(n int) claim.Token {
	return claim.Token(fmt.Sprintf("legion-LEGION-209-worker-%02d", n))
}

// stalePluginClaim is c as a live worker of an earlier release, which is what a plugin wave finds:
// ready, recording the process the stale-plugin lane decides and the session a relaunch resumes,
// at the plugin contract its registration declared.
func stalePluginClaim(c Claim, contract int) Claim {
	c.State = StateReady
	c.Generation = 1
	c.Session, c.SessionFile = session, sessionFile
	c.PluginContract = contract
	c.Locator = &runtime.Locator{
		Runtime:     runtime.RuntimeTmux,
		Claim:       c.Token,
		Incarnation: "4242:77",
		Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
	}
	return c
}

// A plugin wave is forty stale live claims found in one pass, each of them able to answer a round:
// their update tasks are wakes of the daemon's own — each wakes a live session in place of a
// launch — so they go out at the same paced spacing a wave of launches does, one claim's round
// opening at each turn. Nothing is relaunched: the whole of what a claim that can answer costs is
// the task.
func TestAWaveOfStalePluginsPostsItsUpdateTasksAtThePacedSpacing(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	const wave = 40
	machines := make([]*Machine, wave)
	for i := range machines {
		machines[i] = h.machineFor(stalePluginClaim(wakeClaim(waveToken(i), StateReady), PodUpdateContract))
	}

	for i, m := range machines {
		h.mustPostTo(m, StalePlugin{Claim: waveToken(i), Trigger: "sweep"})
	}

	round := PluginRound(h.deps.PluginContract)
	for i, m := range machines {
		if i > 0 {
			if got := m.Claim().UpdateRound; got != "" {
				t.Fatalf("claim %d holds round %q before its turn, want none: its update task waits for the pacing", i, got)
			}
			h.clock.Advance(wakeInterval + wakePart)
			m.Wait()
		}
		if got := m.Claim().UpdateRound; got != round {
			t.Fatalf("claim %d holds round %q at its turn, want %q", i, got, round)
		}
	}
	for i := range machines {
		h.wantStarts(waveToken(i), 0)
		h.wantWakeLine(waveToken(i), "kind=plugin-task")
	}
}

// The sweep posts StalePlugin to every stale live claim at every tick, and a claim whose update
// task is merely queued is not one it skips, so the same claim is posted again every minute of a
// wave. Those re-posts must be free: the claim already holds a wake, so each of them returns
// having taken no token and opened no second round. What the limiter did is read through the claim
// behind it — one that waits two turns rather than sixty-two.
func TestAMinuteOfStalePluginRePostsOverAWaitingWakeTakesNoSecondToken(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	stale := h.machineFor(stalePluginClaim(wakeClaim(secondToken, StateReady), PodUpdateContract))
	behind := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(stale, StalePlugin{Claim: secondToken, Trigger: "boot"})
	for range 60 {
		h.mustPostTo(stale, StalePlugin{Claim: secondToken, Trigger: "sweep"})
	}
	h.mustPostTo(behind, RequestSpawn{Claim: thirdToken})

	if stored := h.store.load(secondToken); stored.State != StateReady || stored.Locator == nil || stored.UpdateRound != "" {
		t.Fatalf("the stored claim is %s recording %+v in round %q, want %s recording its process in none: the gate left it as it stands",
			stored.State, stored.Locator, stored.UpdateRound, StateReady)
	}
	h.clock.Advance(wakeInterval + wakePart)
	stale.Wait()

	if got, want := stale.Claim().UpdateRound, PluginRound(h.deps.PluginContract); got != want {
		t.Fatalf("the claim holds round %q at its turn, want %q", got, want)
	}
	if opened := h.logs.lines("supervise: an update round is open on this claim", "claim="+string(secondToken)); len(opened) != 1 {
		t.Fatalf("the claim opened %d rounds, want 1: %s", len(opened), strings.Join(opened, " | "))
	}
	h.wantStarts(secondToken, 0)
	h.wantWakeLine(secondToken, "kind=plugin-task", "waited=true", "waitedFor="+(wakeInterval+wakePart).String())

	h.clock.Advance(wakeInterval + wakePart)
	behind.Wait()

	h.wantStarts(thirdToken, 1)
	h.wantWakeLine(thirdToken, "kind=launch", "waitedFor="+(2*(wakeInterval+wakePart)).String())
}

// A post that becomes a relaunch rather than a task — a worker whose plugin has no
// ready_for_update op to answer a round with — reserves nothing in refreshPlugin: its launch takes
// the one token, at the gate every launch passes (launchAfter). One token, one wake line, and the
// claim behind it waits two turns rather than three.
func TestAStalePluginRelaunchTakesItsOneTokenAtTheLaunchGate(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	stale := h.machineFor(stalePluginClaim(wakeClaim(secondToken, StateReady), PodUpdateContract-1))
	behind := h.machineFor(wakeClaim(thirdToken, StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(stale, StalePlugin{Claim: secondToken, Trigger: "sweep"})
	h.mustPostTo(behind, RequestSpawn{Claim: thirdToken})

	h.wantStarts(secondToken, 0)
	h.clock.Advance(wakeInterval + wakePart)
	stale.Wait()

	h.wantStarts(secondToken, 1)
	if got := stale.Claim().UpdateRound; got != "" {
		t.Fatalf("the relaunched claim holds round %q, want none: a worker that cannot answer is relaunched instead", got)
	}
	h.wantWakeLine(secondToken, "kind=launch", "waited=true", "waitedFor="+(wakeInterval+wakePart).String())

	h.clock.Advance(wakeInterval + wakePart)
	behind.Wait()

	h.wantStarts(thirdToken, 1)
	h.wantWakeLine(thirdToken, "kind=launch", "waitedFor="+(2*(wakeInterval+wakePart)).String())
}

// The project controller's own claim is held by neither branch of the stale-plugin lane, whatever
// the queue holds. It is relaunched at once rather than asked: the `legion` tool its session holds
// has no ready_for_update op, so a round over it would stay open for ever — and the relaunch takes
// no token, since the operator's own read of a running wave is this one session's.
func TestTheControllersStalePluginIsHeldByNeitherBranch(t *testing.T) {
	for _, state := range []ClaimState{StateReady, StateWorking} {
		t.Run(string(state), func(t *testing.T) {
			h := pacedHarness(t, wakeRate)
			h.deps.PluginContract = PodUpdateContract + 1
			c := stalePluginClaim(controllerClaim(), PodUpdateContract)
			c.State = state
			if state == StateWorking {
				c.Pending = &Delivery{ID: "delivery-1", Task: "Legion controller start", QueuedAt: epoch, DeliveredAt: epoch, ConfirmedAt: epoch}
			}
			controller := h.machineFor(c)

			h.must(RequestSpawn{Claim: h.token})
			h.mustPostTo(controller, StalePlugin{Claim: c.Token, Trigger: "sweep"})

			h.wantStarts(c.Token, 1)
			if got := controller.Claim().UpdateRound; got != "" {
				t.Fatalf("the controller holds round %q, want none: no controller turn can answer one", got)
			}
			h.wantWakeLine(c.Token, "kind=launch", "waited=false", "paced=false")
		})
	}
}

// A registration that lands while the update task waits its turn takes the claim past the fact
// the task was reserved for, so the turn opens no round: a round over a worker already on this
// daemon's plugin would ask it to push and answer, hold its next phase's tasks behind that ask,
// and end hours later in a relaunch of a current process (openPluginUpdateIfStale). The token is
// spent either way — the limiter charges on the reserve — which is why the pass that reserves
// and the turn that runs each make the comparison for themselves.
func TestAPluginWakeWhoseClaimRegisteredCurrentOpensNoRound(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	stale := h.machineFor(stalePluginClaim(wakeClaim(secondToken, StateReady), PodUpdateContract))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(stale, StalePlugin{Claim: secondToken, Trigger: "sweep"})
	h.mustPostTo(stale, RequestRegister{
		Claim: secondToken, Generation: 1, Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract,
	})

	h.clock.Advance(wakeInterval + wakePart)
	stale.Wait()

	if got := stale.Claim().UpdateRound; got != "" {
		t.Fatalf("the claim holds round %q, want none: it registered on this daemon's plugin while its update task waited its turn", got)
	}
	h.wantStarts(secondToken, 0)
	h.wantWakeLine(secondToken, "kind=plugin-task", "waited=true")
}

// What a machine tells a reader of every claim about the wake it is holding
// (ClaimView.WakeWaiting, which the daemon's state is built from): its kind and the moment the
// pacing gave it, for as long as it waits, and nothing at all once its turn has run it. The wake
// travels on the published view rather than behind the machine's lock because the reader asking is
// asking about every claim at once, and a claim mid-relaunch holds its lock for as long as the
// runtime waits out its pods.
func TestTheViewNamesTheWakeTheMachineHoldsUntilItHasRun(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	launching := h.machineFor(wakeClaim(secondToken, StateQueued))
	stale := h.machineFor(stalePluginClaim(wakeClaim(thirdToken, StateReady), PodUpdateContract))
	reserved := h.clock.Now()

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(launching, RequestSpawn{Claim: secondToken})
	h.mustPostTo(stale, StalePlugin{Claim: thirdToken, Trigger: "sweep"})

	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim the burst let out names wake %+v, want none: it launched as it was raised", held)
	}
	for _, waiting := range []struct {
		m     *Machine
		kind  WakeKind
		turns int
	}{
		{launching, WakeLaunch, 1},
		{stale, WakePluginTask, 2},
	} {
		held := waiting.m.View().WakeWaiting
		at := reserved.Add(time.Duration(waiting.turns) * (wakeInterval + wakePart))
		if held == nil || held.Kind != waiting.kind || !held.At.Equal(at) {
			t.Fatalf("%s names wake %+v, want a %s wake due at %s", waiting.m.View().Token, held, waiting.kind, at)
		}
	}

	h.advance(2 * (wakeInterval + wakePart))
	launching.Wait()
	stale.Wait()

	for _, m := range []*Machine{launching, stale} {
		if held := m.View().WakeWaiting; held != nil {
			t.Fatalf("%s still names wake %+v once its turn has run it, want none", m.View().Token, held)
		}
	}
	h.wantStarts(secondToken, 1)
	if got, want := stale.Claim().UpdateRound, PluginRound(h.deps.PluginContract); got != want {
		t.Fatalf("the stale claim holds round %q at its turn, want %q", got, want)
	}
}

// Check c-nonblocking of dispatch://INFRA-976/spec, over a claim the gate parked above a process
// that still runs. Such a claim is handed no task while its launch waits, because that launch
// lets the process go at its turn (launchNow): a worker started on a task moments before its
// process is killed loses both the turn and the work, and the task it had begun is retired as
// served at a turn nothing finished (settle, retirePending). The task waits instead, exactly as
// a launching claim's does, and goes to the process the launch starts.
//
// The path here is a rollout of the plugin contract, which is where the window is widest: a bump
// makes every live claim stale, each takes the plugin-task lane, and the end of the round its
// worker answered relaunches the process through the paced gate (endPodUpdate, replaceProcess).
// The round's end clears the hold that kept the claim's next task back (answeredOpenRound), so
// without this guard the next task goes to the worker the relaunch is about to kill. repoint's
// stale address and relaunchStale open the same window. The task is the workflow's own, named by
// the outbox row it comes from: an operator's task, which names no row, runs that launch where
// it arrives and is queued for the process the launch starts
// (runParkedLaunchNow, TestAnOperatorsDeliveryOverAParkedLaunchRunsThatLaunchNow).
//
// The negative control is the guard reverted (sendPending's launchWaits): the task is then handed
// to the process the launch replaces, which this test reads at the prompts.
func TestATaskDeliveredWhileALaunchWaitsItsTurnWaitsForTheProcessThatLaunchStarts(t *testing.T) {
	const next = "review the pull request"
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	round := PluginRound(h.m.deps.PluginContract + 1)

	// The round as its worker answers one: it reads the ask in a turn, pushes its work, answers
	// ready_for_update, and ends that turn. The claim is idle with its hands empty when the
	// round's end relaunches it, which is the state the sweep's end finds.
	h.openUpdate(round, testReasons)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	h.must(PodUpdateEnded{Claim: h.token, Round: round})

	replaced := h.locator()
	if got := h.claim(); got.State != StateIdle || got.Locator == nil || *got.Locator != replaced {
		t.Fatalf("the claim whose relaunch waits is %s recording %+v, want %s recording the process it still runs",
			got.State, got.Locator, StateIdle)
	}

	h.must(RequestDeliver{Claim: h.token, Task: next, ID: "outbox:42", Row: 42, Generation: 7})

	h.wantPrompts(1)
	if p := h.pending(); p.Task != next || !p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the task delivered while the launch waits is %+v, want %q queued unread: the process it would reach is the one this launch replaces",
			p, next)
	}

	h.advance(wakeInterval + wakePart)

	h.wantState(StateLaunching)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != replaced {
		t.Fatalf("the launch at its turn replaced %+v, want the process the gate left running, %+v", resume.Previous, replaced)
	}
	if p := h.pending(); p.Task != next || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the task after the launch's turn is %+v, want %q still waiting: no turn of it ever ran", p, next)
	}

	h.relaunched()

	sent := h.wantPrompts(2)
	if sent[1].Message != next {
		t.Fatalf("the prompts are %+v, want the round's ask and then the task, to the process the launch started", sent)
	}
}

// The other half of that race, for the turn the gate cannot see: a process parked over by a
// launch can take a turn on a task of its own account — a send already in flight when the wake
// parked, a question a restart armed, or, as here, a reconnecting shim replaying the start of a
// turn it already saw, which is the one start that names its delivery (turnStarted) — and a task
// confirmed by that turn is one settle retires as served the moment the launch writes the claim
// launching, though the turn was cut off by the launch itself (retirePending). So the launch's
// turn takes such a turn back before it lets the process go, exactly as replaceProcess does
// before the relaunch it makes (interrupted): the task goes back unconfirmed under a new id,
// marked interrupted, and the resumed agent is told to carry on from where the turn stopped.
//
// A prompt acknowledged before the park is not one of those turns any more: the park disarms the
// wait for it with the rest of the dead-or-replaced process's timers (parkWake), so a bare start
// in that window is attributed to no delivery and nothing is confirmed for the launch to take
// back.
//
// The negative control is that take-back reverted (wakeNow's interrupted): the task is then gone
// from the claim at its turn, which this test reads where it asks for the pending task.
func TestATurnTheReplacedProcessConfirmedIsTakenBackAtTheLaunchsTurn(t *testing.T) {
	const task = "implement the plan"
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.wantPrompts(1)
	sent := h.pending().ID

	h.observe(runtime.StaleAddress)
	h.must(StreamTurnStart{Claim: h.token, DeliveryID: sent})

	if got := h.claim(); got.State != StateWorking {
		t.Fatalf("the claim whose process took the task is %s, want %s while its launch waits", got.State, StateWorking)
	}
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("the task is %+v, want it confirmed by the turn the replaced process started", p)
	}

	h.advance(wakeInterval + wakePart)

	h.wantState(StateLaunching)
	p := h.pending()
	if !p.ConfirmedAt.IsZero() || !p.Interrupted || p.ID == sent {
		t.Fatalf("the task at the launch's turn is %+v, want it taken back unconfirmed under a new id and marked interrupted: the launch cut its turn off",
			p)
	}
	if p.Task != task || p.DeliveredAt.IsZero() {
		t.Fatalf("the task taken back is %+v, want %q with the read mark it kept: its turn ran", p, task)
	}
	if stored, ok := h.store.delivery(h.token); !ok || stored.ID != p.ID || !stored.ConfirmedAt.IsZero() {
		t.Fatalf("the stored delivery is %+v (%v), want the task taken back: a restart must not read it as served", stored, ok)
	}
}

// A take-back the store refuses at that turn leaves the claim exactly as it stood, task
// included. The claim here is idle holding a task its turn confirmed, which is the window Oh My
// Pi's result has not closed yet: while the daemon waits for that result, no turn that ends is
// known to be this prompt's, so settle retires nothing (resultPending, turnOver). The launch's
// turn takes that confirmed task back (interrupted) and the store refuses the write, so the
// task is still confirmed under the id the store holds — and the wait for the result is still
// the claim's, because interrupted ends it with the write rather than before it. A claim left
// confirmed with no wait outstanding is one the next settlement retires as served, and that
// settlement runs with the very decision the write failed in (Handle, settle): the task would
// go while its worker was still on it, on the process this turn did not replace.
//
// The negative control is that ordering reverted (interrupted's awaitingResult, cleared above
// the write): the settlement that follows the turn then retires the task, which this test reads
// where it asks the claim for it.
func TestATakeBackTheStoreRefusesAtTheTurnRetiresNoTask(t *testing.T) {
	const task = "implement the plan"
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: task, ID: "outbox:7", Row: 7, Generation: 7})
	sent := h.pending().ID

	// The launch parks over the process the agent is on, and that process takes a turn on the
	// task it was already handed: the turn starts inside the wait and ends inside it, with Oh
	// My Pi's result for the prompt still to come.
	h.observe(runtime.StaleAddress)
	waiting := h.wantWaitingWake(WakeLaunch)
	h.must(StreamTurnStart{Claim: h.token, DeliveryID: sent})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	h.wantState(StateIdle)
	if p := h.pending(); p.ID != sent || p.ConfirmedAt.IsZero() {
		t.Fatalf("the task after the turn ended is %+v, want %s still confirmed: the prompt's result decides whose turn that was", p, sent)
	}

	h.store.fail("PutDelivery", errBoom)
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 1)
	p := h.claim().Pending
	if p == nil {
		t.Fatal("the claim holds no task after the take-back its store refused, want the one its worker is on")
	}
	if p.ID != sent || p.Task != task || p.ConfirmedAt.IsZero() {
		t.Fatalf("the task after the refused take-back is %+v, want %s confirmed as the store holds it", p, sent)
	}
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("the claim serves run %d, want the task's 7", run)
	}
}

// A relaunch that same refused take-back abandons leaves no mark. A stale-plugin relaunch marks
// nothing where it is decided: the launch it raises writes PluginRelaunchFor as it starts the
// process that replaces the stale one (markPluginRelaunch, launchNow), so the mark waits in the
// wake while the pacing holds that launch. The turn that cannot take the confirmed turn back
// starts nothing — the process the claim records is kept, the wake is spent and nothing is left
// to run that launch — so no mark is written at all. A mark there would make the next
// registration of that very process the second stale one in a row and charge it toward failed
// for a relaunch that never ran (staleRegistration); with none, that registration is the first
// this claim has had relaunched, so it is relaunched for free.
//
// The negative control is the mark committed at the top of runWake, before its take-back
// (`m.holdPluginRelaunch(waiting.pluginMark); m.commitPluginRelaunch()` ahead of checkLaunch):
// the claim then names this daemon's contract for a relaunch whose take-back the store refused,
// and the re-registration takes the charged path, which this test reads at the mark and at the
// budgets.
func TestATakeBackTheStoreRefusesAtTheTurnLeavesNoMarkBehind(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true

	// The agent acknowledges a task, the sweep finds the claim's plugin stale and raises the
	// relaunch, and that launch waits its turn over the process the agent is on. The turn of the
	// task starts and ends inside the wait, with Oh My Pi's result still to come, so the launch's
	// turn finds a confirmed task to take back.
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", ID: "outbox:7", Row: 7, Generation: 7})
	sent := h.pending().ID
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	waiting := h.wantWaitingWake(WakeLaunch)
	h.must(StreamTurnStart{Claim: h.token, DeliveryID: sent})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	if got := h.claim(); got.State != StateIdle || got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim whose stale-plugin relaunch waits is %s marked for %d, want %s marked for 0: its launch has not run",
			got.State, got.PluginRelaunchFor, StateIdle)
	}

	h.store.fail("PutDelivery", errBoom)
	h.advance(waiting.At.Sub(h.clock.Now()))
	h.store.fail("PutDelivery", nil)

	h.wantStarts(h.token, 1)
	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Errorf("the claim is marked for %d after the take-back its store refused, want 0: the launch that mark names never ran",
			got.PluginRelaunchFor)
	}

	// The agent of the process that was never replaced registers again, on the contract it is
	// still running: the first stale registration this claim has had relaunched, so it is
	// relaunched for free and refused as every stale registration is.
	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})

	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(24, 25) {
		t.Fatalf("the re-registration after the refused take-back = %v, want the stale-plugin refusal naming 24 and 25", err)
	}
	h.wantBudgets(Budgets{})

	// That relaunch waits its turn like the first, and marks the claim where it runs.
	again := h.wantWaitingWake(WakeLaunch)
	h.advance(again.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	h.wantBudgets(Budgets{})
	if got := h.claim(); got.PluginRelaunchFor != 25 {
		t.Errorf("the claim after the relaunch that registration raised is marked for %d, want 25", got.PluginRelaunchFor)
	}
}

// One death is charged once, however many reports of it arrive. A claim whose relaunch waits its
// turn still records the process it found dead, so a second report of that same incarnation
// reaches the judgement every report passes through — the runtime's own sweep makes one every
// interval — and before this guard that second report ran the whole death again: another death
// and another launch failure charged, and the waiting relaunch replaced by a fresh token at the
// back of the queue. In a wave a claim with work outstanding then failed after two real deaths
// rather than at launch_failure_limit, and the claim behind it lost the turn the queue had
// already given it. The four other paths that report a process to that judgement — a closed
// connection, the boot interval, the probe interval and the registration deadline — have a test
// of their own below.
//
// The negative control is the guard reverted (judge's launchWaits): the budgets then read two
// deaths and two launch failures, and the claim behind waits a turn longer, both of which this
// test reads.
func TestASecondReportOfTheOneDeathOverAWaitingRelaunchChargesNothing(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	behind := h.machineFor(wakeClaim(secondToken, StateQueued))
	h.reach(StateWorking)
	dead := h.locator()

	h.observe(runtime.Gone)
	h.observe(runtime.Gone)
	h.mustPostTo(behind, RequestSpawn{Claim: secondToken})

	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	if got := h.claim(); got.Locator == nil || *got.Locator != dead {
		t.Fatalf("the claim whose relaunch waits records %+v, want the process it found dead, %+v", got.Locator, dead)
	}
	h.wantStarts(h.token, 1)

	h.advance(wakeInterval + wakePart)

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	h.wantWakeLine(h.token, "kind=launch", "waited=true", "waitedFor="+(wakeInterval+wakePart).String())

	h.clock.Advance(wakeInterval + wakePart)
	behind.Wait()

	h.wantStarts(secondToken, 1)
	h.wantWakeLine(secondToken, "waitedFor="+(2*(wakeInterval+wakePart)).String())
}

// The same death through the second path that reports one, and the order a restart wave makes:
// the sweep reports the process gone, and the connection it held closes behind it (closed,
// table.go). That path asks the runtime about the process before it judges anything, and while
// the launch waits it asks nothing: probe answers it where it stands (launchWaits), so no call
// is made and the one death stays charged once. The wake keeps the turn the queue gave it
// rather than going to the back with a fresh token.
//
// The negative control is probe's guard reverted (probe's launchWaits): the closed connection
// then probes the process, which this test reads at the runtime's probes. The budgets would not
// read it — judge's guard answers whatever that probe reports — so the probes are where the two
// guards are told apart.
func TestAClosedConnectionAfterTheSweepsDeathChargesNothingMore(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	behind := h.machineFor(wakeClaim(secondToken, StateQueued))
	h.reach(StateWorking)
	dead := h.locator()

	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	h.mustPostTo(behind, RequestSpawn{Claim: secondToken})

	// The shim's connection closes behind the process the sweep found gone.
	h.must(StreamClosed{Claim: h.token})

	h.wantCalls("Probe", 0)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	h.wantWakeDueAt(waiting)
	if got := h.claim(); got.Locator == nil || *got.Locator != dead {
		t.Fatalf("the claim whose relaunch waits records %+v, want the process it found dead, %+v", got.Locator, dead)
	}
	h.wantStarts(h.token, 1)

	h.advance(wakeInterval + wakePart)

	h.wantStarts(h.token, 2)
	h.clock.Advance(wakeInterval + wakePart)
	behind.Wait()
	h.wantStarts(secondToken, 1)
	h.wantWakeLine(secondToken, "waitedFor="+(2*(wakeInterval+wakePart)).String())
}

// A process that dies while it is booting is the common failure of a restart wave, and the park
// keeps that dead process recorded until the launch's turn (parkWake). Its boot interval goes
// with the rest of that process's timers at the park, so nothing of it falls inside the wait:
// the probe a boot interval makes would be about a process whose fate the waiting relaunch has
// already decided, and the turn is what replaces it and arms the new process's own (armBoot).
// The test reads the timer gone for the whole of the wait and no probe made inside it.
//
// The negative control is the disarm reverted (parkWake's timers): the boot interval is then
// armed over the dead process and fires inside the wait, which this test reads where it asks
// the machine for that timer. Nothing else of the test moves with that one revert — the probe
// such a timer makes is stopped by probe's own guard, and what a probe reported would be
// stopped by judge's — so the disarm is what keeps the timer from firing at all, and those two
// are what keep a timer that did fire from charging the one death a second time.
func TestTheBootIntervalOfADeadProcessIsNotFiredWhileItsRelaunchWaits(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateShimConnected)

	// The shim said hello and the pod then went: the boot interval is armed from the launch, so
	// what is left of it when the sweep reports the death is less than the wait the queue gives
	// the relaunch. The queue is what makes that wait: the burst the limiter saved over the boot
	// interval goes to a claim of the wave, so the relaunch waits a turn.
	h.advance(testBoot - wakeInterval)
	queueAhead(h, 1)
	h.observe(runtime.Gone)

	// A claim whose agent never reported ready holds no task, so its death charges the launch
	// alone and no death with work outstanding (chargeDeath).
	h.wantBudgets(Budgets{LaunchFailures: 1})
	waiting := h.wantWaitingWake(WakeLaunch)
	h.wantCalls("Probe", 0)
	if _, armed := h.m.timers[TimerBoot]; armed {
		t.Fatal("the boot interval of the dead process is armed, want the park to have disarmed it with the process's other timers")
	}

	// The process really is gone, so that is what the runtime answers anything that probes it.
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
	h.advance(wakeInterval + wakePart/2)

	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Probe", 0)
	h.wantWakeDueAt(waiting)
	if _, armed := h.m.timers[TimerBoot]; armed {
		t.Fatal("the boot interval is armed inside the wait, want nothing of the dead process watching it until the turn")
	}

	h.advance(wakePart)

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
}

// The registration deadline is the other timer of a dead process the park takes: an agent that
// said hello and registered has that long to say it is ready, and a claim whose process dies in
// the middle of it is parked over the dead process with the deadline still to run. Fired inside
// the wait it would be a deadline over a process the waiting relaunch has already decided — it
// would read the death a second time and retire a process already gone — so the park disarms it
// with the boot interval, and the launch at the turn arms the new process's own
// (armRegistration). The test reads the timer gone for the whole of the wait and no probe made
// inside it.
//
// The negative control is the disarm reverted (parkWake's timers): the deadline is then armed
// over the dead process and fires inside the wait, which this test reads where it asks the
// machine for that timer. Nothing else of the test moves with that one revert — the probe the
// deadline makes is stopped by probe's own guard, and what a probe reported would be stopped by
// judge's — so the disarm is what keeps the deadline from firing at all, and those two are what
// keep one that did fire from retiring a process already gone.
func TestTheRegistrationDeadlineOfADeadProcessIsNotFiredWhileItsRelaunchWaits(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateRegistered)

	// The agent registered and never said it was ready; its deadline has nearly run out when the
	// sweep finds the process gone, and a claim of the wave takes the burst the limiter saved
	// meanwhile, so the relaunch waits a turn.
	h.advance(deadline - wakeInterval)
	queueAhead(h, 1)
	h.observe(runtime.Gone)

	waiting := h.wantWaitingWake(WakeLaunch)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Probe", 0)
	if _, armed := h.m.timers[TimerRegistration]; armed {
		t.Fatal("the registration deadline of the dead process is armed, want the park to have disarmed it with the process's other timers")
	}

	// The process really is gone, so that is what the runtime answers anything that probes it.
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
	h.advance(wakeInterval + wakePart/2)

	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Probe", 0)
	h.wantWakeDueAt(waiting)
	if _, armed := h.m.timers[TimerRegistration]; armed {
		t.Fatal("the registration deadline is armed inside the wait, want nothing of the dead process watching it until the turn")
	}
	if got := h.claim(); got.State == StateRetired {
		t.Fatal("the claim was retired by a deadline inside the wait, want the launch's turn to decide it")
	}

	h.advance(wakePart)

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
}

// Nothing probes the process a waiting launch replaces, so no probe interval ever falls inside
// the wait. The probe interval was the one of the four timers that could still be armed there:
// the park disarms the others (parkWake), and a closed connection whose probe the runtime could
// not answer arms this one again (probe), which would ask the runtime about a process whose fate
// the launch has already decided, every interval of a wait that is tens of minutes long in a
// wave. So the probe makes no call and arms nothing while the launch waits, and the report that
// does arrive — the sweep's own — is the one judge answers (launchWaits).
//
// The wait here is a queue of four turns, which is what a wave looks like from one claim: the
// probe interval would fall inside it, where at the head of an empty queue the turn would come
// first.
//
// The negative control is the guard reverted (probe's launchWaits): the closed connection then
// probes the process, the runtime's error arms the interval, and the probe at that interval asks
// about the process again, which this test reads at the runtime's probes.
func TestTheProcessAWaitingLaunchReplacesIsNotProbedInsideTheWait(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateWorking)
	queueAhead(h, 3)
	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)

	// The connection closes behind the process the sweep found gone. The runtime would answer
	// that it cannot say what became of the pod, which is the answer that arms the interval.
	h.rt.ScriptProbe(fake.ProbeResult{Err: errors.New("the node the pod ran on is unreachable")})
	h.must(StreamClosed{Claim: h.token})

	h.wantCalls("Probe", 0)
	if _, armed := h.m.timers[TimerProbe]; armed {
		t.Fatal("the closed connection armed the probe interval inside the wait, want nothing of the process the launch replaces")
	}

	h.advance(testProbe)

	h.wantCalls("Probe", 0)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	h.wantWakeDueAt(waiting)
	h.wantStarts(h.token, 1)

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
}

// queueAhead puts n claims of a wave in the queue, each taking the turn it is given, so that the
// next wake reserved waits n turns more than it would at the head of an empty queue. That is what
// a restart or a plugin wave does to one claim's wait, and it is what puts a timer of the process
// a launch waits over inside that wait.
func queueAhead(h *harness, n int) {
	h.t.Helper()
	for i := range n {
		h.mustPostTo(h.machineFor(wakeClaim(waveToken(i), StateQueued)), RequestSpawn{Claim: waveToken(i)})
	}
}

// A launch rechecks the tree lifecycle at its turn, because the turn can come tens of minutes
// after the check that reserved it (checkLaunch, which lifecycle.go states of every launch). The
// gap is an operator's close: it reserves the tree's cleanup before it stops the tree's claims, so
// a wake that fires between the two would launch a process into a tree whose cleanup is reserved,
// and a resume AdmitClaim refused leaves a re-parked wake in the same place (reparkWake). The
// refusal goes where launchAfter's own goes: a reserved cleanup suspends the claim for the daemon,
// its session kept, for the tree's close to retire (relaunchRefused).
//
// The negative control is the recheck reverted (wakeNow's checkLaunch): the turn then starts a
// process in a tree whose cleanup is reserved, which this test reads at the runtime's starts.
func TestAWakeWhoseTreeCleanupWasReservedWhileItWaitedLaunchesNothing(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	second := h.machineFor(wakeClaim(secondToken, StateSuspended))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestRuleResume{Claim: secondToken, Reason: "it owns its issue's phase"})
	if got := second.Claim(); got.State != StateLaunching || got.Locator != nil {
		t.Fatalf("the claim whose wake waits is %s recording %+v, want %s recording no process", got.State, got.Locator, StateLaunching)
	}
	second.deps.Store = &refusableCheckLaunchStore{memStore: h.store, refuse: true}

	h.clock.Advance(wakeInterval + wakePart)
	second.Wait()

	h.wantStarts(secondToken, 0)
	if got := second.Claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon {
		t.Fatalf("the claim whose tree cleanup was reserved while its wake waited is %s suspended by %q, want %s by %q: the close retires it",
			got.State, got.SuspendedBy, StateSuspended, SuspendedByDaemon)
	}
	if got := second.Claim(); got.Session == "" {
		t.Fatal("the suspended claim records no session, want the one the tree's close reaps")
	}
}

// unansweredCheckLaunchStore is a store whose lifecycle recheck fails while failing is set, with
// a failure that is not the tree's — the store did not answer — which is the refusal a launch's
// turn can meet over a process the claim still records and the one that clears by itself
// (relaunchRefused, which answers a reserved cleanup differently). A test sets it after the
// launch it means to refuse has parked, since every launch checks the lifecycle before the gate
// as well (launchAfter).
type unansweredCheckLaunchStore struct {
	*memStore
	failing bool
}

func (s *unansweredCheckLaunchStore) CheckLaunch(ctx context.Context, c Claim) error {
	if s.failing {
		return fmt.Errorf("check the launch of %s: the store did not answer", c.Token)
	}
	return s.memStore.CheckLaunch(ctx, c)
}

// A launch refused at its turn over a process the claim still records leaves that process
// watched. The park disarmed the boot interval and the registration deadline of the process the
// launch was to replace (parkWake), the wake is spent by the time the recheck refuses, and the
// refusal keeps the claim exactly as it stands (runWake, relaunchRefused): without the timers
// armed again (watchAgain) a process still booting would have nothing left to retire it, and a
// pod whose agent never registers would sit in shim_connected for the rest of the tree's life.
// With them armed, the deadline retires it and relaunches on the claim's own launch budget,
// which is what an unpaced daemon's refusal left behind.
//
// The negative control is the arming reverted (runWake's watchAgain): the claim then stands
// shim_connected over the booting process with no timer armed for it, and past the deadline
// nothing has retired that process and nothing is charged, which this test reads at the timers
// and at the runtime's suspends.
func TestALaunchRefusedAtItsTurnLeavesItsBootingProcessWatched(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateShimConnected)
	booting := h.locator()
	store := &unansweredCheckLaunchStore{memStore: h.store}
	h.m.deps.Store = store

	// The runtime reports the process at another address, which relaunches it; the launch waits
	// its turn over the process it is to replace, and the park takes that process's timers.
	h.observe(runtime.StaleAddress)
	waiting := h.wantWaitingWake(WakeLaunch)
	for _, kind := range []TimerKind{TimerBoot, TimerRegistration} {
		if _, armed := h.m.timers[kind]; armed {
			t.Fatalf("the park left the %s timer of the process the launch replaces armed, want it disarmed", kind)
		}
	}

	store.failing = true
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateShimConnected)
	h.wantStarts(h.token, 1)
	h.wantBudgets(Budgets{})
	if got := h.claim(); got.Locator == nil || *got.Locator != booting {
		t.Fatalf("the claim records %+v, want the booting process the refused launch kept, %+v", got.Locator, booting)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim names wake %+v, want none: the turn ran and its launch was refused", held)
	}
	for _, kind := range []TimerKind{TimerBoot, TimerRegistration} {
		if _, armed := h.m.timers[kind]; !armed {
			t.Fatalf("the %s timer of the kept process is not armed, want the refusal watching it again", kind)
		}
	}

	// The store answers again, and the agent of that process never registers: its deadline
	// retires the process and relaunches, which is the whole of what still watches the claim.
	store.failing = false
	h.advance(deadline)

	h.wantCalls("Suspend", 1)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantStarts(h.token, 2)
}

// The third timer the same refusal hands back: the bound a prompt the agent acknowledged waits
// for its turn on (acked). The park takes it with the rest of the process's timers (parkWake)
// because the turn would be cut off by the launch anyway, and a refusal at that turn keeps both
// the process and the prompt — so without the bound armed again (watchAgain) an agent that
// acknowledged a task and started no turn on it would leave that task pending for ever, where
// an unpaced daemon's refusal leaves it charged and waiting (noTurn, promptFailed).
//
// The negative control is the arming reverted (runWake's watchAgain): nothing then bounds the
// acknowledged prompt, so the task keeps its id past the wait and nothing is charged, which
// this test reads at the pending task and the budgets.
func TestALaunchRefusedAtItsTurnBindsTheTurnItsAcknowledgedPromptAwaits(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true
	store := &unansweredCheckLaunchStore{memStore: h.store}
	h.m.deps.Store = store

	// The task is sent and the agent acknowledges it, which is what arms the bound; the sweep
	// then finds the claim's plugin stale and relaunches it, and that launch waits its turn
	// over the process the agent is on.
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Row: 7})
	acked := h.wantPrompts(1)[0].DeliveryID
	if _, awaiting := h.m.timers[TimerTurn]; !awaiting {
		t.Fatal("the acknowledged prompt waits for its turn on nothing, want the bound armed")
	}

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	waiting := h.wantWaitingWake(WakeLaunch)
	if _, awaiting := h.m.timers[TimerTurn]; awaiting {
		t.Fatal("the park left the turn bound of the process the launch replaces armed, want it disarmed")
	}

	store.failing = true
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateReady)
	h.wantStarts(h.token, 1)
	if _, awaiting := h.m.timers[TimerTurn]; !awaiting {
		t.Fatal("the acknowledged prompt the refusal kept waits for its turn on nothing, want the bound armed again")
	}

	// No turn starts within it, so the task goes back to waiting and the prompt is charged.
	h.advance(testRPC)

	h.wantBudgets(Budgets{PromptFailures: 1})
	if p := h.pending(); p.ID == acked || p.DeliveredAt.IsZero() {
		t.Fatalf("the task is %+v, want it back under a new id with its read mark, past the bound of prompt %s", p, acked)
	}
}

// The other side of the same arming: a refusal met where no wake parked anything arms nothing.
// A timer is deleted as it fires (fence), so the boot interval of a process the runtime then
// reports gone is gone by the time the death's relaunch is refused (bootInterval, probe, died,
// relaunchAfterFailure). Arming it again there would probe a dead process at every boot interval
// for as long as the refusal lasts, each probe reaching the same refusal and charging nothing,
// where the claim's registration deadline is the watch that is left: it retires the process and
// relaunches it on the claim's own launch budget once the store answers again.
//
// The negative control is the arming moved to the refusal every launch shares
// (relaunchRefused's first branch): the boot interval is then armed over the dead process and
// fires again, which this test reads at the timer and at the runtime's probes.
func TestARefusalOutsideAWakeArmsNoTimerTheFenceHasFired(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateShimConnected)
	store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.m.deps.Store = store

	// The pod went while the agent was booting, so that is what the runtime answers the probe
	// the boot interval makes, and the relaunch the death raises meets a store that cannot
	// answer its lifecycle check.
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone}, fake.ProbeResult{Kind: runtime.Gone})
	h.advance(testBoot)

	h.wantCalls("Probe", 1)
	h.wantStarts(h.token, 1)
	h.wantBudgets(Budgets{})
	if _, armed := h.m.timers[TimerBoot]; armed {
		t.Error("the boot interval that fired into the refusal is armed again, want it left where the fence dropped it")
	}
	if _, armed := h.m.timers[TimerRegistration]; !armed {
		t.Fatal("the registration deadline is not armed, want the watch the refusal never took from the claim")
	}

	h.advance(testBoot)

	h.wantCalls("Probe", 1)
	h.wantBudgets(Budgets{})
	if _, armed := h.m.timers[TimerBoot]; armed {
		t.Fatal("the boot interval is armed a boot interval later, want nothing of the dead process probing it while the refusal lasts")
	}
}

// The other half of the same refusal: a stale-plugin relaunch the lifecycle recheck refuses at
// its turn marks nothing. PluginRelaunchFor says what contract the last relaunch that ran was
// made for, and a registration that still differs once such a relaunch has run is the second in
// a row, charged toward failed (relaunchStale, staleRegistration). The mark is written where
// the launch starts the process that replaces the stale one (launchNow), and a relaunch refused
// at its turn starts none and has nothing left to run it — the error goes back to whoever asked
// — so the claim is left marked exactly as the relaunch found it, which for this claim is not
// at all, and the old process registering afterwards is relaunched for free, as the first stale
// registration of any claim is.
//
// The negative control is the mark committed at the top of runWake, before its lifecycle
// recheck (`m.holdPluginRelaunch(waiting.pluginMark); m.commitPluginRelaunch()` ahead of
// checkLaunch): the claim then names this daemon's contract for a relaunch that started no
// process, which this test reads at the mark.
func TestAStalePluginRelaunchRefusedAtItsTurnLeavesNoMarkBehind(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true
	store := &unansweredCheckLaunchStore{memStore: h.store}
	h.m.deps.Store = store

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	waiting := h.wantWaitingWake(WakeLaunch)
	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim whose stale-plugin relaunch waits is marked for %d, want 0: its launch has not run", got.PluginRelaunchFor)
	}

	store.failing = true
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateReady)
	h.wantStarts(h.token, 1)
	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim is marked for %d after its relaunch was refused, want 0: that relaunch started no process", got.PluginRelaunchFor)
	}

	// The agent of the process that was never replaced registers again, on the contract it is
	// still running. It is the first stale registration this claim has had relaunched, so it is
	// relaunched and charged nothing, and refused as every stale registration is.
	store.failing = false
	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})

	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(24, 25) {
		t.Fatalf("the re-registration after the refused relaunch = %v, want the stale-plugin refusal naming 24 and 25", err)
	}
	h.wantBudgets(Budgets{})

	// That relaunch is paced like any other, so it waits its turn over the process it replaces
	// and runs there, this time with nothing refusing it.
	again := h.wantWaitingWake(WakeLaunch)
	h.advance(again.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	h.wantBudgets(Budgets{})
	if got := h.claim(); got.PluginRelaunchFor != 25 {
		t.Fatalf("the claim after the relaunch that ran is marked for %d, want 25", got.PluginRelaunchFor)
	}
}

// The mark of a relaunch that did run is not one a later refusal touches. A claim relaunched
// onto this daemon's plugin is marked for its contract (relaunchStale), and a process that
// registers stale after that relaunch has run is the second in a row: it is charged and
// relaunched by the failure path, never relaunched for free again (staleRegistration,
// relaunchAfterFailure). That charged relaunch can itself be refused by a store that cannot
// answer the lifecycle check, and the refusal of it says nothing about the relaunch that ran:
// the mark stands, so the next stale registration is still the second in a row and is charged.
// Clearing it there would hand the claim a free relaunch at every refusal, which is the loop on
// a plugin no launch can move that the mark exists to stop.
//
// The negative control is a clear of the mark added to the refusal every launch shares
// (relaunchRefused's first branch): the third stale registration is then relaunched for free
// and the budgets read no launch failure, which this test reads at the mark and at the budgets.
func TestARefusedRelaunchOnTheChargedPathKeepsTheMarkOfTheRelaunchThatRan(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true
	store := &unansweredCheckLaunchStore{memStore: h.store}
	h.m.deps.Store = store

	// The first stale registration of this claim: relaunched for free, and marked for the
	// contract that relaunch is made for. Its turn replaces the process.
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	waiting := h.wantWaitingWake(WakeLaunch)
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	h.wantBudgets(Budgets{})
	if got := h.claim(); got.PluginRelaunchFor != 25 {
		t.Fatalf("the claim after the relaunch that ran is marked for %d, want 25", got.PluginRelaunchFor)
	}

	// The process that relaunch started speaks the old contract too — the plugin it was put on
	// is itself behind this daemon — and registers while the store cannot answer the lifecycle
	// check, so the charged relaunch it asks for is refused.
	h.connect()
	store.failing = true
	staleRegister := RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24}
	if err := h.handle(staleRegister); err == nil {
		t.Fatal("the registration whose charged relaunch the store could not answer = nil, want the refusal")
	}

	h.wantStarts(h.token, 2)
	h.wantBudgets(Budgets{})
	if got := h.claim(); got.PluginRelaunchFor != 25 {
		t.Errorf("the claim after the refused charged relaunch is marked for %d, want the 25 the relaunch that ran left: the refusal relaunched nothing",
			got.PluginRelaunchFor)
	}

	// The store answers again and the same process registers again, still on the old contract.
	// It is the second stale registration in a row of a relaunch that did run, so it is charged.
	store.failing = false
	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})

	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(24, 25) {
		t.Fatalf("the registration after the store answered again = %v, want the stale-plugin refusal naming 24 and 25", err)
	}
	h.wantBudgets(Budgets{LaunchFailures: 1})
}

// The same mark over a claim that runs no process at all. A claim the charged path walked to
// failed carries the mark of the relaunch that put its process on this daemon's plugin, and the
// workflow's retry relaunches it from there (retry, revive). A retry the lifecycle check refuses
// starts nothing and leaves the claim failed, and the mark of the relaunch that did run is no
// more the refusal's to clear than it is over a claim that records a process: the next stale
// registration after the retry does go through must still be read as the second in a row
// (staleRegistration).
//
// The negative control is a clear of the mark added to the refusal every launch shares
// (relaunchRefused's first branch): the failed claim is then left marked for nothing, which
// this test reads at the mark.
func TestARefusedRetryOfAProcesslessClaimKeepsItsPluginMark(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	failed := wakeClaim(secondToken, StateFailed)
	failed.Failure = "launch failures ran out"
	failed.PluginRelaunchFor = h.deps.PluginContract
	retried := h.machineFor(failed)
	retried.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}

	if err := h.postTo(retried, RequestRetry{Claim: secondToken}); err == nil {
		t.Fatal("the retry whose lifecycle check the store could not answer = nil, want the refusal")
	}

	h.wantStarts(secondToken, 0)
	if got := retried.Claim(); got.PluginRelaunchFor != h.deps.PluginContract {
		t.Fatalf("the failed claim after its refused retry is marked for %d, want the %d its last relaunch left: the retry relaunched nothing",
			got.PluginRelaunchFor, h.deps.PluginContract)
	}
}

// A plugin task's wake ends no held suspension. The task waits over a claim whose agent is in a
// turn (refreshPlugin), and a suspend that arrives in that turn is held so the turn is not cut off
// mid-tool-call (holdSuspension): ending the hold at this wake's turn would stop the process in
// the middle of the very turn the hold exists to protect, and the wake is not a relaunch — it
// opens a round over a session that goes on running. So the hold is left to that turn's own end,
// which is the decision that runs it, and the wake opens nothing: the suspension stops the
// process, and the launch that resumes the claim installs this daemon's plugin by itself.
//
// The negative control is the kind test reverted (wakeNow's held branch): the process is then
// stopped at the wake's turn, which this test reads at the runtime's suspends.
func TestAPluginTaskWakeLeavesAHeldSuspensionToTheTurnItProtects(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	working := stalePluginClaim(wakeClaim(secondToken, StateReady), PodUpdateContract)
	working.State = StateWorking
	working.Pending = &Delivery{ID: "delivery-1", Task: "implement the plan", QueuedAt: epoch, DeliveredAt: epoch, ConfirmedAt: epoch}
	stale := h.machineFor(working)
	if err := h.store.PutDelivery(h.ctx, working.Token, *working.Pending); err != nil {
		t.Fatal(err)
	}

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(stale, StalePlugin{Claim: secondToken, Trigger: "sweep"})
	// The daemon's own suspension, its issue having left the workflow: it waits for the turn
	// however long the turn takes, where an operator's is bounded at the stop timeout from its
	// arrival — a bound shorter than a wave's spacing, which would stop this process itself.
	err := h.postTo(stale, RequestSuspend{Claim: secondToken, Reason: "its issue left the workflow", By: SuspendedByDaemon})
	if !suspensionHeld(err) {
		t.Fatalf("the suspend of a claim in a turn answered %v, want the hold for the turn's end", err)
	}

	h.clock.Advance(wakeInterval + wakePart)
	stale.Wait()

	if got := suspendsOf(h, secondToken); got != 0 {
		t.Fatalf("the runtime was asked to stop the process %d times at the update task's turn, want none: the hold protects the turn still running", got)
	}
	got := stale.Claim()
	if got.State != StateWorking || !got.SuspensionHeld {
		t.Fatalf("the claim at the update task's turn is %s holding %v, want %s with the suspension still held",
			got.State, got.SuspensionHeld, StateWorking)
	}
	if got.UpdateRound != "" {
		t.Fatalf("the claim holds round %q, want none: a round opened here would ask a worker the suspension is about to stop", got.UpdateRound)
	}

	h.mustPostTo(stale, StreamTurnEnd{Claim: secondToken})

	if got := suspendsOf(h, secondToken); got != 1 {
		t.Fatalf("the runtime was asked to stop the process %d times at the turn's end, want once: the hold runs there", got)
	}
	if got := stale.Claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon {
		t.Fatalf("the claim at the turn's end is %s suspended by %q, want %s by %q", got.State, got.SuspendedBy, StateSuspended, SuspendedByDaemon)
	}
}

// suspendsOf is how many times the runtime was asked to stop token's process.
func suspendsOf(h *harness, token claim.Token) int {
	h.t.Helper()
	stopped := 0
	for _, call := range h.rt.CallsOf("Suspend") {
		if call.Locator.Claim == token {
			stopped++
		}
	}
	return stopped
}

// Check c-spread of dispatch://INFRA-976/spec names two wakes that never wait, and one of them is
// the wake an operator asks for through the daemon's API. Its deliver route is one of those asks
// (api.operator's deliver, which sends a task with no row id): a person is waiting on the task
// they just gave a claim, as they are on the resume and the retry the same route sends. So an
// operator's task launches the claim where it arrives and takes nothing from the rate — whether
// the claim is suspended, or parked by a wake of the daemon's own that the request drops
// (dropsWaitingWake) — and the queue behind it keeps every turn it already held.
//
// The negative control is the exemption reverted (deliverResuming's revive and dropsWaitingWake):
// both claims then wait for a turn of the queue, which this test reads at the runtime's starts.
func TestAnOperatorsDeliveryNeverWaitsForATurn(t *testing.T) {
	const task = "look at the production check"
	h := pacedHarness(t, wakeRate)
	suspended := h.machineFor(wakeClaim(secondToken, StateSuspended))
	parked := h.machineFor(wakeClaim(thirdToken, StateSuspended))
	behind := h.machineFor(wakeClaim(waveToken(0), StateQueued))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(parked, RequestRuleResume{Claim: thirdToken, Reason: "it owns its issue's phase"})

	h.mustPostTo(suspended, RequestDeliver{Claim: secondToken, Task: task})
	h.mustPostTo(parked, RequestDeliver{Claim: thirdToken, Task: task})

	for _, token := range []claim.Token{secondToken, thirdToken} {
		h.wantStarts(token, 1)
		h.wantWakeLine(token, "kind=launch", "waited=false", "paced=false")
	}
	for _, m := range []*Machine{suspended, parked} {
		got := m.Claim()
		if got.State != StateLaunching || got.Pending == nil || got.Pending.Task != task {
			t.Fatalf("the claim the operator's task reached is %s holding %+v, want %s holding that task: it launches where the task arrives",
				got.State, got.Pending, StateLaunching)
		}
		if held := m.View().WakeWaiting; held != nil {
			t.Fatalf("%s names wake %+v, want none: an operator's task waits for no turn", got.Token, held)
		}
	}

	// The queue behind them is untouched: the burst let one wake out and the rule's resume took
	// the next turn, so the claim raised after the two deliveries holds the turn after that one.
	h.mustPostTo(behind, RequestSpawn{Claim: waveToken(0)})
	h.clock.Advance(2 * (wakeInterval + wakePart))
	behind.Wait()

	h.wantStarts(waveToken(0), 1)
	h.wantWakeLine(waveToken(0), "kind=launch", "waitedFor="+(2*(wakeInterval+wakePart)).String())
}

// The other park an operator's task can reach: a claim that is ready with a launch of the
// daemon's own parked over the process it still runs — a stale address here, and a stale-plugin
// relaunch or an answered plugin round in the same place. Two things are owed at once. A person
// is waiting on the task, so it may not wait out a paced turn tens of minutes away in a wave,
// which R1 forbids; and the task may not reach the process that launch is about to let go, since
// the launch would cut the turn it had just begun and settle would retire it as served
// (sendPending's launchWaits, retirePending). Both hold when the task runs the parked launch
// where it arrives, unpaced and by that launch's own steps, and is queued for the process the
// launch starts (runParkedLaunchNow): the claim is left where an unpaced daemon would have left
// it, and the task goes at that process's ready.
//
// The budget is spent before the park, so a turn of its own would be a wake away, and the launch
// run here takes no second token: the claim behind keeps the turn the queue already gave it.
//
// The negative control is the launch dropped where it is run (dropWakeFor widened to a claim
// that records a process, with no run in its place): the task is then read by the process the
// launch was to replace, which this test reads at the prompts, and nothing replaces it at all.
func TestAnOperatorsDeliveryOverAParkedLaunchRunsThatLaunchNow(t *testing.T) {
	const task = "look at the production check"
	h := pacedHarness(t, wakeRate)
	behind := h.machineFor(wakeClaim(waveToken(0), StateQueued))
	h.reach(StateReady)
	replaced := h.locator()

	h.observe(runtime.StaleAddress)
	h.wantWaitingWake(WakeLaunch)
	h.mustPostTo(behind, RequestSpawn{Claim: waveToken(0)})
	if got := h.claim(); got.State != StateReady || got.Locator == nil {
		t.Fatalf("the claim whose relaunch waits is %s recording %+v, want %s recording the process it still runs",
			got.State, got.Locator, StateReady)
	}

	// The task arrives part of the way into the wait, as a person's does.
	h.advance(wakePart)
	h.must(RequestDeliver{Claim: h.token, Task: task})

	h.wantPrompts(0)
	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != replaced {
		t.Fatalf("the launch the operator's task ran replaced %+v, want the process the gate left running, %+v",
			resume.Previous, replaced)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim names wake %+v, want none: an operator's task waits for no turn", held)
	}
	if p := h.pending(); p.Task != task || !p.DeliveredAt.IsZero() {
		t.Fatalf("the task is %+v, want %q queued unread: it goes to the process this launch starts", p, task)
	}
	h.wantWakeLine(h.token, "kind=launch", "waited=true", "waitedFor="+wakePart.String(), "paced=false")

	h.relaunched()

	sent := h.wantPrompts(1)
	if sent[0].Message != task {
		t.Fatalf("the prompts are %+v, want the operator's task at the process the launch started", sent)
	}

	// The launch took no second token, so the claim behind keeps the turn the queue gave it.
	h.clock.Advance(2 * (wakeInterval + wakePart))
	behind.Wait()

	h.wantStarts(waveToken(0), 1)
	h.wantWakeLine(waveToken(0), "kind=launch", "waitedFor="+(2*(wakeInterval+wakePart)).String())
}

// The same park over a process that is already dead, met by a task the claim refuses. The claim
// is working with a task the death took back, so the operator's own task is answered "a delivery
// is already pending" (queue): a claim that already holds a delivery is the whole of that
// refusal for a task sent with no id and no outbox row, which is what the operator's deliver
// route sends. Nothing runs for a request the claim answers with nothing, so the parked launch
// is left where it waits (runParkedLaunchNow) and the claim is left where the death alone left
// it — working, recording the dead process, with the relaunch due at the moment the queue gave
// it.
//
// Leaving it parked loses nothing. The wake is what holds that process's whole fate, and while
// it waits every report of the process is answered: the sweep's observation at judge's guard,
// and the four timer and closed-connection paths at probe's, so the one death stays charged
// once. The turn relaunches, and the task the claim is still holding goes to the process that
// launch starts.
//
// The negative control is the refusal test reverted (runParkedLaunchNow's pending delivery): the
// launch then runs for a task the row refuses, replacing a live claim's process unpaced for a
// request that was answered with nothing, which this test reads at the state and the runtime's
// starts.
func TestAnOperatorsDeliveryTheClaimRefusesRunsNoParkedRelaunch(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateWorking)
	dead := h.locator()

	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	took := h.pending()

	err := h.handle(RequestDeliver{Claim: h.token, Task: "look at the production check"})

	if !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("the operator's task over a claim already holding one = %v, want %v", err, ErrDeliveryPending)
	}
	h.wantState(StateWorking)
	h.wantStarts(h.token, 1)
	h.wantWakeDueAt(waiting)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	if got := h.claim(); got.Locator == nil || *got.Locator != dead {
		t.Fatalf("the claim records %+v, want the process it found dead, %+v: a refused task replaces nothing", got.Locator, dead)
	}
	if p := h.pending(); p.ID != took.ID || p.Task != took.Task {
		t.Fatalf("the pending task is %+v, want the one the death took back, %+v: the refused task queued nothing", p, took)
	}

	// The sweep's next pass reports the same death again, and the wake still waiting over that
	// process is what answers it: one death, charged once.
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}})

	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
	h.wantStarts(h.token, 1)
	h.wantWakeDueAt(waiting)

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
}

// The same park over a dead process, reached by a claim that is ready and holds no task: the
// task the operator gives it is queued for the process the relaunch starts rather than sent to
// the connection of the process that died, which would read nothing and confirm nothing. The
// death is charged once here too, and the claim's second launch failure is the one the sweep's
// repeat of that death would have charged.
//
// The negative control is the launch dropped where it is run (dropWakeFor widened to a claim
// that records a process, with no run in its place): the task is then handed to the dead
// process's connection, which this test reads at the prompts, and the sweep's next report of
// that same death charges a second launch failure.
func TestAnOperatorsDeliveryOverAParkedRelaunchNeverReachesTheDeadProcess(t *testing.T) {
	const task = "look at the production check"
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	dead := h.locator()

	h.observe(runtime.Gone)
	h.wantWaitingWake(WakeLaunch)
	h.wantBudgets(Budgets{LaunchFailures: 1})

	h.must(RequestDeliver{Claim: h.token, Task: task})

	h.wantPrompts(0)
	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	if p := h.pending(); p.Task != task || !p.DeliveredAt.IsZero() {
		t.Fatalf("the task is %+v, want %q queued unread: the process it would have reached is dead", p, task)
	}

	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}})

	h.wantBudgets(Budgets{LaunchFailures: 1})

	h.relaunched()

	sent := h.wantPrompts(1)
	if sent[0].Message != task {
		t.Fatalf("the prompts are %+v, want the operator's task at the process the relaunch started", sent)
	}
}

// The park over a process that is still booting, which the launch at its turn would retire: the
// park disarms that process's boot interval and registration deadline (parkWake), so nothing but
// the launch retires it. Running the launch where the operator's task arrives arms the new
// process's own timers (launchNow, armBoot), and the claim goes on being watched: the agent that
// never registers has its deadline, and the deadline retires its process and relaunches.
//
// The negative control is the launch dropped where it is run (dropWakeFor widened to a claim
// that records a process, with no run in its place): the claim then stands shim_connected over
// a booting process with no timer armed for it and nothing charged, past the deadline and four
// boot intervals, which this test reads at the state and the runtime's starts.
func TestAnOperatorsDeliveryOverAParkedLaunchArmsTheNextProcessesTimers(t *testing.T) {
	const task = "look at the production check"
	h := pacedHarness(t, wakeRate)
	h.reach(StateShimConnected)
	booting := h.locator()

	h.observe(runtime.StaleAddress)
	h.wantWaitingWake(WakeLaunch)
	for _, kind := range []TimerKind{TimerBoot, TimerRegistration} {
		if _, armed := h.m.timers[kind]; armed {
			t.Fatalf("the park left the %s timer of the process the launch replaces armed, want it disarmed", kind)
		}
	}

	h.must(RequestDeliver{Claim: h.token, Task: task})

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	h.wantCalls("Spawn", 2)
	if got := h.claim(); got.Locator == nil || *got.Locator == booting {
		t.Fatalf("the claim records %+v, want a process of its own rather than the booting one the launch let go, %+v",
			got.Locator, booting)
	}
	for _, kind := range []TimerKind{TimerBoot, TimerRegistration} {
		if _, armed := h.m.timers[kind]; !armed {
			t.Fatalf("the %s timer of the process the launch started is not armed, want the launch watching it", kind)
		}
	}

	// The agent of that process never registers either: its deadline retires it and relaunches,
	// where a claim left parked over a process nothing watches would stand there for ever.
	h.advance(deadline)

	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantStarts(h.token, 3)
}

// A plugin task's wake is left exactly where it waits. It replaces no process — it opens a round
// over a session that goes on taking work (openPluginUpdateIfStale) — so it holds no send back
// (launchWaits, sendPending) and an operator's task reaches the running process through it
// without touching it. Taking it off the claim would buy nothing and cost a second token: the
// sweep posts the stale plugin again at its next tick and the wake it raised would go to the
// back of the queue (refreshPlugin).
//
// The negative control is the kind test reverted (runParkedLaunchNow's launchWaits): the round's
// own wake is then run by the operator's task, so the round opens where that task arrives and
// its ask takes the claim's one delivery slot, and the task itself is refused "a delivery is
// already pending", which this test reads at the delivery.
func TestAnOperatorsDeliveryLeavesAPluginTaskWakeWhereItWaits(t *testing.T) {
	const task = "look at the production check"
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	conn := fake.NewConn()
	h.conns.Register(secondToken, conn)
	stale := h.machineFor(stalePluginClaim(wakeClaim(secondToken, StateReady), PodUpdateContract))

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(stale, StalePlugin{Claim: secondToken, Trigger: "sweep"})
	waiting := stale.View().WakeWaiting
	if waiting == nil || waiting.Kind != WakePluginTask {
		t.Fatalf("the stale claim names wake %+v, want a plugin task waiting its turn", waiting)
	}

	h.mustPostTo(stale, RequestDeliver{Claim: secondToken, Task: task})

	if held := stale.View().WakeWaiting; held == nil || *held != *waiting {
		t.Fatalf("the claim names wake %+v, want the one it held, %+v: an operator's task leaves a plugin task's wake alone", held, waiting)
	}
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].Message != task {
		t.Fatalf("the prompts are %+v, want the operator's task at the process the claim runs: no launch waits over it", prompts)
	}
	h.wantStarts(secondToken, 0)

	h.clock.Advance(wakeInterval + wakePart)
	stale.Wait()

	if got := stale.Claim(); got.UpdateRound != PluginRound(h.deps.PluginContract) {
		t.Fatalf("the claim holds round %q at its turn, want %q", got.UpdateRound, PluginRound(h.deps.PluginContract))
	}
	h.wantWakeLine(secondToken, "kind=plugin-task", "waited=true", "paced=true")
	h.wantStarts(secondToken, 0)
}

// A claim holding a suspension keeps both the hold and the wake. The turn of such a claim ends
// the suspension rather than launching (wakeNow, endHeld), which is not what a task asks for,
// and the suspension is what decides the process either way: it stops it, and the claim's next
// resume launches onto a process the task goes to. So the task is taken where it arrives, queued
// and held exactly as a held claim's task is (sendPending), and the launch keeps its turn.
//
// The negative control is the hold test reverted (runParkedLaunchNow's held): the launch is then
// run inside the hold, which relaunches the claim and drops with the process the suspension the
// operator is waiting on (letGo), where this test reads the claim still working.
func TestAnOperatorsDeliveryOverAHeldSuspensionLeavesTheLaunchParked(t *testing.T) {
	const task = "look at the production check"
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	// A turn of the agent's own, on no task of the claim's: the claim is working with its
	// delivery slot free, which is the one working claim an operator's task is queued on.
	h.must(StreamTurnStart{Claim: h.token})

	h.observe(runtime.StaleAddress)
	waiting := h.wantWaitingWake(WakeLaunch)
	if err := h.handle(RequestSuspend{Claim: h.token, Reason: "its issue left the workflow", By: SuspendedByDaemon}); !suspensionHeld(err) {
		t.Fatalf("the suspend of a claim in a turn answered %v, want the hold for the turn's end", err)
	}

	h.must(RequestDeliver{Claim: h.token, Task: task})

	h.wantState(StateWorking)
	h.wantStarts(h.token, 1)
	h.wantWakeDueAt(waiting)
	if got := h.claim(); !got.SuspensionHeld {
		t.Fatal("the claim holds no suspension, want the one it was handed: an operator's task does not end it")
	}
	if p := h.pending(); p.Task != task || !p.DeliveredAt.IsZero() {
		t.Fatalf("the task is %+v, want %q queued unread: a claim holding a suspension is handed nothing", p, task)
	}
}

// A request the table refuses or ignores runs no launch. The claim here is parked over a process
// it records and is then put in the one live state whose deliver row is an ignore, which no
// decision of this package reaches with a process recorded (the table's launch_uncertain row);
// it is set here because the rule is the table's answer rather than the state, and a row that
// becomes an ignore later must not start a launch either. The launch keeps its turn, and the
// operator is answered the ignore's own refusal.
//
// The negative control is the row test reverted (runParkedLaunchNow's table lookup): the launch
// then runs for a request the table ignores, and the row of the state that launch leaves queues
// the task, so the request is answered with no refusal at all, which this test reads at the
// answer.
func TestADeliveryTheTableIgnoresRunsNoParkedLaunch(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)

	h.observe(runtime.StaleAddress)
	waiting := h.wantWaitingWake(WakeLaunch)
	h.m.claim.State = StateLaunchUncertain

	err := h.handle(RequestDeliver{Claim: h.token, Task: "look at the production check"})

	var refusal *RefusedError
	if !errors.As(err, &refusal) || refusal.State != StateLaunchUncertain {
		t.Fatalf("the delivery the table ignores = %v, want the refusal of %s", err, StateLaunchUncertain)
	}
	h.wantStarts(h.token, 1)
	h.wantWakeDueAt(waiting)
}

// A registration of the process a stale-plugin relaunch waits over raises nothing and is
// charged nothing. The mark a relaunch leaves (PluginRelaunchFor) names the contract it was
// made for, written where its launch starts the replacement (launchNow), and a registration
// that still differs once such a relaunch has run is the second in a row and is charged toward
// failed (staleRegistration). The pacing puts a wait between the decision and the launch, and a
// process that reconnects inside that wait registers again on the contract it is still running:
// the claim carries no mark yet, so what the guard stops is a second relaunch over the one
// already waiting, which would replace its wake and take a second token for the same launch.
// The registration is refused all the same, as every stale one is, and the turn replaces the
// process and marks the claim.
//
// The negative control is the guard reverted (staleRegistration's launchWaits): the
// re-registration then raises a relaunch of its own, which replaces the wake this claim holds
// with one due a whole interval later and takes a second token for the launch that wake was
// already going to make, which this test reads at the wake the claim still holds.
func TestAReRegistrationInsideAStalePluginRelaunchsWaitIsChargedNothing(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	waiting := h.wantWaitingWake(WakeLaunch)
	if got := h.claim(); got.State != StateReady || got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim whose stale-plugin relaunch waits is %s marked for %d, want %s marked for 0: its launch has not run",
			got.State, got.PluginRelaunchFor, StateReady)
	}

	// The shim reconnects inside the wait and the agent registers again, on the plugin contract
	// the process it is still running speaks.
	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})

	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(24, 25) {
		t.Fatalf("the re-registration inside the wait = %v, want the stale-plugin refusal naming 24 and 25", err)
	}
	h.wantBudgets(Budgets{})
	h.wantStarts(h.token, 1)
	h.wantWakeDueAt(waiting)

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	h.wantBudgets(Budgets{})
	if got := h.claim(); got.PluginRelaunchFor != 25 {
		t.Fatalf("the claim after the turn that replaced its process is marked for %d, want 25", got.PluginRelaunchFor)
	}
}

// The mark itself: a stale-plugin relaunch that runs says so, and what that saying buys is the
// charge on the registration after it. PluginRelaunchFor is written where the launch starts the
// process that replaces the stale one (launchNow, commitPluginRelaunch), with that launch's own
// write, so a claim whose relaunch is still waiting its turn carries no mark and the claim whose
// turn has come carries one the store holds. The process that relaunch starts is the one the
// mark is for: a registration of it that still speaks the old contract means the plugin the
// relaunch put it on is itself behind this daemon, which is the second stale registration in a
// row — charged toward failed rather than relaunched for free, which is the loop the mark exists
// to stop (staleRegistration, relaunchAfterFailure).
//
// The negative control is the commit removed (launchNow's commitPluginRelaunch): the claim is
// marked for nothing after the relaunch that ran, and the registration of the process that
// relaunch started is relaunched free, which this test reads at the mark, at the stored claim
// and at the budgets.
func TestAStalePluginRelaunchThatRunsMarksTheClaim(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	waiting := h.wantWaitingWake(WakeLaunch)
	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim whose stale-plugin relaunch waits is marked for %d, want 0: its launch has not run", got.PluginRelaunchFor)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	if got := h.claim(); got.PluginRelaunchFor != 25 {
		t.Fatalf("the claim after the relaunch that replaced its process is marked for %d, want 25", got.PluginRelaunchFor)
	}
	if stored := h.store.load(h.token); stored.PluginRelaunchFor != 25 {
		t.Fatalf("the store holds the claim marked for %d, want 25: the launch's own write carries the mark",
			stored.PluginRelaunchFor)
	}

	// The process that relaunch started speaks the old contract too — the plugin it was put on
	// is itself behind this daemon — so its registration is the second stale one in a row.
	h.connect()
	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})

	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(24, 25) {
		t.Fatalf("the registration of the relaunched process = %v, want the stale-plugin refusal naming 24 and 25", err)
	}
	h.wantBudgets(Budgets{LaunchFailures: 1})
}

// Two stale-plugin relaunches raised over one claim, the second replacing the wake the first
// parked, leave the mark where they found it when neither runs. The daemon's sweep posts the end
// of an answered plugin round at every tick for a claim that is not working, and the stale lane
// relaunches that same claim from a registration of its old process, so the two meet: the
// registration's relaunch parks a wake, and the round's end raises a relaunch of its own over it
// (relaunchStale, endPodUpdate, reserveWake). Neither has marked the claim — the mark is written
// where a launch starts a process (launchNow) — so a refusal at the second wake's turn leaves
// the claim exactly as the first relaunch found it, and the next stale registration is the first
// this claim has had relaunched: relaunched for free rather than walked toward failed.
//
// The negative control is the mark written where the relaunch is decided
// (markPluginRelaunch setting `m.claim.PluginRelaunchFor = m.deps.PluginContract`): the second
// relaunch then marks the claim for this daemon's contract and nothing takes it back, which this
// test reads at the mark.
func TestASecondStalePluginRelaunchOverAParkedOneLeavesNoMarkBehind(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true
	h.m.deps.PodUpdateRetask = testPodUpdateRetask
	store := &unansweredCheckLaunchStore{memStore: h.store}
	h.m.deps.Store = store

	// The claim's own plugin round, answered by its worker: from here the daemon's sweep posts
	// its end at every tick until the end decides something (reconcilePluginUpdate).
	round := PluginRound(25)
	h.openUpdate(round, testReasons)
	h.signalUpdate("c0ffee")

	// The old process re-registers on the old contract inside that round. No launch waits yet,
	// so the stale lane relaunches it, and that launch waits its turn.
	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})

	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(24, 25) {
		t.Fatalf("the stale re-registration = %v, want the stale-plugin refusal naming 24 and 25", err)
	}
	first := h.wantWaitingWake(WakeLaunch)

	// The sweep's next end post for the answered round raises a relaunch of its own, which
	// replaces that wake.
	h.must(PodUpdateEnded{Claim: h.token, Round: round})

	second := h.wantWaitingWake(WakeLaunch)
	if !second.At.After(first.At) {
		t.Fatalf("the round's end left the claim holding the wake due at %s, want the one its own relaunch took after %s",
			second.At, first.At)
	}

	// The second relaunch is refused at its turn, as the first would have been: neither ran.
	store.failing = true
	h.advance(second.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 1)
	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim is marked for %d after both relaunches were abandoned, want 0: neither started a process",
			got.PluginRelaunchFor)
	}

	// The process neither relaunch replaced registers again, still on the old contract: the
	// first stale registration this claim has had relaunched, so it is relaunched for free.
	store.failing = false
	err = h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})

	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(24, 25) {
		t.Fatalf("the re-registration after both abandoned relaunches = %v, want the stale-plugin refusal naming 24 and 25", err)
	}
	h.wantBudgets(Budgets{})
}

// A stale-plugin relaunch a held suspension ends in a suspension marks nothing, whether or not
// the write of that suspension lands. The relaunch replaces no process there: replaceProcess
// stops it and suspends the claim instead (endHeld, suspendedFor), and the resume that follows
// is a launch of its own, raised from the top where the operator or the rule asks for it
// (revive). So the claim comes back from the suspension unmarked, and a resumed process that
// still speaks another contract is relaunched once more for free before the mark that charges it
// is there — one relaunch, since that one marks the claim where it starts its process.
//
// The suspension's own write is the case this test drives failing: the claim is suspended in
// memory whether or not it lands (suspended's letGo and persist), so the mark must read the same
// either way, and here it does — none was written to put back or to leave standing.
//
// The negative control is the mark written where the relaunch is decided
// (markPluginRelaunch setting `m.claim.PluginRelaunchFor = m.deps.PluginContract`): the
// suspended claim then carries the mark of a relaunch that replaced nothing, which this test
// reads at the mark.
func TestAStalePluginRelaunchAHeldSuspensionEndsMarksNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true

	// The operator's suspend arrives in a turn and is held for that turn's end, and the stop the
	// end runs is refused by the runtime, so the hold stands over a claim that still records its
	// process (holdSuspension, suspendHeld).
	if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !suspensionHeld(err) {
		t.Fatalf("the suspend of a claim in a turn = %v, want the hold", err)
	}
	h.rt.FailSuspend(errBoom)
	if err := h.handleQuiet(StreamTurnEnd{Claim: h.token}); err == nil {
		t.Fatal("the turn's end ran the held suspension; this test needs a stop the runtime refuses")
	}
	if h.m.held == nil {
		t.Fatal("the suspension is no longer held; this test needs one held over a live process")
	}

	// The sweep finds the claim's plugin stale. The relaunch ends the held suspension rather
	// than launching, and the write of that suspension fails.
	h.store.fail("PutClaim", errBoom)
	if err := h.handle(StalePlugin{Claim: h.token, Trigger: "sweep"}); !errors.Is(err, errBoom) {
		t.Fatalf("the stale-plugin relaunch whose suspension could not be written = %v, want the store's refusal", err)
	}
	h.store.fail("PutClaim", nil)

	got := h.claim()
	if got.State != StateSuspended || got.Locator != nil {
		t.Fatalf("the claim is %s recording %+v, want suspended with its process let go", got.State, got.Locator)
	}
	if got.PluginRelaunchFor != 0 {
		t.Fatalf("the suspended claim is marked for %d, want 0: its relaunch replaced no process", got.PluginRelaunchFor)
	}
	h.wantCalls("Resume", 0)
}

// A registration that clears the mark while a relaunch waits does not unsay the relaunch that
// then runs. The mark is cleared by a registration that records this daemon's own contract
// (register, reregister), and the launch the pacing holds goes on to replace that process at its
// turn: it did relaunch, so it marks the claim where it starts, and the next registration that
// still differs is read against it (staleRegistration). Nothing of the mark the relaunch found
// is put back over the claim either — there is nothing to put back, because the decision wrote
// nothing.
//
// The negative control is the commit removed (launchNow's commitPluginRelaunch): the claim ends
// marked for nothing after a relaunch that did replace its process, which this test reads at the
// mark and at the stored claim.
func TestARegistrationInsideTheWaitDoesNotUnsayTheRelaunchThatRuns(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = 25
	h.m.deps.PluginRelaunchable = true
	// The mark an earlier relaunch of this claim left, for the registration below to clear.
	h.m.claim.PluginRelaunchFor = 25

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	waiting := h.wantWaitingWake(WakeLaunch)

	// The shim reconnects inside the wait and the agent registers on this daemon's own contract,
	// which clears the mark.
	h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 25})
	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim after a current registration is marked for %d, want 0", got.PluginRelaunchFor)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	if got := h.claim(); got.PluginRelaunchFor != 25 {
		t.Fatalf("the claim after the turn that replaced its process is marked for %d, want 25: that relaunch ran",
			got.PluginRelaunchFor)
	}
	if stored := h.store.load(h.token); stored.PluginRelaunchFor != 25 {
		t.Fatalf("the store holds the claim marked for %d, want 25", stored.PluginRelaunchFor)
	}
}

// suspendHeld is whether err is the answer a suspension held for later gives its caller.
func suspensionHeld(err error) bool { return errors.Is(err, ErrSuspendHeld) }
