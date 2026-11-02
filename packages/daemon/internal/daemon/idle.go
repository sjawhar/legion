package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleTickInterval is how often the daemon decides every pod of its project. The shortest idle
// period an operator would set is minutes, and one read per issue is three small queries, so a
// minute is often enough to release a pod within a tick of its period passing and rare enough to
// cost nothing.
const idleTickInterval = time.Minute

// issuePodImages is the runtime's answer to "is this issue's pod on the image the configuration
// names?" (sandbox.Runtime.IssuePodRunsConfiguredImage). A runtime that runs no pods implements
// none, and the daemon then releases nothing, which is also what `runtime: tmux` wants.
type issuePodImages interface {
	IssuePodRunsConfiguredImage(issue string) bool
}

// tickIdle decides every pod of the project on the daemon's own period (idleTickInterval), and
// releases each one the rule says has had no turn in progress and no turn due for the idle
// period. It returns at once — running no ticker at all — when release is off: `idle_minutes: 0`,
// or a runtime that is not Kubernetes, which has no pod to release and no image to compare.
func (w *workflowRuntime) tickIdle(ctx context.Context) {
	if w.idlePeriod <= 0 || w.idleStore == nil || w.idleImages == nil {
		return
	}
	interval := w.idleTick
	if interval <= 0 {
		interval = idleTickInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.releaseIdlePods(ctx)
		}
	}
}

// releaseIdlePods decides each pod of the project in turn. One issue's failure is logged and the
// rest are decided: a release is a saving, never a correctness step, and nothing waits on it.
func (w *workflowRuntime) releaseIdlePods(ctx context.Context) {
	if !w.listenerLive(ctx) {
		return
	}
	issues, err := w.idleStore.IdlePods(ctx, w.projectID)
	if err != nil {
		w.log.Warn("idle release: could not list the project's pods", "error", err)
		return
	}
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		w.releaseIdlePod(ctx, issue)
	}
}

// releaseIdlePod decides one issue's pod and, when the rule releases it, writes the release's
// rows and its mark in one transaction with the view they rest on (store.ReleaseIdleIssue). The
// pod's image is read before that transaction, from the runtime's own store of pods, because it
// is the one input the daemon's Postgres does not hold.
//
// A pod the rule kept that was quiet for the whole idle period is recorded as a hold
// (recordIdleHold): that is a pod release would have taken and something else stopped, which is
// what tells a deployment whether release is working. The release itself is recorded where it
// completes, since that is where the bundle it saved is known (outbox.releaseIssue, through
// recordPodRelease).
func (w *workflowRuntime) releaseIdlePod(ctx context.Context, issue string) {
	now := time.Now().UTC()
	onConfiguredImage := w.idleImages.IssuePodRunsConfiguredImage(issue)
	var decision workflow.IdleDecision
	var quiet bool
	marked, err := w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view store.IdleView) (store.IdleReleaseMark, error) {
		decision = workflow.IdleRelease(view, now, w.idlePeriod, onConfiguredImage, api.DaemonAPIVersion)
		since, found := view.QuietSince()
		quiet = found && now.Sub(since) >= w.idlePeriod
		if !decision.Release {
			return store.IdleReleaseMark{}, nil
		}
		return idleReleaseRows(view, decision, now)
	})
	if err != nil {
		w.log.Warn("idle release: could not decide an issue's pod", "issue", issue, "error", err)
		return
	}
	if !marked {
		w.log.Debug("idle release: the pod is kept", "issue", issue, "hold", decision.Hold.String())
		if quiet {
			recordIdleHold(ctx, w.idleStore, w.projectID, issue, decision.Hold, now, w.log)
		}
		return
	}
	w.log.Info("idle release: releasing an issue's pod, which has had no turn in progress and no turn due for the idle period",
		"issue", issue, "idle", w.idlePeriod.String(), "roles", len(decision.Roles))
	w.wakeOutbox(issue, record.OutboxKindSupervise)
}

