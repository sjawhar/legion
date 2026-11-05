package supervise

// The wake unit: the pacing that holds back the wakes this package causes, so that a restart or a
// plugin wave starts a project's claims at the rate its operator set rather than all at once
// (dispatch://INFRA-976). One wake is reserved at the gate (reserveWake), parked on the machine
// until its turn (parkWake) and run when that turn comes (wakeNow); a request a person is waiting
// on is let through unpaced (exemptWake), and one that reaches a claim a wake parked takes that
// wake off the machine first and gives it back if it launched nothing (dropWakeFor, reparkWake).
// Every wake writes one line as it runs, in a shape the daemon's own catch-up lane shares
// (wakeRan, WakeRanMessage, WakeRanFields).
//
// The unit reaches the rest of the machine through four calls and nothing else: arm, for the
// TimerWake the turn fires on (table.go); persist, for the claim a park writes launching; letGo,
// which drops the wake a stop or a retirement ends; and publish, which is how a reader of every
// claim is told what this claim waits for (WakeWaiting, WaitingWake, Machine.WaitingWake). The
// rest of the package reads the parked wake in three places of its own: launchWaits (machine.go),
// the suspend of a claim parked over no process (holdSuspendUntilReady, suspend.go), and the
// claim already holding a wake, which takes no second one (refreshPlugin, plugin.go). Its tests
// are wake_test.go.

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// WakeKind is which of the daemon's paced lanes a wake is in, and the whole of what the line that
// wake writes says it was (wakeRan, WakeRanFields). The three below are every lane there is, and
// all three reserve from the one limiter the daemon builds (internal/wakepace, openSupervision in
// internal/daemon), because what an Anthropic organization's per-minute uncached-input cap counts
// is the context a woken session reads, not whether the daemon started a process for it.
//
// They are one named type rather than loose strings so that a misspelt kind does not compile and
// a fourth lane is found by the compiler in every place a kind travels — the machine's own wake,
// the view a reader of every claim takes (WakeWaiting, WaitingWake) and the daemon's state
// (api.WakeWaiting) — rather than by grep. The wire stays a plain string, and the operator API's
// schema keeps it unconstrained (contracts' legionWakeWaiting), so a plugin reads a kind a later
// daemon adds.
type WakeKind string

const (
	// WakeLaunch is a role process this package starts: the launch a spawn, a resume, a retry or
	// a relaunch raises, which lets the claim's previous process go at its turn (launchNow,
	// wakeNow).
	WakeLaunch WakeKind = "launch"
	// WakePluginTask is the update task the stale-plugin lane queues to a live session in place
	// of a launch (refreshPlugin, openPluginUpdateIfStale). It starts no process and replaces
	// none: the session it wakes goes on taking work, and the kind is named apart only in the
	// line the wake writes.
	WakePluginTask WakeKind = "plugin-task"
	// WakeCatchUp is the notice a start row writes to an architect already running in place of a
	// launch, which waits on its own durable row rather than in a machine and is run by the
	// daemon's outbox (internal/daemon's wakeRan, the workflow's claimReady). No machine reserves
	// one; it is declared beside the other two so that the daemon's lanes are named in one place.
	WakeCatchUp WakeKind = "catch-up"
)

// WakePace is the pace the daemon lets the wakes it causes out at — in this package, every role
// process a launch starts (reserveWake). A restart or a plugin wave wakes every claim at once,
// and the sessions those wakes start read their context in the minute that follows, which is what
// pushes an Anthropic organization over its per-minute uncached-input cap. The three parts travel
// together because they are read together: the reservation decides when a launch runs, and the
// rate and the burst are what the line that launch writes says it was held to (wakeRan).
type WakePace struct {
	// Reserve is the moment this wake may run, given the caller's own now, and never a moment
	// before it. A wake is charged on the reserve rather than on the release, so one reserved and
	// then dropped has still spent a wake's worth of the rate, and the wakes behind it keep the
	// turns they hold. nil is the pacing off — every launch runs as it is raised — which is this
	// package's convention for a collaborator a deployment does not have, as Secrets and
	// PhaseHolds are: the daemon builds one internal/wakepace limiter for all its lanes and hands
	// its Reserve here, and this package reads no configuration and knows no limiter
	// (openSupervision in internal/daemon).
	Reserve func(now time.Time) time.Time
	// Rate and Burst are the pace the operator set (worker_wakes_per_minute, worker_wake_burst),
	// carried for the line a wake writes as it runs and read for nothing else: the reservation
	// above is the whole of the decision.
	Rate  int
	Burst int
}

