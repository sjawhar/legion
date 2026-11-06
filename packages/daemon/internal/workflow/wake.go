package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A wake brings a released pod back. The release suspended every claim of the pod with the source
// `released` and deleted the pod, its disk and its Sandbox; a wake resumes one of those claims,
// which relaunches its session in a fresh pod whose launcher restores the workspace bundle the
// release saved. Five things wake a pod, and this file holds four of them: the CI settlement, the
// role message and the agent message the Envoy listener reports, and the stored backstop. The
// fifth — a delivery the daemon already had for a role of the pod — needs nothing here, because a
// delivery to a suspended claim already resumes it (supervise's deliverResuming) and the delivery
// is its own account of why the pod came back.
//
// Every wake acts only on a claim that is still `released`, of an issue still in the workflow
// whose tree is live, and a role comes back once while its pod is on its way: the role's one open
// resume is what says so, so one head that settles again as each of its checks finishes wakes the
// pod once.

// Pods is the daemon's record of its released pods as the workflow reads and writes it: the Envoy
// listener's liveness report, the claims a release left released, the wake that brings one back,
// and the backstop a wake fires. The daemon hands one in (Config.Pods, the store's own
// PodRecordsOf), which holds the project as the claim-keyed tables spell it, so the engine carries
// one project and reaches Postgres through an interface here as it does through record.Store.
//
// A wake is taken whole by TakePodWake: the resume recorded against the release the wake ends,
// which the role's one open resume refuses a second of, and the woken role's backstop cancelled.
// The store owns that order, and this package never writes a part of it.
type Pods interface {
	// RecordListenerReady stamps when the Envoy listener last said it is alive and reporting,
	// which is the whole of R7's gate.
	RecordListenerReady(ctx context.Context, tx pgx.Tx, at time.Time) error
	// ReleasedClaimOfRole is the released claim of issue's role, and whether there is one.
	ReleasedClaimOfRole(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (supervise.Claim, bool, error)
	// ReleasedClaimByToken and ReleasedClaimBySession are the released claim a listener's report
	// names, by the role token a message was published to and by the session one was sent to.
	// Every daemon reads the report and only the one holding a released claim of that name acts.
	ReleasedClaimByToken(ctx context.Context, tx pgx.Tx, token string) (supervise.Claim, bool, error)
	ReleasedClaimBySession(ctx context.Context, tx pgx.Tx, session string) (supervise.Claim, bool, error)
	// TakePodWake takes one wake of issue's role and answers whether this call took it; false is
	// a wake another pass already took, which is already bringing the pod back.
	TakePodWake(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, kind string, at time.Time) (bool, error)
	// FirePodBackstop marks the backstop fired and answers whether this call fired it.
	FirePodBackstop(ctx context.Context, tx pgx.Tx, id int64) (bool, error)
}

// wakeRow is the outbox row a wake writes: a `start` of the woken role at the issue's current
// generation, naming no phase, carrying the task that says why the pod came back. It is the same
// row a phase start writes, which is the point — the outbox's start executor already resumes a
// suspended claim by rule (RequestRuleResume), already records the start that cancels a release
// still in flight (StartedBy, read by store.IssueReleaseStop), and already delivers the row's
// task once the relaunched agent is ready. Naming no phase keeps the row out of every phase
// fence: the issue's phase may move between the wake and the relaunch, and the wake's task is
// never the phase's work.
func wakeRow(issue record.Issue, role claim.Role, task string) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Generation: issue.Generation, Task: task}
}

