package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reclaimUnheldRunning heals a pane that went deaf: a daemon publish to its role topic, or a
// delivery exception for it, found the role had no Envoy holder, yet the topic's claim is a running
// pane this daemon supervises. A listener reset drops a role's holder, and the pane's heartbeat
// re-asserts only a role its in-process envoy instance still believes it holds, which the reset can
// erase (docs/solutions/envoy/heartbeat-role-reassertion-and-regain-hooks.md); so nothing on the
// session side re-claims until the pane's next claim boot. This relaunches the pane on its own
// session, which re-runs that claim boot and re-claims the role, so the next copy of the held
// message lands.
//
// It is a dispatch on the claim's state as this daemon supervises it, the recognizer its two
// callers share — the notice executor's publish (outbox.go, notice) and the delivery-exception
// consumer (notice_exceptions.go, rehold):
//
//   - ready or idle — a live pane that can be prompted and is not in a turn — is relaunched
//     (supervise.RoleUnheld). The relaunch moves the claim out of ready/idle at once, so a burst of
//     publishes or exceptions for one topic triggers one relaunch and no more while it is in flight.
//   - working is left for its turn to end: a relaunch would lose the turn's work. The caller holds
//     the message (errNoticeWaits) or re-queues it, so a later attempt catches the pane idle.
//   - a suspended claim is left alone here; the idle-suspended-worker branch (→ RequestRuleResume)
//     that another lane adds goes in this switch's empty slot.
//   - every other state — booting (launching, shim-connected, or registered, whose own claim boot
//     is about to claim the role), failed, retired, or not supervised — holds no running pane with
//     a role to re-claim.
//
// It never touches an operator-suspended, failed, retired, or lingering-tree claim: the callers
// reach it only when the claim can hold its role again, and the states acted on above exclude the
// rest. The relaunch is uncharged — the pane did nothing wrong, its listener-side role claim was
// dropped under it — and any error is logged and swallowed: the message stays held and its retry
// asks again.
func (r *outbox) reclaimUnheldRunning(ctx context.Context, token claim.Token, trigger string) {
	machine, found := r.supervisor.Machine(token)
	if !found {
		return
	}
	switch machine.View().State {
	case supervise.StateReady, supervise.StateIdle:
		if err := machine.Handle(ctx, supervise.RoleUnheld{Claim: token, Trigger: trigger}); err != nil {
			r.log.Warn("outbox could not relaunch a running-but-unheld pane to re-claim its role",
				"claim", token, "trigger", trigger, "error", err)
		}
	// case supervise.StateSuspended: an idle-suspended worker is resumed by rule (RequestRuleResume)
	// here — another lane adds that branch.
	default:
		// A working pane waits for its turn to end; a booting pane claims the role on its own; a
		// failed, retired, or unsupervised claim holds no role to re-claim.
	}
}
