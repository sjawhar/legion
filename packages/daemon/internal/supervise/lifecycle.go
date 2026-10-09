package supervise

import (
	"context"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The tree lifecycle (internal/treelifecycle) is the store's barrier between a tree's runnable
// work and its cleanup, under every runtime. A claim is bound to its tree's open epoch when it is
// admitted (Store.AdmitClaim) and every launch rechecks that binding (Store.CheckLaunch) before it
// persists StateLaunching or calls the runtime. A reserved cleanup answers either with
// treelifecycle.ErrCleanupReserved, a wait that changes nothing and charges no budget; the start
// that met it retries once the cleanup confirmed and a fresh admission opened the next epoch.

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

// retreeable is where a claim runs nothing and may move to another tree: queued, or one of the
// states its process is gone in. A launch whose outcome is uncertain may still run, so it waits.
var retreeable = append([]ClaimState{StateQueued}, gone...)

// Retree re-points a claim to tree, the tree its issue belongs to now: a child of a closed tree
// re-admitted as a root of its own keeps its roles' claims, which still name the tree it left. The
// claim drops its old tree's epoch, so its next start binds the new tree's lifecycle and launches
// in the new tree's resources. Under a runtime that keeps sessions on the tree's volume
// (SessionsOnVolume) the session stays on the old tree's volume, which the new tree's pods never
// mount, so the claim drops it as a lost volume's claims do and starts fresh, recreating its
// workspace; under tmux, whose sessions are on the host, and under the Sandbox runtime's session
// database, the session resumes. Only a claim that runs nothing is re-pointed; one whose process
// still runs in the old tree is a wait (wait.ErrWaiting) until its old tree's stop lands.
func (m *Machine) Retree(ctx context.Context, tree string) error {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.Tree == tree {
		return nil
	}
	if !slices.Contains(retreeable, m.claim.State) {
		return wait.Errorf("re-point claim %s from tree %s to %s: it is %s there", m.claim.Token, m.claim.Tree, tree, m.claim.State)
	}
	m.log.Info("supervise: the claim's issue moved to another tree", "claim", m.claim.Token, "from", m.claim.Tree, "to", tree)
	m.claim.Tree, m.claim.TreeEpoch = tree, 0
	if m.deps.Runtime.SessionsOnVolume() && (m.claim.Session != "" || m.claim.SessionFile != "") {
		m.loseSession()
	}
	return m.persist(ctx)
}