// WakeTask is the one task a woken role is given, the sentence its first turn reads before
// anything else: which wake brought its pod back and what that wake was about. R6 asks for
// exactly one such task or message per wake, and the outbox row that carries it is written in the
// wake's own transaction, so a wake that commits has its task and one that does not has neither.
func WakeTask(kind, detail string) string {
	const preamble = "Your issue's pod had been released while it was idle, and this wake brought it back. " +
		"Your session, your workspace and any work you had not pushed are as you left them. "
	switch kind {
	case pods.WakeChecks:
		return preamble + "The wake is a CI settlement on your pull request's head: " + detail +
			". Read the checks before you decide anything."
	case pods.WakeRoleMessage:
		return preamble + "The wake is a message somebody published to your role topic (" + detail +
			") while your pod was gone, which nothing kept: the sender's next attempt reaches you now."
	case pods.WakeAgentMessage:
		return preamble + "The wake is a message somebody sent to your session (" + detail +
			") while your pod was gone, which nothing kept: the sender's next attempt reaches you now."
	case pods.WakeBackstop:
		return preamble + "The wake is the release's backstop running out: " + detail +
			". Nothing else asked for you, so take up your phase where you left it."
	default:
		return preamble + "The wake is " + kind + ": " + detail + "."
	}
}

// listenerWake applies one wake report of the Envoy listener (intake.ListenerWake). A
// `listener_ready` report is the positive liveness signal R7's gate reads and wakes nothing. A
// role or agent report names a role token or a session, and this daemon acts only when one of its
// own claims is released under that name: every daemon consumes the subject, because the listener
// does not know which daemon owns a session and must not learn.
func (e *Engine) listenerWake(ctx context.Context, tx pgx.Tx, fact intake.ListenerWake) (intake.Result, error) {
	if e.cfg.Pods == nil {
		return intake.Result{}, nil
	}
	if fact.Kind == intake.ListenerWakeReady {
		return intake.Result{}, e.cfg.Pods.RecordListenerReady(ctx, tx, fact.At)
	}
	var released supervise.Claim
	var found bool
	var err error
	kind, detail := pods.WakeRoleMessage, fact.Role
	if fact.Kind == intake.ListenerWakeAgent {
		kind, detail = pods.WakeAgentMessage, fact.Session
		released, found, err = e.cfg.Pods.ReleasedClaimBySession(ctx, tx, fact.Session)
	} else {
		released, found, err = e.cfg.Pods.ReleasedClaimByToken(ctx, tx, fact.Role)
	}
	if err != nil || !found {
		return intake.Result{}, err
	}
	_, err = e.wakeReleased(ctx, tx, released, kind, detail, fact.At)
	return intake.Result{}, err
}

// wakeOnSettlement wakes a released pod on a CI settlement the daemon records for the head of its
// issue's pull request, which is R2. It runs before the settlement is applied, so the woken role
// is told of the settlement the same transaction records.
//
// Only the worker of the pod's open phase is woken: the settlement is that worker's business and
// no one else's, and an architect reads it from its own notices. A head that settles again as each
// of its checks finishes wakes the pod no second time, because the role's resume is still open.
func (e *Engine) wakeOnSettlement(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks) (claim.Role, error) {
	if e.cfg.Pods == nil || fact.HeadSHA == "" {
		return "", nil
	}
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return "", err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return "", err
	}
	role := OpenPhaseRole(issue.Phase)
	if role == "" {
		return "", nil
	}
	released, found, err := e.cfg.Pods.ReleasedClaimOfRole(ctx, tx, issue.Key, role)
	if err != nil || !found || released.ServingGeneration != issue.Generation {
		return "", err
	}
	// The role is the settlement's to tell only when this call is what woke it: a wake another
	// pass took is already bringing the pod back with a task of its own, and a claim this
	// settlement did not wake is told as any live role is (tellChecks).
	woken, err := e.wakeReleased(ctx, tx, released, pods.WakeChecks, settlementDetail(fact), e.now())
	if err != nil || !woken {
		return "", err
	}
	return role, nil
}

