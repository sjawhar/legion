package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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
	var quiet, holding bool
	marked, err := w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
		decision = workflow.IdleRelease(view, now, w.idlePeriod, onConfiguredImage, api.DaemonAPIVersion)
		since, found := view.QuietSince()
		quiet = found && now.Sub(since) >= w.idlePeriod
		holding = view.Holding
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
		detail := strings.Join(decision.HoldKinds, ",")
		w.log.Debug("idle release: the pod is kept", "issue", issue, "hold", decision.Hold.String(), "detail", detail)
		if !quiet || !recordableHold(decision.Hold) {
			// The pod did something inside the idle period, or what kept it is nothing a
			// deployment reads as a hold: either way no hold of it is running, so the streak it
			// had ends and a later pass that holds it starts a new one. A pod with no streak is
			// left alone — nearly every pass over a working pod is this one, and an update that
			// matches no row costs a write transaction all the same.
			if holding {
				endIdleHolds(ctx, w.idleStore, w.projectID, issue, w.log)
			}
			return
		}
		recordIdleHold(ctx, w.idleStore, w.projectID, issue, decision.Hold, detail, now, w.log)
		return
	}
	w.log.Info("idle release: releasing an issue's pod, which has had no turn in progress and no turn due for the idle period",
		"issue", issue, "idle", w.idlePeriod.String(), "roles", len(decision.Roles))
	w.wakeOutbox(issue, record.OutboxKindSupervise)
}

// recordableHold is whether a hold is one a deployment reads as "release would have taken this
// pod and something stopped it". Four of workflow's values are not:
//   - IdleHoldNone is a release, not a hold;
//   - IdleHoldNotIdle is a pod that is simply not idle yet, which is every working pod on nearly
//     every tick and says nothing about release;
//   - IdleHoldNoPod is an issue the record does not hold or one with no live claim, so there is
//     no pod to have kept (such a view is never quiet either, since the quiet period is measured
//     from the pod's own claims);
//   - IdleHoldReleasing is this daemon's own release already running on the lane, so recording it
//     would count every release again, once per tick of its teardown.
//
// The pass that reads one of them ends the pod's streaks instead of recording, which is the one
// place a streak ends outside a hold of another reason (releaseIdlePod).
//
// Every other value is a reason worth a row, the ones that are working as designed
// (background_work, in_turn, delivery_waiting, reminder_due, awaiting_person) as much as the
// ones that are not (image, plugin_contract, unreported, bundle_too_large, bundle_save_failed,
// overtaken, unauthorized): a deployment reads release's health from which of them it sees, so
// leaving the healthy ones out would leave it unable to tell a pod kept for good reason from one
// never decided at all. IdleHoldTreeNotLive is recorded too: a pod of an issue the workflow has
// taken out is one the close is stopping, and a deployment that sees it every tick is seeing a
// close that is stuck.
func recordableHold(hold workflow.IdleHold) bool {
	switch hold {
	case workflow.IdleHoldNone, workflow.IdleHoldNotIdle, workflow.IdleHoldNoPod, workflow.IdleHoldReleasing:
		return false
	}
	return true
}

// recordIdleHold records one pass of the idle rule that kept a pod past the idle period, under
// the rule's own word for the reason and the detail that word leaves out ("" where there is
// none), and ends every streak of that pod under another reason (store.RecordPodHold). Its
// caller has read recordableHold: a hold no deployment reads as a hold is a streak to end and
// not a row to write, and ending it is the caller's, which knows whether the pod has one. A
// record that cannot be written is logged and nothing else: the records are a deployment's
// measurement, and a release is a saving, so nothing waits on either.
func recordIdleHold(ctx context.Context, st *store.Store, project, issue string, hold workflow.IdleHold, detail string, at time.Time, log *slog.Logger) {
	if st == nil {
		return
	}
	if err := st.RecordPodHold(ctx, store.PodHold{Project: project, Issue: issue, Reason: hold.String(), Detail: detail, At: at}); err != nil {
		log.Warn("idle release: could not record a pod's hold", "issue", issue, "hold", hold.String(), "error", err)
	}
}

// endIdleHolds ends every hold streak of a pod, which is what a pass that reads no hold worth a
// row leaves behind.
func endIdleHolds(ctx context.Context, st *store.Store, project, issue string, log *slog.Logger) {
	if st == nil {
		return
	}
	if err := st.EndPodHoldStreaks(ctx, project, issue); err != nil {
		log.Warn("idle release: could not end a pod's hold streaks", "issue", issue, "error", err)
	}
}

