package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// leave is an issue leaving the workflow for status (done, backlog, icebox, or triage). A root
// takes its tree with it into linger, and a root closed as done on a merged pull request then
// opens its production check (openCheckIfMerged), behind the linger's rows: the root's own
// IssueSuspend finishes without acting on the check's start (store.IssueSuspension's restarted
// fence), and the start resumes the implementer in the still-live pod. A child ends only itself:
// it leaves the table, its phase parked in done, so no transition or status write follows the
// human's move; the tree's architect is told, and decides what the rest of its tree does, as the
// shipped daemon routes a child's close to the architect. A later todo re-enters the child. What
// becomes of the child's claims and its pod is the status's: a child parked in backlog, icebox or
// triage has every claim suspended and its Sandbox kept, sessions and volume with it, until it runs
// again or its tree closes; a child closed as done with no merged pull request (close_root, a
// human's done) is finished, so each of its claims is closed (issue_close: retired, its session
// dropped) and its Sandbox released with the volume it owns (record.IssueSuspend's Release), while
// its parent keeps running on a volume of its own. A child closed as done at its merge still owes
// its production check: five of its claims are closed, the implementer's is not, no IssueSuspend
// is written, and the check opens in the implementer's live session; the check's close writes the
// release (closeCheck). A done child set back to todo starts fresh. An issue leaving disarms its
// open pull request (disarmOpen), as a root's linger disarms every member's.
//
// A member's leave may be the last its root waited for: a merged root stands in awaiting_merge
// while any other issue of its tree is in the workflow (closeMerged), and closes once the last of
// them leaves (closeAwaitingRoot). Every way a member stops holding it runs this function — a
// status moving to backlog, icebox, triage or done (dispatchIssue), a phase reaching done
// (transition), the architect's close_root, and LeaveUnslotted — and the boot's sweep is the
// backstop. The child's check opens after that close, so its start row follows the linger's
// suspends and resumes the implementer as a root's own check does, rather than being stopped by
// them.
func (e *Engine) leave(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	e.logOnCommit(ctx, "workflow: issue left the workflow", "issue", issue.Key, "tree", issue.Tree, "status", status)
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		if issue.Lingers() {
			return nil
		}
		if held, err := e.operatorHoldsTree(ctx, tx, issue); err != nil || held {
			if err != nil {
				return err
			}
			return e.leaveUnderOperator(ctx, tx, issue, status)
		}
		if err := e.beginLinger(ctx, tx, issue); err != nil {
			return err
		}
		if status != "done" {
			return nil
		}
		return e.openCheckIfMerged(ctx, tx, issue, false)
	}
	if issue.Phase != phase.Done {
		issue.Phase, issue.Hold, issue.ReadyPendingVersion = phase.Done, nil, nil
		if err := e.store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
	}
	if err := e.disarmOpen(ctx, tx, issue.Key); err != nil {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	merged := status == "done" && pr != nil && pr.State == record.PullRequestMerged
	op, release, except := record.SuperviseOp("suspend"), false, claim.Role("")
	if status == "done" {
		op, release = "issue_close", true
	}
	if merged {
		except = claim.RoleImplementer
	}
	if err := e.everyClaim(ctx, tx, issue, op, fmt.Sprintf("%s is %s", issue.Key, status), except); err != nil {
		return err
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil {
		return err
	}
	if root == nil {
		return fmt.Errorf("suspend issue %s: tree root %s is not recorded", issue.Key, issue.Tree)
	}
	if !merged {
		if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{
			Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation, Release: release,
		}); err != nil {
			return err
		}
	}
	kind := record.NoticeKind("child-status")
	if status == "done" {
		kind = "child-closed"
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: kind, Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is %s", issue.Key, status)}); err != nil {
		return err
	}
	if err := e.closeAwaitingRoot(ctx, tx, *root); err != nil {
		return err
	}
	if !merged {
		return nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return err
	}
	return e.openCheck(ctx, tx, issue, *pr, lingers)
}

// operatorHoldsTree says whether the operator opened the lifetime of root's tree (an operator root
// spawn under the root's key) and its cleanup has not confirmed. Only a root waiting for its slot
// can meet one: promotion waits on the operator's lifetime (treelifecycle.ErrAuthorityHeld) and
// never opens a workflow tree under it.
func (e *Engine) operatorHoldsTree(ctx context.Context, tx pgx.Tx, root record.Issue) (bool, error) {
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		return false, err
	}
	lifecycle, found, err := e.store.TreeLifecycle(ctx, tx, project, root.Tree)
	if err != nil {
		return false, err
	}
	return found && lifecycle.HeldByOperator(), nil
}

