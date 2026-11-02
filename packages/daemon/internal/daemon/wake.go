package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// wakeTickInterval is how often the daemon fires the backstops that came due and warns about
	// the resumes that are stuck. Both are measured in minutes, so a minute is often enough to
	// act within a tick of a due time and rare enough to cost two small queries a minute.
	wakeTickInterval = time.Minute
	// listenerReadyWindow is how fresh the Envoy listener's own `listener_ready` report must be
	// for the daemon to release anything. The listener publishes one at its start and every five
	// minutes after it, so fifteen minutes is three reports' worth of grace and still short
	// enough that a listener that stopped reporting holds release off within one idle period.
	//
	// The gate is R7's and it is one-sided on purpose: a released pod comes back for a role
	// message or an agent message only because the listener reports them, and without a positive
	// signal "the listener sends no reports" and "the listener has nothing to report" are the
	// same silence. A pod released into that silence could sit released with a message waiting
	// for it, so the daemon releases nothing until it has heard the listener.
	listenerReadyWindow = 15 * time.Minute
	// A wake that has not reached the woken role's first turn within workflow.StrandedReleaseWake
	// is the same fact two passes read: this one warns about it and marks it (R5), and the
	// stranded report names its claim (workflow.Stranded), so the bound is that rule's and is
	// not restated here.
)

// listenerLive is whether the Envoy listener has said it is alive and reporting within
// listenerReadyWindow (store.Store.ListenerLive): the one gate the idle rule reads before it
// releases anything. A daemon that has read no report at all, and one whose newest report is
// older than the window, releases nothing and says which of the two it is.
func (w *workflowRuntime) listenerLive(ctx context.Context) bool {
	if w.idleStore == nil {
		return false
	}
	live, at, found, err := w.idleStore.ListenerLive(ctx, w.projectID, time.Now(), listenerReadyWindow)
	switch {
	case err != nil:
		w.log.Warn("idle release: could not read the listener's liveness report; releasing nothing", "error", err)
	case !found:
		w.log.Info("idle release: the Envoy listener has not once reported itself ready to this daemon; releasing nothing")
	case !live:
		w.log.Warn("idle release: the Envoy listener's newest report is too old to release a pod on; a released pod would not be woken by a role or agent message",
			"reported", at.Format(time.RFC3339), "age", time.Since(at).Truncate(time.Second).String(), "window", listenerReadyWindow.String())
	}
	return live
}

// tickWakes fires the backstops that came due and warns about the resumes that are stuck, on the
// daemon's own period. It runs only where release runs: with no idle period and no store there is
// no released pod to wake and no resume to measure.
func (w *workflowRuntime) tickWakes(ctx context.Context) {
	if w.idlePeriod <= 0 || w.idleStore == nil {
		return
	}
	interval := w.idleTick
	if interval <= 0 {
		interval = wakeTickInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			w.fireDueBackstops(ctx, now)
			w.warnStuckResumes(ctx, now)
		}
	}
}

// fireDueBackstops applies one PodBackstopDue fact per backstop of the project due at or before
// now. The row is the durable half and the fact the acting half: a backstop that came due while
// the daemon was down is read by the first pass after it starts, and the row's conditional fire
// (store.FirePodBackstop) is what makes two passes that read the same row wake its pod once.
func (w *workflowRuntime) fireDueBackstops(ctx context.Context, now time.Time) {
	due, err := w.idleStore.DuePodBackstops(ctx, w.projectID, now)
	if err != nil {
		w.log.Warn("wake: could not read the backstops that came due", "error", err)
		return
	}
	for _, backstop := range due {
		if ctx.Err() != nil {
			return
		}
		fact := intake.PodBackstopDue{ID: backstop.ID, Issue: backstop.Issue, Role: backstop.Role}
		id := fmt.Sprintf("pod-backstop:%d", backstop.ID)
		if _, err := intake.ApplyFact(ctx, w.pool, "wake", id, fact, w.handlers...); err != nil {
			w.log.Warn("wake: could not fire a released pod's backstop", "issue", backstop.Issue, "role", backstop.Role, "error", err)
			continue
		}
		w.wakeOutbox(backstop.Issue, record.OutboxKindSupervise)
	}
}