// recordPodRelease writes the record of one completed release: the run it released, the roles
// whose claims it suspended as released, the moment it completed, and the size of the bundle of
// unpushed work it saved — zero when the workspace held none. It is called from the teardown
// (releaseIssue), the one place both the bundle and the fact that the release finished are known.
//
// The roles are the ones this release's own stops suspended, as its mark recorded them
// (store.ReleaseSuspendedClaim), and never every released claim of the issue: a claim an earlier
// release left `released` and no wake has brought back is still released now, and counting it
// here would charge this release with a role it never touched and the hours that role's pod was
// gone a second time.
//
// A record that cannot be written is logged: the pod is already released, and nothing undoes
// that for want of a row.
func (r *outbox) recordPodRelease(ctx context.Context, issue string, generation uint64, suspended []claim.Role, bundleBytes int) {
	if r.supervisor == nil || r.supervisor.store == nil {
		return
	}
	roles := make([]string, 0, len(suspended))
	for _, role := range suspended {
		roles = append(roles, string(role))
	}
	slices.Sort(roles)
	if _, err := r.supervisor.store.RecordPodRelease(ctx, store.PodRelease{
		Project: r.project, Issue: issue, Generation: generation, Roles: roles,
		At: time.Now().UTC(), BundleBytes: bundleBytes,
	}); err != nil {
		r.log.Warn("outbox: could not record an issue pod's release", "issue", issue, "error", err)
	}
}

// releaseHold is the hold a cancel the store decided is recorded under, and the sentence the
// log line carries. The cause is the store's answer and not the row's, so a pod taken up again
// is one reason in the records wherever the release read it: at a role stop, where the start or
// the delivery arrives, and at the teardown, where the same read runs again. The store names a
// cause on every cancel it decides, and those two are all it names.
func releaseHold(cause store.ReleaseCause) (workflow.IdleHold, string) {
	if cause == store.ReleaseCauseOvertaken {
		return workflow.IdleHoldOvertaken, "the pod was given work after its release was marked"
	}
	return workflow.IdleHoldUnauthorized, "the store no longer authorizes it"
}

// recordPodHold writes the hold of a release that did not finish (cancelRelease): the pod was
// kept, and the reason the teardown read is the same vocabulary the idle tick records. The
// teardown reads a hold and not the view behind it, so its holds carry no detail.
func (r *outbox) recordPodHold(ctx context.Context, issue string, hold workflow.IdleHold) {
	if r.supervisor == nil {
		return
	}
	recordIdleHold(ctx, r.supervisor.store, r.project, issue, hold, "", time.Now().UTC(), r.log)
}

// idleReleaseRows is the release as outbox rows, in the order the issue's lifecycle lane runs
// them — three acts, each one row a reader can name:
//
//  1. the save (record.IssueBundle): the shim of the first live role is asked for the git bundle
//     of the workspace's unpushed work, which is stored in the daemon's database. It runs first
//     because only a shim can build it and a shim dies with its role's process, which act 2
//     stops. It stops nothing itself, so a release cancelled here costs only the save;
//  2. the role stops: one `release` supervise row per live role, each stopping that role's
//     process and keeping its session. The first of them is the release's boundary — the stop of
//     the first role process of the pod — which is what R4's cancel is read against, so a
//     delivery queued while the save ran is still below it and cancels the whole release;
//  3. the teardown (record.IssueRelease): it deletes the pod, its disk and its Sandbox, and
//     records the release with the size the save stored.
func idleReleaseRows(view pods.IdleView, decision workflow.IdleDecision, now time.Time) (store.IdleReleaseMark, error) {
	mark := store.IdleReleaseMark{Generation: view.Generation, Role: decision.Roles[0]}
	rows := make([]store.IdleReleaseRow, 0, len(decision.Roles)+2)
	add := func(payload record.OutboxPayload, boundary bool) error {
		row, err := record.NewOutboxRow(view.Issue, payload, now)
		if err != nil {
			return fmt.Errorf("encode the %s of %s's release: %w", payload.OutboxKind(), view.Issue, err)
		}
		rows = append(rows, store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload, Boundary: boundary})
		return nil
	}
	if err := add(record.IssueBundle{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation, Role: mark.Role}, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	for i, role := range decision.Roles {
		if err := add(record.SuperviseRequest{Op: "release", Tree: view.Tree, Role: role,
			Generation: view.Generation, Reason: "the issue's pod has been idle for the release period"}, i == 0); err != nil {
			return store.IdleReleaseMark{}, err
		}
	}
	if err := add(record.IssueRelease{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation}, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	mark.Rows = rows
	return mark, nil
}
