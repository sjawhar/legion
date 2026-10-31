package supervise

import (
	"context"
	"errors"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The tree lifecycle (internal/treelifecycle) is the store's barrier between a tree's runnable
// work and its cleanup, under every runtime. A claim is bound to its tree's open epoch when it is
// admitted (Store.AdmitClaim) and every launch rechecks that binding (Store.CheckLaunch) before it
// persists StateLaunching or calls the runtime. A reserved cleanup answers either with
// treelifecycle.ErrCleanupReserved, a wait that charges no budget and changes nothing but a claim
// whose process a retirement already stopped (relaunchRefused); the start that met it retries once
// the cleanup confirmed and a fresh admission opened the next epoch.

// revive binds an explicitly spawned, resumed or retried claim to the open tree epoch before
// launching. In-lifetime recovery only rechecks the bound epoch (checkLaunch), so an old launch
// cannot cross a cleanup confirmation into a later admission.
func (m *Machine) revive(ctx context.Context) error {
	bound, err := m.deps.Store.AdmitClaim(ctx, m.claim)
	if err != nil {
		return err
	}
	m.claim.TreeEpoch = bound.TreeEpoch
	return m.launch(ctx)
}

// checkLaunch is the lifecycle recheck every launch makes first: a claim whose tree cleanup is
// reserved, or whose bound epoch is no longer the open one, starts nothing and changes nothing, so
// a relaunch refused here charges no budget.
func (m *Machine) checkLaunch(ctx context.Context) error {
	return m.deps.Store.CheckLaunch(ctx, m.claim)
}

// relaunchRefused is a relaunch checkLaunch refused, relaunch the call that made it
// (relaunchAfterFailure, launchAfter) and so the one to try again. A claim that records its
// process, or runs none — a spawn, a resume, a retry — is left as it was. A live claim that records
// none has already let its process go: a retirement's relaunch whose stop already ended the process
// (the registration deadline and a prompt retirement check before they stop, so the check failed
// while the stop waited out its grace), or a launch whose write failed, waiting (relaunchLater). A
// claim left there would have nothing watching or relaunching it. A reserved cleanup suspends it
// for the daemon, its session kept, for the tree's close to retire. Any other failure is not the
// tree's — a store that did not answer — and ends nothing: the relaunch waits (relaunchLater), as
// a stop that fails does.
func (m *Machine) relaunchRefused(ctx context.Context, relaunch func(context.Context) error, err error) error {
	if m.claim.Locator != nil || !slices.Contains(live, m.claim.State) {
		return err
	}
	if errors.Is(err, treelifecycle.ErrCleanupReserved) {
		m.log.Warn("supervise: the tree's cleanup is reserved and the retired process is stopped; suspending the claim for the tree's close", "error", err)
		return errors.Join(err, m.suspended(ctx, SuspendedByDaemon))
	}
	m.log.Warn("supervise: the lifecycle check of the retired process's relaunch failed; trying it again at the probe interval", "error", err)
	m.relaunchLater(relaunch)
	return errors.Join(err, m.persist(ctx))
}

// relaunchLater leaves a claim whose process is already let go waiting launching with no process,
// its task kept, and tries relaunch again at the probe interval (TimerRelaunch, relaunchAgain): a
// relaunch whose lifecycle check the store did not answer (relaunchRefused), or a launch whose
// write failed (launchAfter). Nothing else would relaunch such a claim before a restart, which
// takes it up from what the store last holds.
func (m *Machine) relaunchLater(relaunch func(context.Context) error) {
	m.claim.State = StateLaunching
	m.relaunch = relaunch
	m.arm(TimerRelaunch, m.deps.Timeouts.Probe, "")
}

// relaunchAgain is the relaunch relaunchLater left waiting (TimerRelaunch). A suspension held
// meanwhile ends here, with no process left to stop, rather than in a relaunch; otherwise the
// relaunch is tried again as it was first made, charged as that call charges it.
func relaunchAgain(m *Machine, ctx context.Context, _ Event) error {
	relaunch := m.relaunch
	m.relaunch = nil
	if m.held != nil {
		return m.suspendedFor(ctx, *m.held)
	}
	return relaunch(ctx)
}

// RunsNothing is whether the claim is in a state with no process of its own (RunsNothingStates):
// only such a claim may move to another tree.
func (c Claim) RunsNothing() bool { return slices.Contains(runsNothing, c.State) }

// Retree re-points a claim to tree, the tree its issue belongs to now: a child of a closed tree
// re-admitted as a root of its own keeps its roles' claims, which still name the tree it left. The
// claim drops its old tree's epoch, so its next start binds the new tree's lifecycle and launches
// in the new tree's resources, and keeps its session: the session is the issue's, on the host under
// tmux, and under a sandbox in the runtime's session database (runtime.kubernetes.session_store
// postgres), which every tree's pods read, so it resumes in the new tree while the old tree
// lingers, whatever the old tree's cleanup takes. Only a claim that runs nothing is re-pointed; one
// whose process still runs in the old tree is a wait (wait.ErrWaiting): the new tree's start
// suspends it first, keeping its session, and waits for that suspension to land (the daemon's
// outbox, stopElsewhere).
func (m *Machine) Retree(ctx context.Context, tree string) error {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.Tree == tree {
		return nil
	}
	if !m.claim.RunsNothing() {
		return wait.Errorf("re-point claim %s from tree %s to %s: it is %s there", m.claim.Token, m.claim.Tree, tree, m.claim.State)
	}
	m.log.Info("supervise: the claim's issue moved to another tree", "claim", m.claim.Token, "from", m.claim.Tree, "to", tree)
	m.claim.Tree, m.claim.TreeEpoch = tree, 0
	return m.persist(ctx)
}
