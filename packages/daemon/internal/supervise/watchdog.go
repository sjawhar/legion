package supervise

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The turn watchdog bounds a turn that has stopped making progress.
//
// Measured on a deployment over the 24 hours to 2026-10-25, across 2,059 completed turns, the
// longest silence inside an open turn was 2h24m (an implementer running a subagent) and the
// longest turn 2h22m: real work goes quiet for a long time, and the silence that is a fault looks
// exactly like the silence that is a build. So the clock is per role and generous — several times
// each role's measured worst case — and the ladder below starts with the step that costs the agent
// nothing, escalating only when the step before it has not brought the turn back:
//
//  1. Ask the process for its state (WatchdogProbed). The existing get_state frame, which starts no
//     turn. An answer proves the process is up and nothing more: in the freeze this was built for
//     the process answered while its shell was deadlocked, so an answer never clears the clock —
//     only a new turn event does (lastTurnEvent). No answer, or no connection at all, means the
//     process is gone, and the ladder goes straight to the relaunch.
//  2. Interrupt the turn (WatchdogInterrupted). The existing abort, which cancels the turn's tool
//     calls and keeps the process and its session — what Quiesce sends a worker whose phase another
//     start takes over. A merely slow turn loses its current step and nothing else. The turn's end
//     is the agent_end that follows (turnEnded), waited out for Timeouts.Stop, the daemon's own
//     grace for a turn it has asked to end.
//  3. Relaunch the claim, resuming its session (WatchdogRelaunched). Exactly what the operator's
//     `legion claims suspend` and `legion claims resume` did by hand: the process is replaced, the
//     session resumed, the workspace and the issue's branch untouched, and the outstanding task
//     delivered again (interrupted).
//  4. Report (WatchdogReported). Only once the relaunch has not produced a ready agent within the
//     registration deadline: the daemon tells the issue's architect, or the controller role for the
//     controller's own claim (Deps.Wedged), and logs it.
//
// Each rung is recorded on the claim (Claim.Watchdog) before the next is armed, so a daemon restart
// takes the ladder up where it left it rather than starting over, and `legion state` shows it.
// Reaching ready clears the record (watchdogRecovered): the agent is back.

// WatchdogStep is the rung of the ladder a claim's watchdog record last ran.
type WatchdogStep string

const (
	// WatchdogProbed: the agent answered get_state, which says its process is up and nothing about
	// its turn.
	WatchdogProbed WatchdogStep = "probed"
	// WatchdogInterrupted: the agent took the abort, and its turn's end is awaited.
	WatchdogInterrupted WatchdogStep = "interrupted"
	// WatchdogRelaunched: the claim was relaunched on its own session.
	WatchdogRelaunched WatchdogStep = "relaunched"
	// WatchdogReported: the relaunch produced no ready agent, and a human was told.
	WatchdogReported WatchdogStep = "reported"
)

// Watchdog is the turn watchdog's position on a claim whose turn stopped producing events, and nil
// on every claim whose agent is working or whose turn ended. It is persisted as the JSON its tags
// name (the store's `claims.watchdog`), so a restart does not repeat a rung, and it is published in
// `legion state`.
type Watchdog struct {
	// Step is the rung that last ran.
	Step WatchdogStep `json:"step"`
	// Silence is how long the turn had produced no event when the ladder started.
	Silence time.Duration `json:"silence"`
	// At is when Step ran.
	At time.Time `json:"at"`
	// Steps is every rung tried, in order, each with what it found, for the report and the state.
	Steps []string `json:"steps"`
}

// WedgedTurn is the ladder's report: its relaunch did not bring the claim's agent back
// (Deps.Wedged). The daemon publishes it to the issue's architect — or, for the controller's own
// claim, to the controller role — and logs it.
type WedgedTurn struct {
	// Claim is the claim as the machine holds it, with its role, issue and state.
	Claim Claim
	// Silence is how long the turn had produced no event when the ladder started.
	Silence time.Duration
	// Steps is every rung tried, in order, each with what it found.
	Steps []string
}

// Reason is the one sentence the report is told in: the claim, how long it was wedged, and the
// rungs that were tried.
func (w WedgedTurn) Reason() string {
	return fmt.Sprintf("%s: its turn produced no event for %s and the turn watchdog could not bring its agent back (%s); it is %s",
		w.Claim.Token, w.Silence.Round(time.Second), strings.Join(w.Steps, "; "), w.Claim.State)
}

// turnLimit is how long this claim's role may produce no turn event before the watchdog acts, and
// zero for a role the deployment switched the watchdog off for (Deps.TurnLimit, legion.yaml's
// `turn_watchdog_seconds`). A daemon that supplies no limits watches nothing.
func (m *Machine) turnLimit() time.Duration {
	if m.deps.TurnLimit == nil {
		return 0
	}
	return m.deps.TurnLimit(m.claim.Role)
}

