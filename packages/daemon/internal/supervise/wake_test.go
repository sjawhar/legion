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
	limiter := wakepace.New(rate, 1, func(time.Duration) time.Duration { return wakePart })
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
		kind  string
		turns int
	}{
		{launching, wakeLaunch, 1},
		{stale, wakePluginTask, 2},
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

// suspendHeld is whether err is the answer a suspension held for later gives its caller.
func suspensionHeld(err error) bool { return errors.Is(err, ErrSuspendHeld) }