// WakeWaiting is a machine's waiting wake as its view publishes it: what kind of wake it is
// (WakeLaunch, WakePluginTask) and the moment the pacing gave it, which is when it runs (wakeNow).
// It is the memory-only `wake` the machine holds, cut down to what a reader outside this package
// is owed: never the launch itself, and never the state the gate found, which only an operator's
// request that does not wait reads (dropWakeFor).
type WakeWaiting struct {
	Kind WakeKind
	At   time.Time
}

// WaitingWake is one claim's waiting wake as the reader of every claim takes it
// (Machine.WaitingWake): the claim the wake will wake, that claim's issue and role, and the kind
// and the moment the view names (WakeWaiting). It is every field the daemon's state names a wake
// by (api.WakesWaitingOf), so that read copies these five and nothing else of the claim.
type WaitingWake struct {
	Claim claim.Token
	Issue string
	Role  claim.Role
	Kind  WakeKind
	At    time.Time
}

// wake is a launch the pacing holds until its turn: the moment it may run (at), the moment it was
// reserved, which the line it writes reports as how long it waited, what kind of wake it is for
// that line, the state the gate found the claim in — which a request that never waits restores
// before it acts (dropWakeFor) — the launch itself, and, for a launch, the same launch raised
// anew.
type wake struct {
	at       time.Time
	reserved time.Time
	kind     WakeKind
	state    ClaimState
	run      func(context.Context) error
	// again is the work this wake holds raised again from the top (launchAfter), for the turn
	// whose own lifecycle recheck the store could not answer: that retry waits the probe interval
	// (relaunchLater, relaunchAgain) and must recheck the lifecycle when it comes, as every launch
	// of this package does, so it re-enters the gate rather than the launch — and pays a token
	// again, which reserveWake states. It is set for a launch wake alone: a plugin task's turn
	// makes no lifecycle check, since it starts no process (openPluginUpdateIfStale).
	again func(context.Context) error
}

// reserveWake is the wake gate: it asks the pacing when this wake may run and, when that moment
// is later than now, parks the wake on the machine until its turn (parkWake, wakeNow) and returns
// with the caller's work left to that turn. A wake that need not wait runs that work here, having
// written the line it writes as it goes (wakeRan).
//
// A claim that records no process when the gate holds it — a spawn from queued, a rule resume
// from suspended, a retry from failed or retired, a process already retired by the decision that
// launches — is written launching and persisted first, exactly as relaunchLater writes one. That
// is not cosmetic: the outbox's start row delivers its task after the launch call, and a task
// delivered to a claim left failed is a named ignore, while one delivered to a claim left
// suspended launches it a second time (deliverResuming). At launching the task is queued
// (deliverLater) and goes at the next ready. A claim that still records its process is left
// exactly as it is, so a restart never finds it launching with a live process; the launch lets
// that process go when its turn comes (launchNow).
//
// Exempt is the request saying so (exemptWake) or the claim being the project controller's: a
// death, a stale address or a displaced pod reaches a launch through displaced and repoint with
// no request at all, and the controller is the one session the operator's own read of a running
// wave relies on. An exempt wake is charged nothing and writes its line as it goes.
//
// A second launch raised over a claim whose wake still waits replaces that wake and takes a token
// of its own; the one the first took is spent either way, since the limiter charges on the
// reserve. No paced request reaches here twice over a parked claim: the table answers a second
// start of a claim at launching without launching anything, and a claim already holding a wake
// takes no second one from the stale-plugin lane (refreshPlugin).
//
// again is the launch raised anew, for the turn to retry with when its own lifecycle recheck is
// unanswered; nil for a kind whose turn makes no such check (wake.again).
func (m *Machine) reserveWake(ctx context.Context, kind WakeKind, exempt bool, run, again func(context.Context) error) error {
	exempt = exempt || claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree)
	reserve := m.deps.WakePace.Reserve
	if exempt || reserve == nil {
		// The operator counts the paced wakes of a minute against the rate and the burst this line
		// names (R6), so a wake no pacing could have held — the pacing off, or this wake let
		// through — says so rather than swelling that count.
		m.wakeRan(kind, false, 0)
		return run(ctx)
	}
	// Past the return above there is a limiter and this wake is not one it lets through, so the
	// pacing could have held it: the daemon hands a Reserve only while the operator's rate is
	// above 0 (the daemon's run), which is why nothing here reads WakePace.Rate, whose doc says
	// it is read for the line alone.
	now := m.deps.Clock.Now()
	at := reserve(now)
	if !at.After(now) {
		m.wakeRan(kind, true, 0)
		return run(ctx)
	}
	return m.parkWake(ctx, &wake{at: at, reserved: now, kind: kind, state: m.claim.State, run: run, again: again}, at.Sub(now))
}