// lastTurnEvent is when this claim's agent last produced an event of its own: the turn boundary
// the machine saw (turnEventAt, set wherever the claim becomes working or its turn ends) or any
// frame the connection has read since (runtime.Conn.LastEvent) — a tool call, a message, the
// turn's own start. The later of the two, so a connection that has read nothing yet never drags
// the mark back.
//
// A shim replays its backlog on every reconnect, which marks the connection now for frames the
// agent wrote long ago. That is one reset per reconnect and it only ever delays the ladder, which
// is the safe direction: a wedged turn is judged again at the next limit.
func (m *Machine) lastTurnEvent() time.Time {
	at := m.turnEventAt
	if conn, ok := m.deps.Conns.Conn(m.claim.Token); ok {
		if last := conn.LastEvent(); last.After(at) {
			at = last
		}
	}
	return at
}

// armWatchdog starts the turn watchdog's clock for a turn now running: the role's whole limit from
// now, or nothing at all for a role the watchdog is off for. The claim's own turn-event mark moves
// with it, since a turn starting is an event of the agent's.
func (m *Machine) armWatchdog() {
	m.turnEventAt = m.deps.Clock.Now()
	limit := m.turnLimit()
	if limit <= 0 {
		m.disarm(TimerWatchdog)
		return
	}
	m.arm(TimerWatchdog, limit, "")
}

// watchdogRecovered is the claim's agent back: its turn ended, or a relaunch of it reached ready.
// The ladder's record goes, so the next wedge starts at the first rung, and its timer with it.
func (m *Machine) watchdogRecovered(reason string) {
	m.disarm(TimerWatchdog)
	if m.claim.Watchdog == nil {
		return
	}
	m.log.Info("supervise: the turn watchdog's claim is working again", "reason", reason,
		"step", string(m.claim.Watchdog.Step), "steps", strings.Join(m.claim.Watchdog.Steps, "; "))
	m.claim.Watchdog = nil
}

// watchdogTurn is the watchdog's timer firing while the agent is in a turn. A turn that has
// produced an event since the arming is working: the clock restarts from that event, and nothing
// else happens — which is the whole of the no-false-positive case, a two-hour subagent included.
// Otherwise the ladder takes its next rung.
func watchdogTurn(m *Machine, ctx context.Context, _ Event) error {
	limit := m.turnLimit()
	if limit <= 0 {
		return nil
	}
	silence := m.deps.Clock.Now().Sub(m.lastTurnEvent())
	if silence < limit {
		m.arm(TimerWatchdog, limit-silence, "")
		return nil
	}
	return m.escalate(ctx, silence)
}

// escalate runs the ladder's next rung for a turn that has been silent for silence.
func (m *Machine) escalate(ctx context.Context, silence time.Duration) error {
	switch m.watchdogStep() {
	case "":
		return m.watchdogProbe(ctx, silence)
	case WatchdogProbed:
		return m.watchdogInterrupt(ctx, silence)
	case WatchdogInterrupted:
		return m.watchdogRelaunch(ctx, silence)
	}
	// Relaunched or reported: the relaunch's own watch decides what follows (watchdogAfterRelaunch),
	// and a claim back in a turn has had its record cleared.
	return nil
}

// watchdogStep is the rung the claim's record last ran, and "" for a claim with no record.
func (m *Machine) watchdogStep() WatchdogStep {
	if m.claim.Watchdog == nil {
		return ""
	}
	return m.claim.Watchdog.Step
}

// recordStep moves the claim's watchdog to step, keeping the silence the ladder started at and adding
// what this rung found. It does not write the claim: the rung's own action does, so a rung and its
// record land together.
func (m *Machine) recordStep(step WatchdogStep, silence time.Duration, found string) {
	w := m.claim.Watchdog
	if w == nil {
		w = &Watchdog{Silence: silence}
		m.claim.Watchdog = w
	}
	w.Step, w.At = step, m.deps.Clock.Now()
	w.Steps = append(w.Steps, found)
}

// watchdogProbe is rung 1: ask the process for its state. It starts no turn, so it costs a slow
// turn nothing. An answer records the rung and gives the turn one probe interval more before the
// interrupt; a process that cannot answer, or has no connection at all, is gone, and the ladder
// goes straight to rung 3.
func (m *Machine) watchdogProbe(ctx context.Context, silence time.Duration) error {
	m.log.Warn("supervise: the agent's turn has produced no event within its role's limit; asking the process for its state",
		"silence", silence.Round(time.Second), "limit", m.turnLimit())
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		m.recordStep(WatchdogProbed, silence, "the claim had no connection to ask")
		m.recordStep(WatchdogInterrupted, silence, "no connection to interrupt the turn over")
		return m.watchdogRelaunch(ctx, silence)
	}
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	state, err := conn.GetState(asking)
	if err != nil {
		m.recordStep(WatchdogProbed, silence, "the process did not answer get_state: "+err.Error())
		m.recordStep(WatchdogInterrupted, silence, "a process that does not answer takes no abort")
		return m.watchdogRelaunch(ctx, silence)
	}
	m.recordStep(WatchdogProbed, silence, fmt.Sprintf("the process answered get_state (streaming=%t), which says nothing about its turn", state.IsStreaming))
	// The answer clears nothing: only a turn event does. The interrupt waits one probe interval,
	// so a turn that speaks in that window is never cut off.
	m.arm(TimerWatchdog, m.deps.Timeouts.Probe, "")
	return m.persist(ctx)
}

