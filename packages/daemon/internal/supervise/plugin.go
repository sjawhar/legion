package supervise

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// PodUpdateContract is the daemon contract that introduced ready_for_update (api/version.go's
// 44). A claim whose registration declared a contract below it cannot answer an update task: the
// op list is a closed enum compiled into the plugin, and a Claude Code role's plugin is its
// image's own, never installed. Such a claim is never asked — an update round records itself on
// it and sends it nothing (openPodUpdate) — and the stale-plugin lane keeps today's immediate
// relaunch for it (refreshPlugin), which keeps the pod and the disk and so loses nothing.
//
// That relaunch is what takes a worker of the release before this one past the constant: it
// installs this daemon's plugin, and the registration that follows records this daemon's
// contract, so the claim can answer the round it is in. What it cannot take past the constant is
// a plugin an install never reaches — a Claude Code role's, which is its pod image's own — so
// that is the one case an operator drains by hand (internal/daemon's cannotAnswer).
//
// It lives here rather than beside DaemonAPIVersion because internal/api imports this package.
// internal/api's own test holds it to that version, so a later contract bump can never leave this
// constant above the daemon's own.
const PodUpdateContract = 44

// CanAnswerUpdate says whether c's registration declared PodUpdateContract or above, so its
// plugin has the ready_for_update op. It reads the contract alone, which is what a suspended
// claim's revive asks (askForUpdate) and what the stale-plugin lane that decides between a round
// and an immediate relaunch asks of a process it holds (refreshPlugin).
func CanAnswerUpdate(c Claim) bool { return c.PluginContract >= PodUpdateContract }

// CannotAnswerUpdate says whether c's worker can never answer the update task it is given: its
// process runs now (HoldsCapability) and the registration that process made declared a plugin
// contract below the one that introduced the op. It is the one statement of that rule, read by
// the round's own ask (askForUpdate), by the stale-plugin lane (refreshPlugin), and by the
// daemon's sweep, which names the claims a round can never finish on, keeps the stale-plugin
// relaunch for the ones it can relaunch, and marks each in the controller's notice
// (internal/daemon's cannotAnswer, refreshStalePlugins, waitingOf).
//
// The running process is half the rule, and the half a reading of the contract alone would get
// wrong. A claim with none — suspended, failed, launching, or still queued — records the contract
// of a process that has ended, and the one its next launch starts speaks this daemon's: a claim
// relaunched onto the current plugin a moment ago registers at this contract, and a suspended
// claim the round revives comes back on it. Reading those records as "this claim can never
// answer" would tell the operator to drain a pod at every roll of this deployment, minutes before
// the claim answers.
func CannotAnswerUpdate(c Claim) bool { return HoldsCapability(c.State) && !CanAnswerUpdate(c) }