// leaveUnderOperator takes a waiting root out of the workflow while the operator runs a tree under
// its key: its record's phase is done, so todo with the label re-admits it as a closed root's does,
// and nothing else is written. Its claims, its pod and its tree's cleanup are the operator's, which
// share the root's tokens and names, so no suspend, IssueSuspend, linger close or disarm is queued
// for the workflow's record, and the workflow never ran a phase of it to check in production.
func (e *Engine) leaveUnderOperator(ctx context.Context, tx pgx.Tx, root record.Issue, status string) error {
	e.logOnCommit(ctx, "workflow: a waiting root left the workflow while the operator runs a tree under its key; the operator's tree is left alone",
		"issue", root.Key, "tree", root.Tree, "status", status)
	root.Phase, root.Hold, root.Awaiting, root.ReadyPendingVersion = phase.Done, nil, nil, nil
	return e.store.PutIssue(ctx, tx, root)
}

// LeaveUnslotted takes out of the workflow a root admission found in it with neither a slot nor a
// place in the waiting line (admit's releaseUnslotted), as a status leaving todo takes a waiting root
// out (leave, at the status the root stands at): its phase is done and its tree lingers, so nothing
// starts for it, and a todo with the label re-admits it as a lingering root's does. That status is
// the move, so no status write the daemon queued before it lands over it, as for any move out of
// the workflow (dispatchIssue).
func (e *Engine) LeaveUnslotted(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if err := e.store.DropStatusWrites(ctx, tx, root.Key); err != nil {
		return err
	}
	return e.leave(ctx, tx, root, root.Status)
}

// openCheckIfMerged opens issue's production check when its pull request has merged: a done issue
// with no merged pull request — a root closed before its first phase (close_root), a human's done
// on an issue short of its merge — has nothing to check in production and opens none.
func (e *Engine) openCheckIfMerged(ctx context.Context, tx pgx.Tx, issue record.Issue, wake bool) error {
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil || pr.State != record.PullRequestMerged {
		return err
	}
	return e.openCheck(ctx, tx, issue, *pr, wake)
}

// closeAwaitingRoot closes root when its merge was waiting on the rest of its tree: a merged root
// stands in awaiting_merge (or held from it) while any other member of its tree is in the workflow
// (closeMerged), and closes, as its merge would have, once the last of them leaves. Which members
// still hold it is closeMerged's to judge, so this runs on every issue that leaves the workflow
// and changes nothing for any other root. It tells nobody: the architect heard the wait when the
// close entered it (merged), and this path is walked at every departure.
func (e *Engine) closeAwaitingRoot(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if !awaitsMerge(root) {
		return nil
	}
	pr, err := e.store.PullRequest(ctx, tx, root.Key)
	if err != nil || pr == nil || pr.State != record.PullRequestMerged {
		return err
	}
	waiting, err := e.closeMerged(ctx, tx, root, pr)
	if err != nil || len(waiting) > 0 {
		return err
	}
	e.logOnCommit(ctx, "workflow: a merged root closes once its last child left the workflow", "issue", root.Key, "pullRequest", pr.Number)
	return nil
}

// awaitsMerge says whether issue awaits its merge: in awaiting_merge, or held from it, the hold
// the upgrade's data move left an issue that was held in production_check.
func awaitsMerge(issue record.Issue) bool {
	return issue.Phase == phase.AwaitingMerge || issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.From == phase.AwaitingMerge
}

// InWorkflow says whether an issue is in the workflow: its status is one the workflow runs in and
// its phase short of done. One still admitted counts, since it will run — a root reopened inside
// its linger (admit's readmit) stands in `admitted` with no role's phase until its architect
// reaches planning, and is running again from the reopen. It is the production check's "runs
// again" predicate (check.go) and the outbox's (checkLookHolds): a look of an issue that runs
// again would resume the implementer and relaunch its Sandbox under the run, so none starts.
func InWorkflow(issue record.Issue) bool {
	return !record.OutOfWorkflow(issue.Status) && issue.Phase != phase.Done
}

// beginLinger suspends the root's whole tree and arms its linger deadline, ending the root's hold
// if it has one, as a child's leave does. Its one caller (leave) calls it once per close: a root
// that already lingers leaves no second time.
func (e *Engine) beginLinger(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	until := e.lingerAt()
	root.LingerUntil = &until
	root.Phase, root.Hold, root.Awaiting = phase.Done, nil, nil
	if err := e.store.PutIssue(ctx, tx, root); err != nil {
		return err
	}
	members, err := e.treeMembers(ctx, tx, root)
	if err != nil {
		return err
	}
	for _, member := range members {
		if err := e.everyClaim(ctx, tx, member, "suspend", fmt.Sprintf("the tree of %s lingers", root.Key), ""); err != nil {
			return err
		}
		if err := e.disarmOpen(ctx, tx, member.Key); err != nil {
			return err
		}
		if err := e.enqueue(ctx, tx, member.Key, record.IssueSuspend{
			Tree: member.Tree, Generation: member.Generation, TreeGeneration: root.Generation,
		}); err != nil {
			return err
		}
	}
	if err := e.enqueueAt(ctx, tx, root.Key, record.LingerClose{Generation: root.Generation}, until); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: tree lingers", "tree", root.Key, "generation", root.Generation, "until", until)
	return nil
}