// parkWake is the park the gate and the re-park share (reserveWake, reparkWake): waiting waits on
// the machine, TimerWake is armed for after — what is left of its wait — and a claim that records
// no process is written launching and persisted, which is what reserveWake states. A claim that
// still records its process is left exactly as it stands.
func (m *Machine) parkWake(ctx context.Context, waiting *wake, after time.Duration) error {
	m.wake = waiting
	m.arm(TimerWake, after, "")
	if m.claim.Locator != nil {
		return nil
	}
	m.claim.State = StateLaunching
	return m.persist(ctx)
}

// wakeNow is the wake's turn (TimerWake): the launch the gate held runs now, and takes no second
// token — its reservation was taken once, before it waited. A suspension held meanwhile ends here
// instead of in a launch, exactly as relaunchAgain ends one, and the process the claim records is
// stopped first, as the relaunch this wake holds would have stopped it (replaceProcess, endHeld).
//
// Every hold a launch's turn finds here is over a process that still runs: the suspend of a claim
// in a turn (holdSuspension) and the suspend of a launch that did start a process
// (holdSuspendUntilReady), which are the only two the machine keeps, and letGo drops a hold with
// the process it is held for. A suspend of a parked claim that records no process is held no
// more — it is decided where it arrives, since there is no process to protect and the wake's turn
// is tens of minutes away over a wave (holdSuspendUntilReady) — and that suspension takes the wake
// with it, so no turn of such a claim is reached at all.
//
// A plugin task's turn ends no hold, because that wake replaces no process: it opens a round over
// a session that goes on running (openPluginUpdateIfStale), while the hold it would end exists to
// keep that session's turn whole until the turn's own end runs it (holdSuspension). Ending it
// here would stop the process in the middle of the very turn the hold protects — the turn that
// held suspension is waiting for — so the hold is left to that end and this wake opens nothing.
// Nothing is lost: the suspension stops the process, and the launch that resumes the claim
// installs this daemon's plugin by itself, which is what the round would have asked for.
//
// Two things a launch's turn does before it launches, which together close the window over a
// process the gate left running (reserveWake):
//
//   - The tree lifecycle is rechecked (checkLaunch), because the turn can be tens of minutes after
//     the check launchAfter made: an operator's close reserves its tree's cleanup before it stops
//     the tree's claims, so a wake that fires between the two would launch into a tree whose
//     cleanup is reserved. The refusal goes where launchAfter's own goes (relaunchRefused), and
//     the retry it may leave waiting re-enters the gate (wake.again).
//   - A turn the process it replaces may have started is taken back first (interrupted), exactly
//     as replaceProcess takes one back before it relaunches. The gate let this wake park over a
//     live process, and that process can have confirmed a task since: letting it go without the
//     take-back would leave a confirmed task in a launching claim, which settle retires as served
//     though no turn of it ever finished (retirePending). The other half of that race is the
//     delivery sent during the wait, which sendPending no longer makes (delivery.go).
func wakeNow(m *Machine, ctx context.Context, _ Event) error {
	waiting := m.wake
	m.wake = nil
	if m.held != nil {
		if waiting.kind != WakeLaunch {
			m.log.Info("supervise: this claim's update task came due while a suspension waits for its turn to end; the hold keeps that turn and no round is opened",
				"kind", waiting.kind, "reason", m.held.Reason, "by", m.held.By)
			return nil
		}
		return m.endHeld(ctx)
	}
	if waiting.kind == WakeLaunch {
		if err := m.checkLaunch(ctx); err != nil {
			return m.relaunchRefused(ctx, waiting.again, err)
		}
		if m.claim.Locator != nil {
			if err := m.interrupted(ctx, "the process this launch replaces is let go now that its turn has come", ordinaryEnd); err != nil {
				return err
			}
		}
	}
	m.wakeRan(waiting.kind, true, m.deps.Clock.Now().Sub(waiting.reserved))
	return waiting.run(ctx)
}