// recordableHold is whether a hold is one a deployment reads as "release would have taken this
// pod and something stopped it". Four of workflow's values are not:
//
//   - IdleHoldNone is a release, not a hold;
//   - IdleHoldNotIdle is a pod that is simply not idle yet, which is every working pod on nearly
//     every tick and says nothing about release;
//   - IdleHoldNoPod is an issue the record does not hold or one with no live claim, so there is
//     no pod to have kept (such a view is never quiet either, since the quiet period is measured
//     from the pod's own claims);
//   - IdleHoldReleasing is this daemon's own release already running on the lane, so recording it
//     would count every release again, once per tick of its teardown.
//
// Every other value is a reason worth a row, the ones that are working as designed
// (background_work, in_turn, delivery_waiting, reminder_due, awaiting_person) as much as the ones
// that are not (image, plugin_contract, unreported, bundle_too_large, bundle_save_failed): a
// deployment reads release's health from which of them it sees, so leaving the healthy ones out
// would leave it unable to tell a pod kept for good reason from one never decided at all.
// IdleHoldTreeNotLive is recorded too: a pod of an issue the workflow has taken out is one the
// close is stopping, and a deployment that sees it every tick is seeing a close that is stuck.
func recordableHold(hold workflow.IdleHold) bool {
	switch hold {
	case workflow.IdleHoldNone, workflow.IdleHoldNotIdle, workflow.IdleHoldNoPod, workflow.IdleHoldReleasing:
		return false
	}
	return true
}

// recordIdleHold writes one pass of the idle rule that kept a pod past the idle period. A hold
// that cannot be written is logged and nothing else: the records are a deployment's measurement,
// and a release is a saving, so nothing waits on either.
func recordIdleHold(ctx context.Context, st *store.Store, project, issue string, hold workflow.IdleHold, at time.Time, log *slog.Logger) {
	if st == nil || !recordableHold(hold) {
		return
	}
	if err := st.RecordPodHold(ctx, store.PodHold{Project: project, Issue: issue, Reason: hold.String(), At: at}); err != nil {
		log.Warn("idle release: could not record a pod's hold", "issue", issue, "hold", hold.String(), "error", err)
	}
}

// recordPodRelease writes the record of one completed release: the run it released, the roles
// whose claims it suspended as released, the moment it completed, and the size of the bundle of
// unpushed work it saved — zero when the workspace held none. It is called from the teardown
// (releaseIssue), the one place both the bundle and the fact that the release finished are known.
// A record that cannot be written is logged: the pod is already released, and nothing undoes that
// for want of a row.
func (r *outbox) recordPodRelease(ctx context.Context, issue string, generation uint64, bundleBytes int) {
	if r.supervisor == nil || r.supervisor.store == nil {
		return
	}
	var roles []string
	for _, machine := range r.supervisor.supervised() {
		c := machine.Claim()
		if c.Issue == issue && c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByReleased {
			roles = append(roles, string(c.Role))
		}
	}
	slices.Sort(roles)
	if _, err := r.supervisor.store.RecordPodRelease(ctx, store.PodRelease{
		Project: r.project, Issue: issue, Generation: generation, Roles: roles,
		At: time.Now().UTC(), BundleBytes: bundleBytes,
	}); err != nil {
		r.log.Warn("outbox: could not record an issue pod's release", "issue", issue, "error", err)
	}
}

// recordPodHold writes the hold of a release that did not finish (cancelRelease): the pod was
// kept, and the reason the teardown read is the same vocabulary the idle tick records.
func (r *outbox) recordPodHold(ctx context.Context, issue string, hold workflow.IdleHold) {
	if r.supervisor == nil {
		return
	}
	recordIdleHold(ctx, r.supervisor.store, r.project, issue, hold, time.Now().UTC(), r.log)
}

// idleReleaseRows is the release as outbox rows, in the order the issue's lifecycle lane runs
// them: one `release` supervise row per live role, which stops that role's process and keeps its
// session, and then the teardown, which saves the workspace's unpushed work and deletes the pod,
// its disk and its Sandbox. The first row is the release's boundary — the stop of the first role
// process of the pod — and the teardown asks that same role's launcher for the bundle, a process
// that lives as long as the pod does.
func idleReleaseRows(view store.IdleView, decision workflow.IdleDecision, now time.Time) (store.IdleReleaseMark, error) {
	mark := store.IdleReleaseMark{Generation: view.Generation, Role: decision.Roles[0]}
	payloads := make([]record.OutboxPayload, 0, len(decision.Roles)+1)
	for _, role := range decision.Roles {
		payloads = append(payloads, record.SuperviseRequest{Op: "release", Tree: view.Tree, Role: role,
			Generation: view.Generation, Reason: "the issue's pod has been idle for the release period"})
	}
	payloads = append(payloads, record.IssueRelease{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation, Role: mark.Role})
	for _, payload := range payloads {
		row, err := record.NewOutboxRow(view.Issue, payload, now)
		if err != nil {
			return store.IdleReleaseMark{}, fmt.Errorf("encode the %s of %s's release: %w", payload.OutboxKind(), view.Issue, err)
		}
		mark.Rows = append(mark.Rows, store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload})
	}
	return mark, nil
}
