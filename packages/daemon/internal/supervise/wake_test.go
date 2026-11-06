package supervise

// The wake pacing as a consumer sees it: a daemon that must wake many claims at once lets them out
// at the rate its operator set, one claim's wait never costs another its turn, and the requests a
// person is waiting on never wait at all. These tests drive the real limiter (internal/wakepace)
// through supervise.Deps.WakePace, which is how the daemon wires it, on the package's own
// advanceable clock: nothing here sleeps.

import (
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

// Check c-nonblocking of dispatch://INFRA-976/spec, over the case round 1 left: a claim the gate
// parked over a process that still runs. Such a claim is handed no task while its launch waits,
// because that launch lets the process go at its turn (launchNow): a worker started on a task
// moments before its process is killed loses both the turn and the work, and the task it had
// begun is retired as served at a turn nothing finished (settle, retirePending). The task waits
// instead, exactly as a launching claim's does, and goes to the process the launch starts.
//
// The path here is this pull request's own rollout, which is where the window is widest: a bump
// of the plugin contract makes every live claim stale, each takes the plugin-task lane, and the
// end of the round its worker answered relaunches the process through the paced gate
// (endPodUpdate, replaceProcess). The round's end clears the hold that kept the claim's next
// task back (answeredOpenRound), so until this guard the next task went to the worker the
// relaunch was about to kill. repoint's stale address and relaunchStale open the same window.
// The task is the workflow's own, named by the outbox row it comes from: an operator's task, which
// names no row, drops the launch the pacing holds and goes to the process the claim is running
// instead (dropWakeFor, TestAnOperatorsDeliveryOverAParkedLaunchReachesTheRunningProcess).
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
// table.go), which probes the process and is told the same death. The guard the sweep's report
// met is in the judgement itself (judge), so this path meets it too: the death is charged once,
// and the wake keeps the turn the queue gave it rather than going to the back with a fresh token.
//
// The negative control is the guard reverted (judge's launchWaits): the budgets then read two
// deaths and two launch failures and the wake is due a turn later, both of which this test reads.
func TestAClosedConnectionAfterTheSweepsDeathChargesNothingMore(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	behind := h.machineFor(wakeClaim(secondToken, StateQueued))
	h.reach(StateWorking)
	dead := h.locator()

	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	h.mustPostTo(behind, RequestSpawn{Claim: secondToken})

	// The shim's connection closes behind the process the sweep found gone, and the runtime says
	// of it what the sweep said.
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
	h.must(StreamClosed{Claim: h.token})

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
// keeps that dead process recorded until the launch's turn (parkWake). Its boot interval must not
// fire into the wait: the probe it makes would report the one death again, where before this park
// the death went straight to a launch and letGo disarmed the timer with the process it watched.
// So the park disarms it, and the turn's launch arms the new process's own (armBoot).
//
// The negative control is the disarm reverted (parkWake's timers): the boot interval then probes
// the process inside the wait, which this test reads at the runtime's probes. With the other half
// of the same fix reverted as well (judge's launchWaits) that probe's answer is charged too, and
// the budgets read a second launch failure this claim never had.
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

	// The process really is gone, so that is what the runtime answers anything that probes it.
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
	h.advance(wakeInterval + wakePart/2)

	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Probe", 0)
	h.wantWakeDueAt(waiting)

	h.advance(wakePart)

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
}

// The registration deadline is the other timer the dead process leaves armed: an agent that said
// hello and registered has that long to say it is ready, and a claim whose process dies in the
// middle of it is parked over the dead process with the deadline still to run. Fired inside the
// wait it would probe the process, read the death a second time and retire a process already
// gone, so the park disarms it with the boot interval; the launch at the turn arms the new
// process's own (armRegistration).
//
// The negative control is the disarm reverted (parkWake's timers): the deadline then probes the
// process inside the wait and suspends it, which this test reads at the runtime's probes. With
// the other half reverted as well (judge's launchWaits), what it reads of the process is charged
// to the claim a second time.
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

	// The process really is gone, so that is what the runtime answers anything that probes it.
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
	h.advance(wakeInterval + wakePart/2)

	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Probe", 0)
	h.wantWakeDueAt(waiting)
	if got := h.claim(); got.State == StateRetired {
		t.Fatal("the claim was retired by a deadline inside the wait, want the launch's turn to decide it")
	}

	h.advance(wakePart)

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
}

