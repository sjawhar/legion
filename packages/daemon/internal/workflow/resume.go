package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// booted is the daemon's boot (intake.DaemonBooted): every claim of this project that owns its
// issue's current phase and that the daemon or its own exit suspended (supervise's ByRule) is
// started again. Nothing else would start it: the phase's start ran before the restart, a
// re-admission's starts are queued only when a tree is re-admitted, and the next start is the next
// phase's. This is the one trigger main did not have, so it acts on no other source: an operator's
// suspension holds until the operator resumes the claim, and a suspension recorded before the
// source existed (unknown) may be a drain made before the upgrade, so both are left as they are.
//
// The phase's owner is the role that works the issue's phase (RoleFor) on an issue in the workflow
// whose tree is admitted — its root holds a slot — and does not linger: a lingering tree's claims
// stay suspended until re-admission runs the tree again. Each owner is given the start admission
// gives a re-admitted tree's mid-phase child, through the outbox's start like every other start:
// its claim resumed by rule, and the task that carries its phase on (ResumePhaseTask, the merger's
// with the head its round approved), which the outbox does not deliver to a claim still holding its
// phase's task. A role a start already covers (StartFor) gets no second.
func (e *Engine) booted(ctx context.Context, tx pgx.Tx, _ intake.DaemonBooted) (intake.Result, error) {
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
		if issue.Project != e.cfg.Project || role == "" || record.OutOfWorkflow(issue.Status) {
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
