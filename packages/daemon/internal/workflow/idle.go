package workflow

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// IdleHold is why a pod the idle rule read was not released, or IdleHoldNone when it was. The
// daemon records the hold of every pod it keeps past the idle period, so the value is the
// vocabulary of that record and String is how it is written down.
type IdleHold int

const (
	// IdleHoldNone: nothing held the pod and it is released.
	IdleHoldNone IdleHold = iota
	// IdleHoldTreeNotLive: the issue or its tree root has left the workflow, or the root lingers.
	// Such a pod is a close's to stop, not a release's.
	IdleHoldTreeNotLive
	// IdleHoldAwaitingPerson: the pod is a root's, and the root waits on an answer a person has
	// not given. Its children's pods are not held by it.
	IdleHoldAwaitingPerson
	// IdleHoldImage: the pod runs an image other than the one the configuration names, or its
	// image could not be read at all.
	IdleHoldImage
	// IdleHoldPluginContract: a live claim of the pod speaks a plugin contract other than the
	// daemon's current one.
	IdleHoldPluginContract
	// IdleHoldInTurn: a claim of the pod has a process in a turn, starting or stopping, or one
	// whose launch is not yet known.
	IdleHoldInTurn
	// IdleHoldDelivery: a task, notice, message or start waits for delivery to a role of the pod.
	IdleHoldDelivery
	// IdleHoldBackgroundWork: a live role's session reported background work of its own running.
	IdleHoldBackgroundWork
	// IdleHoldUnreported: a live role's session has reported nothing, or last reported before its
	// own last turn, so nothing says what it is doing now.
	IdleHoldUnreported
	// IdleHoldReminder: a live phase worker whose phase is open has a phase reminder due inside
	// the idle period, which is a turn due.
	IdleHoldReminder
	// IdleHoldNotIdle: the pod did something inside the idle period, so the period has not passed.
	IdleHoldNotIdle
	// IdleHoldNoPod: the record holds no such issue, or the issue has no live claim to release.
	IdleHoldNoPod
	// IdleHoldReleasing: a release of this pod is already in progress.
	IdleHoldReleasing
	// IdleHoldBundleTooLarge: the workspace's unpushed work does not fit the bundle a release
	// carries (runtime.MaxWorkspaceBundleBytes), so the pod is kept and the work with it.
	IdleHoldBundleTooLarge
	// IdleHoldBundleSave: the bundle could not be built or could not be saved, so the pod is kept.
	IdleHoldBundleSave
)

var idleHoldNames = map[IdleHold]string{
	IdleHoldNone:           "none",
	IdleHoldTreeNotLive:    "tree_not_live",
	IdleHoldAwaitingPerson: "awaiting_person",
	IdleHoldImage:          "image",
	IdleHoldPluginContract: "plugin_contract",
	IdleHoldInTurn:         "in_turn",
	IdleHoldDelivery:       "delivery_waiting",
	IdleHoldBackgroundWork: "background_work",
	IdleHoldUnreported:     "unreported",
	IdleHoldReminder:       "reminder_due",
	IdleHoldNotIdle:        "not_idle",
	IdleHoldNoPod:          "no_pod",
	IdleHoldReleasing:      "releasing",
	IdleHoldBundleTooLarge: "bundle_too_large",
	IdleHoldBundleSave:     "bundle_save_failed",
}

// String is the hold's recorded name, which a reader of the daemon's release records reads.
func (h IdleHold) String() string {
	if name, ok := idleHoldNames[h]; ok {
		return name
	}
	return "unknown"
}

// IdleDecision is what IdleRelease decided about one pod: whether it is released, the hold that
// kept it when it is not, and the roles whose live claims a release suspends, in the order the
// view read them.
type IdleDecision struct {
	Release bool
	Hold    IdleHold
	Roles   []claim.Role
}