// warnStuckResumes warns about each resume whose wake ran out of workflow.StrandedReleaseWake
// with the woken role still having taken no turn, and marks it, so each is warned about once.
// The warning names the issue, the role and the step the resume is waiting on, which is what an
// operator needs to tell "the pod never came back" from "the pod came back and the agent never
// took its task".
func (w *workflowRuntime) warnStuckResumes(ctx context.Context, now time.Time) {
	stuck, err := w.idleStore.StuckPodResumes(ctx, w.projectID, now.Add(-workflow.StrandedReleaseWake))
	if err != nil {
		w.log.Warn("wake: could not read the resumes with no first turn", "error", err)
		return
	}
	for _, resume := range stuck {
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("wake: a released pod's resume has had no first turn since its wake",
			"issue", resume.Issue, "role", resume.Role, "kind", resume.Kind,
			"waiting", w.stuckResumeStep(ctx, resume), "wake", resume.At.UTC().Format(time.RFC3339),
			"after", now.Sub(resume.At).Truncate(time.Second).String())
		if err := w.idleStore.MarkPodResumeStuck(ctx, resume.ID); err != nil {
			w.log.Warn("wake: could not mark a resume stuck", "issue", resume.Issue, "role", resume.Role, "error", err)
		}
	}
}

// stuckResumeStep is the step a stuck resume is waiting on, which is what its warning names: the
// replacement pod, when nothing has observed it ready, or the woken role's first turn, when the
// pod is up and the agent has taken nothing. The pod-ready stamp is read here rather than
// carried on the resume, because the read that lists the stuck resumes does not carry it.
func (w *workflowRuntime) stuckResumeStep(ctx context.Context, resume store.PodResume) string {
	ready, err := w.idleStore.PodResumeReadyAt(ctx, resume.ID)
	if err != nil {
		w.log.Warn("wake: could not read a stuck resume's pod-ready stamp", "issue", resume.Issue, "error", err)
	}
	if ready.IsZero() {
		return "the replacement pod to be ready"
	}
	return "the woken role's first turn"
}

// podResume stamps what a supervisor's durable transition says about a woken role's open resume:
// a ready claim is the replacement pod up, the first observation the daemon has of it, and a
// working claim is the woken role's first turn, which ends the resume R5 measures. A wake for a
// notice or a phase-ended message gives the role no delivery to confirm, so the turn itself —
// not a confirmation — is what says the pod came back and took its work.
//
// It is a supervisor terminal callback, so it runs on the machine's own goroutine and must not
// block it; a failed stamp is logged and the resume keeps its zero time.
func (w *workflowRuntime) podResume(c supervise.Claim, state supervise.ClaimState) {
	if w.idleStore == nil || c.Issue == "" || c.Role == claim.RoleController {
		return
	}
	mark, what := w.idleStore.MarkPodResumeReady, "ready"
	switch state {
	case supervise.StateReady:
	case supervise.StateWorking:
		mark, what = w.idleStore.MarkPodResumeFirstTurn, "first turn"
	default:
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), readyReleaseTimeout)
		defer cancel()
		w.stampPodResume(ctx, c.Issue, c.Role, mark, what)
	}()
}

// stampPodResume writes one observation onto the newest resume of issue's role that has not
// reached its first turn, when there is one. A claim that was never released has no open resume
// and nothing is written, which is the ordinary case: every ready and every turn of every claim
// passes here.
func (w *workflowRuntime) stampPodResume(ctx context.Context, issue string, role claim.Role,
	mark func(context.Context, int64, time.Time) error, what string) {
	id, found, err := w.idleStore.OpenPodResume(ctx, w.projectID, issue, role)
	if err != nil {
		w.log.Warn("wake: could not read the open resume of a role", "issue", issue, "role", role, "error", err)
		return
	}
	if !found {
		return
	}
	if err := mark(ctx, id, time.Now().UTC()); err != nil {
		w.log.Warn("wake: could not stamp a resume", "issue", issue, "role", role, "observation", what, "error", err)
	}
}

