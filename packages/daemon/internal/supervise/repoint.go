package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// repoint is the claim's process found alive holding an address a process launched now is not
// handed (runtime.StaleAddress): one of the addresses the runtime hands every new process from the
// daemon's configuration (the worker stream it dials, the daemon's API, NATS, Envoy, Dispatch, the
// secrets broker) moved since this one launched, as each does when the daemon restarts with its key
// changed; the worker stream's keys are `advertise_host`, `bind` when no `advertise_host` is set,
// and `worker_stream_port`. The process keeps what it holds for as long as it runs, and what it
// holds may never reach the daemon or the service again, so there is nothing to wait out. A task
// whose turn the process was running goes back to waiting first (interrupted), the same recovery a
// death in a turn gets: that turn used the addresses the process holds, and the relaunch ends it. A
// held suspension ends here as it does for a death (endHeld): the claim is suspended, not
// relaunched. Otherwise the claim is relaunched at once through the launch path a death uses
// (launch): a Resume of its recorded session, or a Spawn over the claim's existing Sandbox when it
// has not registered yet, onto a process handed the current addresses. The stale observation is
// never charged, unlike a death (chargeDeath, relaunchAfterFailure): the process did nothing wrong.
// A relaunch the runtime refuses is charged as any launch failure is.
func (m *Machine) repoint(ctx context.Context, observation runtime.Observation) error {
	m.log.Warn("supervise: the process is alive at a stale address; replacing it with one at the current address",
		"incarnation", m.claim.Locator.Incarnation, "detail", observation.Detail)
	return m.replaceProcess(ctx, "the process at a stale address is replaced mid-turn")
}

// displaced is the claim's process found gone, or found to be some other process, through no
// fault of its own (runtime.Observation.Displaced): the daemon itself replaced its pod while it ran,
// to launch another role there, or it was another runtime's process — a Sandbox role process a
// daemon running the project under kubernetes recorded — that this daemon takes over
// (`runtime.tmux.take_over`) and cannot see. The observation's detail names which. The process did
// nothing wrong, so it is recovered as repoint recovers a stale one: nothing is charged, neither a
// launch failure nor a death with work outstanding, and the claim's session is relaunched at once
// (replaceProcess). A turn the process was in is lost, and its task goes back to waiting, sent
// again once the session resumes; a held suspension ends in a suspension.
func (m *Machine) displaced(ctx context.Context, observation runtime.Observation) error {
	m.log.Warn("supervise: the process was displaced through no fault of its own, its pod replaced by the daemon or it another runtime's process this daemon takes over; relaunching it uncharged",
		"incarnation", m.claim.Locator.Incarnation, "process", m.claim.Locator.Label(), "observed", string(observation.Kind), "detail", observation.Detail)
	return m.replaceProcess(ctx, "the process was displaced mid-turn, its pod replaced by the daemon or it another runtime's process this daemon takes over")
}

// replaceProcess is the recovery a process ended or found stale through no fault of its own
// (repoint's stale address, refreshPlugin's stale plugin contract, displaced's replaced pod) shares
// once logged: a task whose turn the process was running goes back to waiting first (interrupted),
// the same recovery a death in a turn gets; a held suspension ends here as it does for a death
// (endHeld), suspended rather than relaunched; otherwise the claim is relaunched at once through
// the launch path a death uses (launch), a Resume of its recorded session or a Spawn over its
// existing Sandbox when it has not registered yet. interruption is the sentence the turn's recovery
// is logged with.
func (m *Machine) replaceProcess(ctx context.Context, interruption string) error {
	if err := m.interrupted(ctx, interruption); err != nil {
		return err
	}
	if m.held != nil {
		return m.endHeld(ctx)
	}
	return m.launch(ctx)
}

// reclaimRole is the onRoleUnheld row: a daemon publish or a delivery exception found a running
// pane's role topic has no Envoy holder (RoleUnheld), so the pane is relaunched on its session and
// its claim boot re-claims the role. It is the relaunch repoint and refreshPlugin share
// (replaceProcess), charged nothing by this row: the pane did nothing wrong, its listener-side role
// claim was dropped under it. The table reaches it only for a ready or idle claim, which runs no
// turn, so the turn's recovery (interrupted) has nothing to do; replaceProcess then launches — a
// Resume of the recorded session — unless the claim still holds a suspension, which an idle claim can
// when its runtime refused the stop at its turn's end (turnEnded): that suspension is completed
// instead (endHeld) and nothing is launched. A launch the tree lifecycle refuses (checkLaunch) leaves
// the claim as it is and is charged nothing (launchAfter's relaunchRefused). A Resume the runtime
// refuses is charged one launch failure, as any launch's is (launchAfter). The Kubernetes runtime's
// Resume stops the role's earlier generation itself, so there the relaunch is uncharged; the tmux
// runtime's refuses while the live pane it replaces runs, a known tmux-only defect, so there it is
// charged one failure. The daemon's limiter reads the claim's generation afterwards and refunds a
// reclaim that launched nothing (relaunchUnheld, internal/daemon).
func reclaimRole(m *Machine, ctx context.Context, ev Event) error {
	m.log.Warn("supervise: the pane's Envoy role has no holder; relaunching it on its session so its claim boot re-claims",
		"trigger", ev.(RoleUnheld).Trigger, "state", m.claim.State, "session", m.claim.Session, "suspensionHeld", m.held != nil)
	return m.replaceProcess(ctx, "the pane's lost Envoy role is re-claimed by relaunching it on its session")
}