// refreshPlugin is the claim's process found to speak a plugin contract this daemon does not: at
// boot, re-adopting a claim whose recorded contract is an earlier daemon's, or none at all (the
// first boot on this release); or, uncharged, a registration that still differs from the daemon's
// but was not itself the registration a relaunch was already made for (staleRegistration, which
// charges that case instead). It is never reached for a claim whose plugin is current — a claim
// still launching is left to its own registration to decide — but it re-checks the comparison
// before doing anything, because the boot's own decision runs later than the post that queued it:
// a registration may have landed, recording the daemon's contract, in between, and such a claim is
// left alone rather than relaunched for a fact already fixed.
//
// A claim that can answer ready_for_update is asked before it is replaced. relaunchStale ends the
// turn in flight, and a worker that never pushed loses that turn's work with it, so a claim
// whose recorded contract is PodUpdateContract or above gets an update round of its own instead
// (openPluginUpdate) and keeps running until it answers. A claim below that contract cannot
// answer at all and is relaunched at once, as every such claim was before this round existed. The
// relaunch keeps the pod and the disk under every runtime, so what it costs is one interrupted
// turn. The other caller of relaunchStale is the stale registration (staleRegistration), which
// calls it rather than this: that agent is refused and its process exits on the refusal, so a
// round there would wait for a signal no running turn could send.
//
// The project controller's own claim keeps the immediate relaunch too, whatever contract it
// records: the `legion` tool its session holds is the controller's, which has read_state and
// set_status and no ready_for_update op at all (packages/pi-legion/src/tools.ts), so a round over
// it would ask for an answer no controller turn can give and would stay open for ever. The
// relaunch costs it the turn in flight and nothing else — its pod holds no issue workspace, so it
// has nothing to push.
//
// What it does once it decides the claim is still stale is exactly repoint: the turn in flight is
// interrupted and its task kept waiting for the relaunch to send again, a held suspension ends in a
// suspension rather than a relaunch (endHeld), and otherwise the recorded session is resumed on a
// new process (launch). Nothing here is charged — the process did nothing wrong; it simply
// predates this daemon's plugin — and the loop this could otherwise become is closed by the budget
// ready alone resets: a claim whose new process never reaches ready at the generation this starts
// keeps whatever LaunchFailures a true launch failure already charged it, and a registration that
// arrives at that generation still speaking the old contract is not this path's to decide again —
// it is staleRegistration's, which charges it.
//
// relaunchStale can refuse the relaunch it starts — a tree lifecycle cleanup already reserved, a
// moved epoch, a transient store error — so when no suspension is already held (m.held nil, the
// case replaceProcess would launch rather than end in a suspension) that same check runs there
// first, before the Warn, the PluginRelaunchFor mark, or interrupted's turn recovery: a refused
// attempt returns having changed nothing, exactly as stale as it found it, and the orphan sweep's
// re-post of StalePlugin for it (refreshStalePlugins in internal/daemon) is a true no-op until the
// refusal clears. A held suspension is checked nothing there, since replaceProcess ends it in a
// suspension (endHeld) rather than a launch, and endHeld makes no launch-path call to refuse.
// launch rechecks the same thing inside replaceProcess's own call — that second check is the price
// of keeping launch whole, the same price relaunchAfterFailure already pays (budgets.go).
// Interrupting an already-interrupted turn (interrupted) is itself a no-op, the pending delivery
// already marked, so a retried attempt never re-delivers or charges a task twice.
func (m *Machine) refreshPlugin(ctx context.Context, trigger string) error {
	if m.claim.PluginContract == m.deps.PluginContract {
		return nil
	}
	if !CannotAnswerUpdate(m.claim) && !claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree) {
		return m.openPluginUpdate(ctx)
	}
	return m.relaunchStale(ctx, trigger)
}

// relaunchStale replaces the claim's process now, onto this daemon's plugin: the stale lane for a
// claim no update round can wait for. Its two callers are refreshPlugin, for a claim whose plugin
// cannot answer a round at all, and staleRegistration, whose own registration is refused in the
// same decision. trigger names the pass that found the claim ("boot", "sweep", "registration"),
// for the line it logs. refreshPlugin states what the relaunch does and what refuses it.
func (m *Machine) relaunchStale(ctx context.Context, trigger string) error {
	if m.held == nil {
		if err := m.checkLaunch(ctx); err != nil {
			return err
		}
	}
	m.log.Warn("supervise: the process speaks another plugin contract; relaunching it onto the current plugin",
		"incarnation", m.claim.Locator.Incarnation, "pluginContract", m.claim.PluginContract,
		"daemonApiVersion", m.deps.PluginContract, "trigger", trigger)
	m.claim.PluginRelaunchFor = m.deps.PluginContract
	return m.replaceProcess(ctx, "the process on another plugin contract is replaced mid-turn")
}

