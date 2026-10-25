package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// booted is the daemon's boot (intake.DaemonBooted). It does three things, in this order.
//
// First, the upgrade's sweep (sweepMerged): an issue of this project that awaits its merge on a
// pull request already merged is one the migration moved out of production_check, and the merge
// that would have closed it will never come again, so the boot closes it as the merge would have
// (closeMerged) — unless it is a root with a child still in the workflow, which stays where it is
// and closes when its last child leaves (leave's closeAwaitingRoot), so the deploy suspends no
// running child under a mass of lingers. The check such a close opens has no merge commit, which
// its first compare fills from GitHub.
//
// Second, a release build's boot compares its own build, and the build Dispatch reported to it,
// against the open checks of the repository the daemon is built from (compareBuilds): one
// check_compare row per check, whose GitHub read wakes the check when the build carries its
// merge. A boot with no revision — every non-release build — compares nothing.
//
// Third, the resume by rule: every claim of this project that owns its issue's current phase and
// that the daemon or its own exit suspended (supervise's ByRule) is started again. Nothing else
// would start it: the phase's start ran before the restart, a re-admission's starts are queued
// only when a tree is re-admitted, and the next start is the next phase's. This is the one trigger
// main did not have, so it acts on no other source: an operator's suspension holds until the
// operator resumes the claim, and a suspension recorded before the source existed (unknown) may be
// a drain made before the upgrade, so both are left as they are.
//
// The phase's owner is the role that works the issue's phase (RoleFor) on an issue in the workflow
// whose tree is admitted — its root holds a slot — and does not linger: a lingering tree's claims
// stay suspended until re-admission runs the tree again. Accepting's owner is the tree's
// architect, which admission starts with the tree, so no issue there is given one. Each owner is
// given the start admission gives a re-admitted tree's mid-phase child, through the outbox's start
// like every other start: its claim resumed by rule, and the task that carries its phase on
// (ResumePhaseTask, the merger's with the head its round approved), which the outbox does not
// deliver to a claim still holding its phase's task. A role a start already covers (StartFor) gets
// no second. The sweep runs first so the resume reads the trees it closed as lingering.
func (e *Engine) booted(ctx context.Context, tx pgx.Tx, fact intake.DaemonBooted) (intake.Result, error) {
	if err := e.sweepMerged(ctx, tx); err != nil {
		return intake.Result{}, err
	}
	if fact.Revision != "" {
		if err := e.compareBuilds(ctx, tx, wakeDaemonBoot, fact.Revision); err != nil {
			return intake.Result{}, err
		}
		// Dispatch and the daemon are built from one repository, so a deploy that ships them
		// together reports one commit twice: the daemon's own compare already covers it.
		if fact.DispatchBuild != fact.Revision {
			if err := e.compareBuilds(ctx, tx, wakeDispatchBuild, fact.DispatchBuild); err != nil {
				return intake.Result{}, err
			}
		}
	}
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return intake.Result{}, err
	}
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return intake.Result{}, err
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	slotted := make(map[string]bool, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = true
	}
	for _, issue := range issues {
		role := RoleFor(issue.Phase)
		if issue.Project != e.cfg.Project || role == "" || role == claim.RoleArchitect || record.OutOfWorkflow(issue.Status) {
			continue
		}
		root, ok := byKey[issue.Tree]
		if !ok || !slotted[root.Key] || root.Lingers() {
			continue
		}
		project, err := claim.ProjectToken(issue.Project)
		if err != nil {
			return intake.Result{}, err
		}
		token, err := claim.NewToken(project, issue.Key, role)
		if err != nil {
			return intake.Result{}, err
		}
		run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
		if err != nil {
			return intake.Result{}, err
		}
		held := run.Claim
		if held == nil || held.State != supervise.StateSuspended || !held.SuspendedBy.ByRule() ||
			!StartFor(run, root, issue.Generation, issue.Phase) {
			continue
		}
		task, err := ResumeTask(ctx, e.store, tx, issue, role)
		if err != nil {
			return intake.Result{}, err
		}
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Generation: issue.Generation,
			Phase: issue.Phase, Task: task, ResumeTask: true, RuleResume: true}); err != nil {
			return intake.Result{}, err
		}
		e.logOnCommit(ctx, "workflow: the boot starts a suspended claim that owns its issue's phase", "issue", issue.Key, "role", role,
			"phase", issue.Phase, "suspendedBy", held.SuspendedBy)
	}
	return intake.Result{}, nil
}

// sweepMerged closes every issue of the project that awaits a merge GitHub already reported
// (booted). Each is re-read before its close: a child's close may have closed its root
// (closeAwaitingRoot), and a root's close lingers the tree its children then close under. A root
// whose tree still runs is left where it stands by closeMerged, which owns that rule, and the
// sweep tells nobody of the wait: a restart must not repeat the architect's notice, and a root can
// now wait for days across many boots.
func (e *Engine) sweepMerged(ctx context.Context, tx pgx.Tx) error {
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return err
	}
	for _, listed := range issues {
		if listed.Project != e.cfg.Project || !awaitsMerge(listed) {
			continue
		}
		pr, err := e.store.PullRequest(ctx, tx, listed.Key)
		if err != nil || pr == nil || pr.State != record.PullRequestMerged {
			if err != nil {
				return err
			}
			continue
		}
		issue, err := e.store.Issue(ctx, tx, listed.Key)
		if err != nil || issue == nil || !awaitsMerge(*issue) {
			if err != nil {
				return err
			}
			continue
		}
		waiting, err := e.closeMerged(ctx, tx, *issue, pr)
		if err != nil {
			return err
		}
		if len(waiting) > 0 {
			continue
		}
		e.logOnCommit(ctx, "workflow: the boot closes an issue whose pull request merged before the upgrade", "issue", issue.Key,
			"tree", issue.Tree, "pullRequest", pr.Number, "phase", issue.Phase)
	}
	return nil
}