// The probe interval is the one timer of the four that can be armed inside the wait: a closed
// connection whose probe the runtime could not answer arms it (probe), and when it fires the
// runtime answers with the death the sweep already charged. That report reaches the same
// judgement, and decides nothing there.
//
// The wait here is a queue of four turns, which is what a wave looks like from one claim: the
// probe interval falls inside it, where at the head of an empty queue the turn would come first.
//
// The negative control is the guard reverted (judge's launchWaits): the probe interval then
// charges a second death and a second launch failure, which this test reads at the budgets.
func TestTheProbeIntervalInsideTheWaitChargesNothing(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateWorking)
	queueAhead(h, 3)
	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)

	// The connection closes and the runtime cannot say what became of the pod, so the probe is
	// armed for its interval; at that interval the runtime answers with the same death.
	h.rt.ScriptProbe(fake.ProbeResult{Err: errors.New("the node the pod ran on is unreachable")})
	h.must(StreamClosed{Claim: h.token})
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

	h.advance(testProbe)

	h.wantCalls("Probe", 2)
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

// A plugin task's wake ends no held suspension. The task waits over a claim whose agent is in a
// turn (refreshPlugin), and a suspend that arrives in that turn is held so the turn is not cut off
// mid-tool-call (holdSuspension): ending the hold at this wake's turn would stop the process in
// the middle of the very turn the hold exists to protect, and the wake is not a relaunch — it
// opens a round over a session that goes on running. So the hold is left to the turn's own end,
// which is where it ran before this pull request paced the task, and the wake opens nothing: the
// suspension stops the process, and the launch that resumes the claim installs this daemon's
// plugin by itself.
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

// The other park an operator's task can reach, and the one round 2 found waiting: a claim that is
// ready with a launch of the daemon's own parked over the process it still runs — a stale address
// here, and a stale-plugin relaunch or an answered plugin round in the same place. The claim is
// not handed a task while that launch waits, because the launch lets the process go at its turn
// (sendPending's launchWaits), so an operator's own task would have waited out the paced turn,
// tens of minutes away in a wave, which R1 forbids. It drops the launch instead and is decided
// where it arrives: the task goes to the process the claim is running, which is right, because
// nothing is replacing that process any more. The launch the drop abandons is raised again by the
// lane that raised it (dropWakeFor).
//
// The budget is spent before the park, so a turn of its own would be a wake away.
//
// The negative control is the widening reverted (dropWakeFor's live-process case): the task is
// then queued unread behind the paced turn, which this test reads at the prompts.
func TestAnOperatorsDeliveryOverAParkedLaunchReachesTheRunningProcess(t *testing.T) {
	const task = "look at the production check"
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	running := h.locator()

	h.observe(runtime.StaleAddress)
	h.wantWaitingWake(WakeLaunch)
	if got := h.claim(); got.State != StateReady || got.Locator == nil {
		t.Fatalf("the claim whose relaunch waits is %s recording %+v, want %s recording the process it still runs",
			got.State, got.Locator, StateReady)
	}

	h.must(RequestDeliver{Claim: h.token, Task: task})

	sent := h.wantPrompts(1)
	if sent[0].Message != task {
		t.Fatalf("the prompts are %+v, want the operator's task at the process the claim runs", sent)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim names wake %+v, want none: an operator's task waits for no turn", held)
	}
	if got := h.claim(); got.Locator == nil || *got.Locator != running {
		t.Fatalf("the claim the operator's task reached records %+v, want the process it was already running, %+v", got.Locator, running)
	}
	if p := h.pending(); p.Task != task || p.DeliveredAt.IsZero() {
		t.Fatalf("the task is %+v, want %q read by the running process", p, task)
	}

	// Nothing launches at the turn the dropped wake held, and the queue behind it keeps its own.
	h.advance(wakeInterval + wakePart)

	h.wantStarts(h.token, 1)
	h.wantState(StateReady)
}

// suspendHeld is whether err is the answer a suspension held for later gives its caller.
func suspensionHeld(err error) bool { return errors.Is(err, ErrSuspendHeld) }