// wakeRan is the one line a wake writes, where it runs: at Info, so the production handler keeps
// it, and in one shape whether the wake waited or not, because the operator counts the wakes of a
// minute from it. kind is which wake it was, paced says whether the pacing could have held this
// wake at all — false for the controller's own claim, for a request the pacing lets through, and
// for a daemon whose rate is 0, which is the pacing off — and waitedFor is how long it was held,
// left out of a wake that was not. rate and burst are the pace it was held to, so one line says
// both what happened and what the operator set. The claim is the logger's own (NewMachine).
func (m *Machine) wakeRan(kind WakeKind, paced bool, waitedFor time.Duration) {
	m.log.Info(WakeRanMessage, WakeRanFields(kind, paced, waitedFor, m.deps.WakePace.Rate, m.deps.WakePace.Burst)...)
}

// WakeRanMessage and WakeRanFields are the shape of that line, for the one wake lane outside this
// package: the catch-up the daemon's outbox runs, which the pacing held on its row rather than in
// a machine (the outbox's wakeRan). The shape is the contract — one grep over the daemon's log
// finds every wake of a restart or a plugin wave, whichever lane caused it, and a reader counts
// the wakes of a minute against the rate and the burst each line names — so the two lanes compose
// it here rather than each write their own. A caller with a claim of its own, where this package's
// logger already carries one, prepends it to the fields.
const WakeRanMessage = "supervise: wake ran"

// WakeRanFields is the fields of a wake's line, in order, as wakeRan states them. A wake that
// waited nothing leaves waitedFor out.
func WakeRanFields(kind WakeKind, paced bool, waitedFor time.Duration, rate, burst int) []any {
	fields := []any{"kind", kind, "waited", waitedFor > 0}
	if waitedFor > 0 {
		fields = append(fields, "waitedFor", waitedFor)
	}
	return append(fields, "paced", paced, "rate", rate, "burst", burst)
}

// dropWakeFor is an operator's wake never waiting: a request the pacing lets through, reaching a
// claim a paced wake parked with no process, drops that wake and is then decided against the
// claim the gate found. Parked, the claim answers an operator's resume "the claim is not
// suspended" and an operator's forget-session "the claim has a live process or launch", neither
// of which is true of a claim that is only waiting its turn, and it queues an operator's own task
// behind that turn where the claim the gate found would have launched for it at once
// (dropsWaitingWake, deliverResuming).
//
// Only a wake with no process is dropped: one over a process still running left the claim exactly
// as it was, so every row already decides it as it stands — and the task such a claim is handed
// waits for the process its launch is about to start, rather than for the one it replaces
// (sendPending). The state is restored in memory alone, and the decision that follows writes the
// claim — a resume, a retry, a spawn and an operator's delivery launch, and a forget-session
// persists. The dropped wake's token is not handed back, since the limiter charges on the
// reserve, so every wake behind it keeps the turn it already holds.
//
// The wake is returned so that a decision which launched nothing can take it up again
// (reparkWake): a wake that waits must lose nothing, and a refusal, a named ignore or a
// forget-session leaves the claim exactly where the gate found it, with the launch this wake
// holds still owed to it.
//
// A paced request drops nothing: a second wake request takes no second token, and the claim's
// own wake is the launch it is asking for.
func (m *Machine) dropWakeFor(ev Event) *wake {
	if m.wake == nil || m.claim.Locator != nil || !dropsWaitingWake(ev) {
		return nil
	}
	dropped := m.wake
	request, _ := requestName(ev)
	m.log.Info("supervise: a request that does not wait reached a claim the wake pacing holds; dropping the wake",
		"request", request, "kind", dropped.kind, "due", dropped.at, "state", dropped.state)
	m.claim.State = dropped.state
	m.disarm(TimerWake)
	m.wake = nil
	return dropped
}