// IdleRelease decides whether an issue's pod may be released now. It is the whole rule and it is
// pure: the view the store read (store.IdleView), the clock, the idle period, whether the pod
// runs the image the configuration names, and the plugin contract this daemon serves
// (api.DaemonAPIVersion) are all it reads, so every condition below is one row of a table test.
//
// A pod is released only when all of these hold, and the first that does not is the hold the
// decision reports:
//
//   - the issue is in the workflow and its tree is live: neither the issue nor its root has left
//     the workflow, and the root does not linger. A pod in that state is a close's to stop;
//
//   - the pod is not a root's that waits on an answer a person has not given. A root whose answer
//     has come waits only for its slot and qualifies, and the children of a root that waits on a
//     person qualify whatever their root is waiting for;
//
//   - the pod runs the image the configuration names, and each live claim speaks this daemon's
//     plugin contract: a released pod comes back as a fresh pod on the configured image, so a pod
//     already behind is a relaunch's to replace, not a release's to save;
//
//   - no claim of the pod has a process in a turn, starting or stopping, or whose launch is not
//     yet known. Failed, retired and already suspended claims stay as they are and hold nothing;
//
//   - no task, notice, message or start waits for delivery to a role of the pod;
//
//   - each live role's session reported, after its last turn, that it runs no background work. A
//     claim with no report, or one whose report predates its last turn, is never released: that
//     is what makes a host that cannot report degrade safely;
//
//   - no live phase worker whose phase is open has a phase reminder due inside the idle period. A
//     phase worker's phase is open when the issue's current phase is that worker's role and its
//     claim serves the current generation;
//
//   - the pod has done nothing the daemon recorded for the whole idle period.
//
// The plan names one more condition, "no INFRA-484 update round open on the pod", which reads
// supervise.Claim.UpdateRound through InPodRound(). That field does not exist on this branch; it
// arrives with sjawhar/legion#2538, and the forward merge that brings it in adds its row here and
// one row to the table test.
func IdleRelease(view store.IdleView, now time.Time, idle time.Duration, onConfiguredImage bool, contract int) IdleDecision {
	if !view.Found {
		return IdleDecision{Hold: IdleHoldNoPod}
	}
	if view.Releasing {
		return IdleDecision{Hold: IdleHoldReleasing}
	}
	if !idleTreeLive(view) {
		return IdleDecision{Hold: IdleHoldTreeNotLive}
	}
	if view.IsRoot() && view.Root.AwaitingPerson && !view.Root.Answered {
		return IdleDecision{Hold: IdleHoldAwaitingPerson}
	}
	var roles []claim.Role
	for _, c := range view.Claims {
		if !c.Live() {
			continue
		}
		roles = append(roles, c.Role)
	}
	if len(roles) == 0 {
		return IdleDecision{Hold: IdleHoldNoPod}
	}
	if !onConfiguredImage {
		return IdleDecision{Hold: IdleHoldImage}
	}
	for _, c := range view.Claims {
		if c.Live() && c.PluginContract != contract {
			return IdleDecision{Hold: IdleHoldPluginContract}
		}
	}
	for _, c := range view.Claims {
		if c.Live() && !idleStateReleasable(c) {
			return IdleDecision{Hold: IdleHoldInTurn}
		}
	}
	if len(view.Waiting) > 0 {
		return IdleDecision{Hold: IdleHoldDelivery}
	}
	for _, c := range view.Claims {
		if !c.Live() {
			continue
		}
		if !c.Reported() {
			return IdleDecision{Hold: IdleHoldUnreported}
		}
		if c.ActivityBackground {
			return IdleDecision{Hold: IdleHoldBackgroundWork}
		}
	}
	for _, c := range view.Claims {
		if !c.Live() || !idlePhaseOpen(view, c) {
			continue
		}
		if due := c.ActivityNextReminderAt; !due.IsZero() && !due.After(now) {
			return IdleDecision{Hold: IdleHoldReminder}
		}
	}
	since, ok := view.QuietSince()
	if !ok || now.Sub(since) < idle {
		return IdleDecision{Hold: IdleHoldNotIdle}
	}
	return IdleDecision{Release: true, Roles: roles}
}

// idleTreeLive is whether the issue and its tree are still the workflow's to run: neither the
// issue nor its root carries a status or a phase that takes it out (InWorkflow), and the root
// does not linger. An issue of its own that lingers is a member of a lingering tree, which the
// root's own linger already says.
func idleTreeLive(view store.IdleView) bool {
	issue := record.Issue{Status: view.Status, Phase: view.Phase}
	root := record.Issue{Status: view.Root.Status, Phase: view.Root.Phase}
	return InWorkflow(issue) && InWorkflow(root) && !view.Root.Lingering && !view.Lingering
}

// idleStateReleasable is whether a live claim's state is one a release may suspend: an agent that
// finished booting and is waiting for work (ready), or one that finished a turn (idle). Every
// other live state is a process in a turn, one starting, or one whose launch is not yet known.
func idleStateReleasable(c store.IdleClaim) bool {
	switch c.State {
	case supervise.StateReady, supervise.StateIdle:
		return true
	}
	return false
}

// idlePhaseOpen is whether the claim is the phase worker of the issue's current phase, serving
// the current generation: the one shape of claim a phase reminder is armed in.
func idlePhaseOpen(view store.IdleView, c store.IdleClaim) bool {
	if view.Phase == phase.Done || c.Role == claim.RoleArchitect {
		return false
	}
	return RoleFor(view.Phase) == c.Role && c.ServingGeneration == view.Generation
}