// settlementDetail names the head and what its settlement came to, which is what the woken role's
// task carries: the checks the settlement names failed, then the ones it names cancelled, and
// "every check passed" when it names neither.
func settlementDetail(fact intake.PullRequestChecks) string {
	head := fact.HeadSHA
	if len(head) > 12 {
		head = head[:12]
	}
	switch {
	case len(fact.Failing) > 0:
		return head + " failed " + strings.Join(fact.Failing, ", ")
	case len(fact.Cancelled) > 0:
		return head + " had " + strings.Join(fact.Cancelled, ", ") + " cancelled"
	default:
		return head + " passed every check"
	}
}

// podBackstopDue wakes a released pod on its backstop, which is R5: backstop_minutes after the
// release, whatever else did or did not happen. The row is fired under a conditional update, so a
// backstop two passes read fires once, and the row survives a restart, so one that came due while
// the daemon was down fires at the first pass after it starts.
func (e *Engine) podBackstopDue(ctx context.Context, tx pgx.Tx, fact intake.PodBackstopDue) (intake.Result, error) {
	if e.cfg.Pods == nil {
		return intake.Result{}, nil
	}
	fired, err := e.cfg.Pods.FirePodBackstop(ctx, tx, fact.ID)
	if err != nil || !fired {
		return intake.Result{}, err
	}
	released, found, err := e.cfg.Pods.ReleasedClaimOfRole(ctx, tx, fact.Issue, fact.Role)
	if err != nil || !found {
		return intake.Result{}, err
	}
	detail := fmt.Sprintf("the backstop armed for %s's %s came due", fact.Issue, fact.Role)
	_, err = e.wakeReleased(ctx, tx, released, pods.WakeBackstop, detail, e.now())
	return intake.Result{}, err
}

// wakeReleased is the one act every wake of this file performs, and the one place the gates are
// read: the claim is still released, and its tree is live — neither the issue nor its root has
// left the workflow and neither lingers (pods.InWorkflowNotLingering). A pod in any other state
// is a close's to stop or an operator's to resume, never a wake's.
//
// A wake of a role whose resume is already open wakes nothing a second time: that pod is coming
// back, and the start row of the wake that took it carries the task. A wake that takes it records
// itself against the release it ends and cancels the woken role's backstop (Pods.TakePodWake), and
// writes the one outbox row that resumes the role and gives it the task naming the wake. Both are
// this transaction's, so a wake this daemon took is a wake whose start row is queued: a take that
// committed without one is what wedges a role, which is why the outbox's own delivery wake, which
// resumes a claim in process instead, heals a refused wake (wakeForDelivery).
func (e *Engine) wakeReleased(ctx context.Context, tx pgx.Tx, released supervise.Claim, kind, detail string, at time.Time) (bool, error) {
	issue, err := e.store.Issue(ctx, tx, released.Issue)
	if err != nil || issue == nil {
		return false, err
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || root == nil {
		return false, err
	}
	if !pods.InWorkflowNotLingering(issue.Standing(), root.Standing(), record.OutOfWorkflow) {
		return false, nil
	}
	taken, err := e.cfg.Pods.TakePodWake(ctx, tx, issue.Key, released.Role, kind, at)
	if err != nil || !taken {
		return false, err
	}
	e.log.Info("wake: a released pod is coming back", "issue", issue.Key, "role", released.Role, "kind", kind, "detail", detail)
	return true, e.enqueue(ctx, tx, issue.Key, wakeRow(*issue, released.Role, WakeTask(kind, detail)))
}

// OpenPhaseRole is the phase worker of an open phase, and "" where there is none: a phase no role
// works, and the architect, whose phases (accepting, and every phase of a tree root the table
// gives no worker) are the architect's own turn rather than a worker's. It is the one statement of
// "the worker of the open phase" the release reads: which role a release arms a backstop for —
// an architect gets none, because nothing of its own is due and a sub-architect woken for nothing
// would hold a pod open for the backstop period — which role a CI settlement wakes, and which
// live claim of a pod has a phase reminder armed in it.
func OpenPhaseRole(p phase.Phase) claim.Role {
	if role := RoleFor(p); role != claim.RoleArchitect {
		return role
	}
	return ""
}