// wakeForDelivery brings a released pod back for a delivery the daemon already has for one of its
// roles and cannot hand over any other way: a notice whose owning architect is released, and a
// phase-ended message whose role is. Every other delivery is an outbox `start` or a task, which
// the supervise machine already resumes a suspended claim for (deliverResuming); these two
// publish to a live session instead, so the row's own retry is what waits for the pod, and this
// is what makes the pod come back for it to find.
//
// It answers whether the claim was released and the wake taken. key is what makes the wake one
// wake however often the row retries — the row's id — so a notice that backs off for ten minutes
// resumes the claim once. A claim of an issue that has left the workflow, or whose tree lingers,
// is not woken: that pod is a close's to stop.
func (r *outbox) wakeForDelivery(ctx context.Context, token claim.Token, key, detail string) (bool, error) {
	machine, found := r.supervisor.Machine(token)
	if !found {
		return false, nil
	}
	held := machine.Claim()
	if !held.Released() {
		return false, nil
	}
	issue, err := r.issue(ctx, held.Issue)
	if err != nil {
		return false, err
	}
	root, err := r.root(ctx, issue)
	if err != nil {
		return false, err
	}
	if root == nil || !workflow.TreeLive(issue, *root) {
		return false, nil
	}
	taken, err := r.trees.TakePodWake(ctx, r.project, issue.Key, held.Role, pods.WakeDelivery, key, r.now().UTC())
	if err != nil {
		return false, err
	}
	if !taken {
		// Another attempt of this same row already woke the pod; it is on its way back.
		return true, nil
	}
	r.log.Info("wake: a released pod is coming back for a delivery the daemon holds for one of its roles",
		"issue", issue.Key, "role", held.Role, "detail", detail)
	if err := machine.Handle(ctx, supervise.RequestRuleResume{Claim: token, Reason: "a wake for " + detail}); err != nil {
		return false, fmt.Errorf("wake claim %s for %s: %w", token, detail, err)
	}
	return true, nil
}

// armBackstops arms the backstop of a release that completed: backstop_minutes from now, for the
// worker of the pod's open phase (workflow.OpenPhaseRole). An architect gets none — nothing of
// its own is due, and a sub-architect woken for nothing would hold a pod open for the backstop
// period — and neither does an issue whose phase no role works.
//
// It also forgets the wake marks of the release before this one: their keys are spent, and a head
// that settled before the pod was released last must be able to wake it again now.
func (r *outbox) armBackstops(ctx context.Context, issue record.Issue) error {
	if err := r.trees.ForgetPodWakesOf(ctx, r.project, issue.Key); err != nil {
		return err
	}
	if r.backstop <= 0 {
		return nil
	}
	role := workflow.OpenPhaseRole(issue.Phase)
	if role == "" {
		return nil
	}
	released, err := r.trees.ReleasedClaimsIn(ctx, r.project, issue.Key)
	if err != nil {
		return err
	}
	for _, c := range released {
		if c.Role != role || c.ServingGeneration != issue.Generation {
			continue
		}
		due := r.now().UTC().Add(r.backstop)
		r.log.Info("wake: the release of an issue's pod arms its open phase's backstop",
			"issue", issue.Key, "role", role, "due", due.Format(time.RFC3339))
		return r.trees.ArmPodBackstop(ctx, store.PodWakeBackstop{Project: r.project, Issue: issue.Key,
			Role: role, DueAt: due})
	}
	return nil
}

// takeOverRelease is what every path that suspends, stops or closes a claim does to a pod that
// was released: it drops the wake marks and the open backstops of the issue, so no later wake
// acts on a claim this path has taken over. The claim's own suspension source is re-stamped by
// the machine (supervise's suspendedAgain), which is what takes it out of every released-claim
// read; this drops what is left behind in the record.
func (r *outbox) takeOverRelease(ctx context.Context, issue string) {
	if err := r.trees.ForgetPodWakesOf(ctx, r.project, issue); err != nil {
		r.log.Warn("wake: could not forget the wakes of a pod another path took over", "issue", issue, "error", err)
	}
}

// dropReleasedWork takes a released pod's issue over and deletes the workspace bundle its release
// saved: the issue is done, or its tree is closing, so nothing will restore that bundle and
// nothing should. A linger and a park keep theirs, because their issue comes back.
func (r *outbox) dropReleasedWork(ctx context.Context, issue string) {
	r.takeOverRelease(ctx, issue)
	if err := r.trees.DeleteWorkspaceBundle(ctx, issue); err != nil {
		r.log.Warn("wake: could not delete the workspace bundle of a finished issue", "issue", issue, "error", err)
	}
}