// openPluginUpdate opens this claim's own update round for the plugin contract it is behind: the
// worker is asked to push its work and answer ready_for_update, and the process is replaced only
// once it has (PodUpdateEnded on a claim in a plugin round, which the daemon's sweep posts). It
// returns having relaunched nothing.
//
// The round is this claim's alone, never its pod's: the stale-plugin relaunch keeps the pod and
// the disk, so it costs one interrupted turn rather than a workspace, and holding a contract bump
// behind every worker of a pod would buy nothing. The task says that much (UpdateTask reads the
// round's kind). A round already open on the claim — this one, or the pod's — is left exactly as
// it is: the sweep that opened it is what carries it on, and re-opening it here would start its
// clock again at every post of StalePlugin.
func (m *Machine) openPluginUpdate(ctx context.Context) error {
	if m.claim.UpdateRound != "" {
		return nil
	}
	round := PluginRound(m.deps.PluginContract)
	reasons := fmt.Sprintf("the process speaks plugin contract %d, and this daemon speaks %d",
		m.claim.PluginContract, m.deps.PluginContract)
	m.log.Warn("supervise: the process speaks another plugin contract; its worker is asked to push its work before the process is replaced",
		"incarnation", m.claim.Locator.Incarnation, "pluginContract", m.claim.PluginContract,
		"daemonApiVersion", m.deps.PluginContract)
	return openPodUpdate(m, ctx, PodUpdateOpen{
		Claim:   m.claim.Token,
		Round:   round,
		Reasons: reasons,
		Task:    UpdateTask(round, reasons),
	})
}

// refreshStalePlugin is the StalePlugin event's row, for the four states a claim's relaunched
// process can run in: the event's own Trigger (boot or sweep) names the pass that posted it.
func refreshStalePlugin(m *Machine, ctx context.Context, ev Event) error {
	return m.refreshPlugin(ctx, ev.(StalePlugin).Trigger)
}

// staleRegistration is what register and reregister run once they have recorded the plugin
// contract the request declared, whatever it is: a registration whose contract still differs from
// the daemon's is relaunched onto the current one, and the registration itself is refused with the
// sentence that tells the plugin why (claim.StalePluginContract) — the agent that just registered
// is not the one the relaunch starts, and pi-legion exits on the 4xx, so its own log then says why.
// The relaunch runs first and is charged only when it is the second relaunch in a row still to find
// the daemon's contract: the claim remembers, in PluginRelaunchFor, the contract its last relaunch
// was made for, so a registration that still differs while PluginRelaunchFor already names this
// daemon's contract means the plugin or the image the relaunch put the process on is itself behind
// this daemon, and relaunching it again for free would loop; relaunchAfterFailure charges it
// instead and ends it in a worker-died once the launch budget runs out. Every other stale
// registration — PluginRelaunchFor unset, or made for a contract this daemon has since moved past —
// is relaunchStale's uncharged relaunch. This path never opens an update round, whatever the
// claim's contract: the agent that just registered is refused and its process exits on that
// refusal, so a round here would wait for an answer no running turn could give.
//
// A registration that already speaks this daemon's contract ends a plugin round open over the
// claim (endCurrentPluginRound): the process the round was opened for is gone, and the one that
// registered needs no replacement. Leaving the round open would hold the claim's next task behind
// it and end it, hours later, in a relaunch of a process that is already current.
//
// A daemon with nothing to relaunch onto (PluginRelaunchable false, LEGION-164's degraded
// Kubernetes mode) relaunches nothing and refuses nothing: the registration stands, accepted, and
// one Error line records the gap for the operator to close with runtime.kubernetes.plugins.
func (m *Machine) staleRegistration(ctx context.Context, got int) error {
	if got == m.deps.PluginContract {
		return m.endCurrentPluginRound(ctx)
	}
	if !m.deps.PluginRelaunchable {
		m.log.Error(NoPluginSourceMessage,
			"incarnation", m.claim.Locator.Incarnation, "pluginContract", got, "daemonApiVersion", m.deps.PluginContract)
		return nil
	}
	var err error
	if m.claim.PluginRelaunchFor == m.deps.PluginContract {
		err = m.relaunchAfterFailure(ctx)
	} else {
		err = m.relaunchStale(ctx, "registration")
	}
	if err != nil {
		return err
	}
	return claim.StalePluginContract(got, m.deps.PluginContract)
}

// NoPluginSourceMessage is logged whenever a daemon that can relaunch nothing onto a plugin
// (supervise.Deps.PluginRelaunchable false, LEGION-164's degraded Kubernetes mode) finds a stale
// process instead of relaunching it: staleRegistration here, refreshStalePlugins
// (internal/daemon/daemon.go) at boot.
const NoPluginSourceMessage = "supervise: the process speaks another plugin contract, and this daemon knows no plugin versions to relaunch it onto; set runtime.kubernetes.plugins"