// reparkWake takes a dropped wake up again when the decision it was dropped for launched nothing
// (Handle, after both the named ignore's refusal and the row that acted): the claim is in the
// state the gate found, with no process and no wake of its own, which is where a refusal, an
// ignore and a forget-session each leave it — and the launch the wake holds is still owed. It is
// parked again by the park the gate itself makes (parkWake), at the same release moment, for
// whatever is left of the wait: the token was taken once, so this costs nothing and the wakes
// behind it keep their turns.
//
// A write that fails is logged and changes no verdict: the request is answered as the table
// answered it, and the claim stands as that answer left it, which is what a refusal leaves today.
// The wake still waits in memory, and a restart loses it as it loses any other.
func (m *Machine) reparkWake(ctx context.Context, dropped *wake) {
	if dropped == nil || m.wake != nil || m.claim.Locator != nil || m.claim.State != dropped.state {
		return
	}
	m.log.Info("supervise: the request launched nothing; the wake the pacing holds keeps its turn",
		"kind", dropped.kind, "due", dropped.at, "state", dropped.state)
	if err := m.parkWake(ctx, dropped, max(dropped.at.Sub(m.deps.Clock.Now()), 0)); err != nil {
		m.log.Warn("supervise: the wake that keeps its turn could not be written; it waits in memory alone",
			"kind", dropped.kind, "due", dropped.at, "error", err)
	}
}

// dropsWaitingWake is whether ev is a request the wake pacing never makes wait over a parked
// claim: every request exemptWake lets through, an operator's forget-session — which launches
// nothing, but which a claim parked launching would refuse "the claim has a live process or
// launch", and it has neither — and an operator's own delivery, the one the API's deliver route
// sends with no id of a row (api.operator's deliver, RequestDeliver.ID). The delivery is here
// because the claim it reaches is likely parked over the state the gate found it in: a suspended
// claim parked by the rule's own resume answers a delivery by queuing it behind that paced turn,
// where the state the gate found answers it by launching at once (deliverResuming, revive).
//
// The daemon's resume by rule is none of them: it meets a parked claim with "the claim runs:
// nothing to resume" (nothingToDo), which is the truth — the wake the claim waits for is the
// launch that rule would make.
func dropsWaitingWake(ev Event) bool {
	switch ev := ev.(type) {
	case RequestForgetSession:
		return true
	case RequestDeliver:
		return ev.ID == ""
	}
	return exemptWake(ev)
}

// exemptWake is whether the launch a request raises is one the wake pacing lets through: the two
// that reach a machine from the operator's own route alone (RequestResume, RequestOperatorRetry),
// which a person is waiting on, and a spawn whose caller marked it exempt — the operator's spawn
// route, and the controller keeper's, whose claim reserveWake exempts by identity anyway
// (claim.IsController). Every other path into a launch is paced, so every other event is too: an
// operator's delivery carries no launch of its own, and revive reads the exemption from the
// delivery itself (deliverResuming).
func exemptWake(ev Event) bool {
	switch ev := ev.(type) {
	case RequestSpawn:
		return ev.Exempt
	case RequestResume, RequestOperatorRetry:
		return true
	}
	return false
}