// watchdogInterrupt is rung 2: the existing abort, which cancels the turn's tool calls and keeps
// the process and its session. The turn's end is the agent_end that follows, not the abort's
// answer, so the ladder waits Timeouts.Stop for it — the daemon's own grace for a turn it has
// asked to end — and relaunches if it does not come. An abort the agent cannot take means the
// process is past interrupting: the ladder goes to rung 3 at once.
func (m *Machine) watchdogInterrupt(ctx context.Context, silence time.Duration) error {
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		m.recordStep(WatchdogInterrupted, silence, "no connection to interrupt the turn over")
		return m.watchdogRelaunch(ctx, silence)
	}
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	if err := conn.Abort(asking); err != nil {
		m.recordStep(WatchdogInterrupted, silence, "the agent did not take the abort: "+err.Error())
		return m.watchdogRelaunch(ctx, silence)
	}
	m.log.Warn("supervise: the turn watchdog interrupted the wedged turn; waiting for its end",
		"silence", silence.Round(time.Second), "grace", m.deps.Timeouts.Stop)
	m.recordStep(WatchdogInterrupted, silence, "the agent took the abort")
	m.arm(TimerWatchdog, m.deps.Timeouts.Stop, "")
	return m.persist(ctx)
}

// watchdogRelaunch is rung 3: the claim relaunched on its own session, which is the operator's
// suspend-then-resume. The task whose turn it is goes back first (interrupted), as a death in a
// turn takes it back, so the relaunched agent is sent it again; then the process is stopped and
// the same session launched. A relaunch the tree lifecycle refuses changes nothing and is tried
// again at the probe interval (relaunchRefused), as every other relaunch is.
//
// The rung is recorded before the launch, so the launch's own write carries it: a daemon that
// crashes mid-relaunch takes the ladder up at rung 3 rather than interrupting a fresh agent.
func (m *Machine) watchdogRelaunch(ctx context.Context, silence time.Duration) error {
	m.log.Warn("supervise: the turn watchdog relaunches the claim on its own session",
		"silence", silence.Round(time.Second), "steps", strings.Join(m.watchdogSteps(), "; "))
	if err := m.interrupted(ctx, "the turn watchdog relaunches the agent whose turn wedged"); err != nil {
		return err
	}
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, func(ctx context.Context) error { return m.watchdogRelaunch(ctx, silence) }, err)
	}
	m.recordStep(WatchdogRelaunched, silence, "the claim was relaunched on its own session")
	if m.claim.Locator != nil {
		if err := m.suspendProcess(ctx); err != nil {
			// The stop failed, so the process is still the claim's: the record stands and the
			// relaunch is tried again at the probe interval, as a held suspension's stop is.
			m.arm(TimerWatchdog, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("stop the wedged process of %s: %w", m.claim.Token, err)
		}
	}
	return m.launch(ctx)
}

// watchdogRelaunchFailed is rung 4's trigger: the relaunch the ladder made has not produced a
// live, registered agent, which is what the claim's own boot watch decides — its registration
// deadline running out (registrationDeadline), or its launch budget with it (fail). Why names
// which. A claim the ladder holds nothing on, or one whose agent became ready meanwhile (its
// record cleared), reports nothing.
//
// It is read from the claim rather than armed as a timer of the watchdog's own, because the
// relaunch raises the claim's generation and every timer armed before it is fenced out: the one
// bound that survives a relaunch is the boot watch the relaunch itself arms.
func (m *Machine) watchdogRelaunchFailed(ctx context.Context, why string) error {
	if m.watchdogStep() != WatchdogRelaunched {
		return nil
	}
	return m.watchdogReport(ctx, why)
}

// watchdogReport is rung 4: the relaunch produced no ready agent, so a human is told, why naming
// what decided that. It is the one rung that reaches outside the daemon, and it runs once — the
// record moves to reported, which no later trigger acts on.
func (m *Machine) watchdogReport(ctx context.Context, why string) error {
	w := m.claim.Watchdog
	m.recordStep(WatchdogReported, w.Silence, "the relaunched agent did not become ready: "+why)
	report := WedgedTurn{Claim: copyClaim(m.claim), Silence: w.Silence, Steps: m.watchdogSteps()}
	m.log.Error("supervise: the turn watchdog could not recover the claim; reporting it", "silence", w.Silence.Round(time.Second),
		"why", why, "steps", strings.Join(report.Steps, "; "), "state", string(m.claim.State))
	if err := m.persist(ctx); err != nil {
		return err
	}
	if m.deps.Wedged != nil {
		m.deps.Wedged(report)
	}
	return nil
}

// watchdogSteps is every rung the claim's record has tried, in order; empty for a claim with none.
func (m *Machine) watchdogSteps() []string {
	if m.claim.Watchdog == nil {
		return nil
	}
	return append([]string(nil), m.claim.Watchdog.Steps...)
}