// settleUnfinished records a hand-over for every child a done root leaves in the workflow at the
// end of its linger. A reopen inside the linger resumes those children in the old tree instead:
// lingerExpired is fenced on the root generation, and the durable hand-over record makes a late
// daemon PATCH no-op when the root came back before its event arrives. Each hand-over keeps the
// child's phase, gate, phase rows and pull request across a new root generation; promotion starts
// the phase's role again in that phase. A child still admitted never started under its tree's
// design, and one finished or parked has no run to carry on.
func (e *Engine) settleUnfinished(ctx context.Context, tx pgx.Tx, root record.Issue, members []record.Issue) error {
	for _, member := range members {
		if member.Key == root.Key || member.Phase == phase.Admitted || record.OutOfWorkflow(member.Status) {
			continue
		}
		if err := e.handOver(ctx, tx, root, member); err != nil {
			return err
		}
	}
	return nil
}

// handOver writes the durable fence before queuing the one Dispatch PATCH that sets the child todo
// and adds the legion label. intake consumes its daemon-authored event only while the fence names a
// root that still lingers at this generation; the direct child re-admission then carries state into
// the child's independent tree.
func (e *Engine) handOver(ctx context.Context, tx pgx.Tx, root, child record.Issue) error {
	if err := e.store.PutHandover(ctx, tx, record.Handover{Issue: child.Key, Tree: root.Tree, Generation: root.Generation}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a finished root hands its unfinished child to Legion", "issue", child.Key, "tree", root.Key,
		"phase", child.Phase, "status", child.Status)
	return e.enqueue(ctx, tx, child.Key, record.StatusWrite{Status: "todo", ObservedStatus: child.Status, HandOver: true,
		HandOverTree: root.Tree, HandOverLinger: root.Generation})
}

// lingerExpired closes the tree whose root's linger elapsed at the generation the fact names: a
// tree_close of every member's claims, the removal of every member's workspace, and, for a root
// that stayed done, the hand-over of each child it left in the workflow (settleUnfinished). The
// close waits while a member's production check is open: the check's looks run in the tree's
// Sandbox, on its volume, which the close deletes, so the fact returns a wait naming the moment
// the earliest open check is bound to have ended its last look (its bound plus the look bound),
// rolls back unapplied, and its row runs again then — or sooner, since the close of the tree's
// last open check re-arms the row (closeCheck). A tree whose root was reopened, or that lingers
// at a later generation, changes nothing.
func (e *Engine) lingerExpired(ctx context.Context, tx pgx.Tx, fact intake.LingerExpired) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Generation != fact.Generation || !issue.Lingers() {
		return intake.Result{}, err
	}
	open, err := e.store.OpenProductionChecksOfTree(ctx, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if len(open) > 0 {
		earliest := slices.MinFunc(open, func(a, b record.ProductionCheck) int { return a.BoundAt.Compare(b.BoundAt) })
		return intake.Result{}, wait.Until(earliest.BoundAt.Add(CheckLookBound),
			fmt.Sprintf("tree %s lingers past its deadline; member %s's production check is open", issue.Key, earliest.Issue))
	}
	members, err := e.treeMembers(ctx, tx, *issue)
	if err != nil {
		return intake.Result{}, err
	}
	// Each row names the root generation whose linger it expires: a member keeps its own generation
	// across re-admission, so only that tells this linger's rows from a later one's.
	for _, member := range members {
		for _, role := range claim.Roles {
			closeRow := record.SuperviseRequest{Op: "tree_close", Tree: member.Tree, Role: role, Generation: member.Generation, Linger: issue.Generation}
			if err := e.enqueue(ctx, tx, member.Key, closeRow); err != nil {
				return intake.Result{}, err
			}
		}
		if err := e.enqueue(ctx, tx, member.Key, record.WorkspaceRemove{Linger: issue.Generation}); err != nil {
			return intake.Result{}, err
		}
	}
	e.logOnCommit(ctx, "workflow: linger closed the tree", "tree", issue.Key, "generation", issue.Generation, "issues", len(members))
	if issue.Status == "done" {
		return intake.Result{}, e.settleUnfinished(ctx, tx, *issue, members)
	}
	return intake.Result{}, nil
}
