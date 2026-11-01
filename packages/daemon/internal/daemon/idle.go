package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
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
func (w *workflowRuntime) releaseIdlePod(ctx context.Context, issue string) {
	now := time.Now().UTC()
	onConfiguredImage := w.idleImages.IssuePodRunsConfiguredImage(issue)
	var decision workflow.IdleDecision
	marked, err := w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view store.IdleView) (store.IdleReleaseMark, error) {
		decision = workflow.IdleRelease(view, now, w.idlePeriod, onConfiguredImage, api.DaemonAPIVersion)
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
		return
	}
	w.log.Info("idle release: releasing an issue's pod, which has had no turn in progress and no turn due for the idle period",
		"issue", issue, "idle", w.idlePeriod.String(), "roles", len(decision.Roles))
	w.wakeOutbox(issue, record.OutboxKindSupervise)
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
