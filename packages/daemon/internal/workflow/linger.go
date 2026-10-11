package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// leave is an issue leaving the workflow for status (done, backlog, icebox, or triage). A root
// takes its tree with it into linger. A child ends only itself: it leaves the table, its phase
// parked in done, so no transition or status write follows the human's move; the tree's
// architect is told, and decides what the rest of its tree does, as the shipped daemon routes a
// child's close to the architect. A later todo re-enters the child. What becomes of the child's
// claims and its pod is the status's: a child parked in backlog, icebox or triage has every claim
// suspended and its Sandbox kept, sessions and volume with it, until it runs again or its tree
// closes; a child closed as done is finished, so each of its claims is closed (issue_close: retired,
// its session dropped) and its Sandbox released with the volume it owns (record.IssueSuspend's
// Release), while its parent keeps running on a volume of its own. A done child set back to todo
// starts fresh.
func (e *Engine) leave(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	e.logOnCommit(ctx, "workflow: issue left the workflow", "issue", issue.Key, "tree", issue.Tree, "status", status)
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return e.beginLinger(ctx, tx, issue)
	}
	if issue.Phase != phase.Done {
		issue.Phase, issue.Hold, issue.ReadyPendingVersion = phase.Done, nil, nil
		if err := e.store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
	}
	op, release := record.SuperviseOp("suspend"), false
	if status == "done" {
		op, release = "issue_close", true
	}
	if err := e.everyClaim(ctx, tx, issue, op, fmt.Sprintf("%s is %s", issue.Key, status)); err != nil {
		return err
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil {
		return err
	}
	if root == nil {
		return fmt.Errorf("suspend issue %s: tree root %s is not recorded", issue.Key, issue.Tree)
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{
		Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation, Release: release,
	}); err != nil {
		return err
	}
	kind := record.NoticeKind("child-status")
	if status == "done" {
		kind = "child-closed"
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: kind, Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is %s", issue.Key, status)})
}

// beginLinger suspends the root's whole tree and arms its linger deadline, ending the root's hold
// if it has one, as a child's leave does; a second call while it lingers changes nothing.
func (e *Engine) beginLinger(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if root.Lingers() {
		return nil
	}
	until := e.lingerAt()
	root.LingerUntil = &until
	root.Phase, root.Hold = phase.Done, nil
	if err := e.store.PutIssue(ctx, tx, root); err != nil {
		return err
	}
	members, err := e.treeMembers(ctx, tx, root)
	if err != nil {
		return err
	}
	for _, member := range members {
		if err := e.everyClaim(ctx, tx, member, "suspend", fmt.Sprintf("the tree of %s lingers", root.Key)); err != nil {
			return err
		}
		if err := e.enqueue(ctx, tx, member.Key, record.IssueSuspend{
			Tree: member.Tree, Generation: member.Generation, TreeGeneration: root.Generation,
		}); err != nil {
			return err
		}
	}
	row, err := record.NewOutboxRow(root.Key, record.LingerClose{Generation: root.Generation}, until)
	if err != nil {
		return err
	}
	if err := e.store.Enqueue(ctx, tx, row); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: tree lingers", "tree", root.Key, "generation", root.Generation, "until", until)
	return nil
}

// settleUnfinished is the close, at the end of a done root's linger, of each child of its tree
// still in the workflow and past admission. The linger is the window in which the root can come
// back: a person setting it todo re-admits the tree, and admission's startMidPhaseChildren resumes
// each such child where it stood. So the settling waits for the linger to end with the root still
// done, whether a sign-off, a close, or a person closing a root they had parked put it there, and
// lingerExpired's fence on the root's generation means a root that came back settles nothing.
//
// A child in its first phases (planning through reviewing, or held from one) is handed to Legion
// as a root of its own (handOver). Its re-admission is a new generation that clears the old one's
// record (admit's readmit): under an armed design gate a person approves its spec again, and it
// plans and works again from its branch. A child nearer done (retro, merging, awaiting_merge,
// production_check, or held from one) is not handed over: that clearing would drop its approved
// review, its READY, and the record of a pull request that may already be merged, and run work
// already on its base again. It is left for a person to finish, and the issue says so. A child still
// admitted never started under its tree's design, and one finished or parked has no run to carry on.
func (e *Engine) settleUnfinished(ctx context.Context, tx pgx.Tx, root record.Issue, members []record.Issue) error {
	for _, member := range members {
		if member.Key == root.Key || record.OutOfWorkflow(member.Status) {
			continue
		}
		at := member.Phase
		if at == phase.Held && member.Hold != nil {
			at = member.Hold.From
		}
		var err error
		switch at {
		case phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing:
			err = e.handOver(ctx, tx, root, member)
		case phase.Retro, phase.Merging, phase.AwaitingMerge, phase.ProductionCheck:
			e.logOnCommit(ctx, "workflow: a finished root leaves its nearly done child for a person", "issue", member.Key, "tree", root.Key, "phase", at)
			err = e.enqueue(ctx, tx, member.Key, record.MessagePost{Body: fmt.Sprintf(
				"%s closed while this issue was in %s, and Legion no longer runs it. It is not handed over as a root of its own, since that would clear its review and pull request record and run its work again; a person finishes it.", root.Key, at)})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// handOver hands a child its finished root leaves in its first phases to Legion as a root of its
// own, with the write a person makes to do so: the child todo, with the legion label, in one
// Dispatch write. The event that write makes admits the child as an orphan (admit's orphan). The
// write names the tree and the root generation whose linger ended, and the outbox writes it only
// while the child is still that tree's and the root still lingers at that generation, so a root
// re-admitted before the write runs keeps its child (record.StatusWrite's HandOverTree). It runs
// after any status write of the child still queued, as an issue's status writes run in order, and a
// person's move of the child before it runs stands.
func (e *Engine) handOver(ctx context.Context, tx pgx.Tx, root, child record.Issue) error {
	e.logOnCommit(ctx, "workflow: a finished root hands its unfinished child to Legion", "issue", child.Key, "tree", root.Key,
		"phase", child.Phase, "status", child.Status)
	return e.enqueue(ctx, tx, child.Key, record.StatusWrite{Status: "todo", ObservedStatus: child.Status, HandOver: true,
		HandOverTree: root.Tree, HandOverLinger: root.Generation})
}

func (e *Engine) lingerExpired(ctx context.Context, tx pgx.Tx, fact intake.LingerExpired) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Generation != fact.Generation || !issue.Lingers() {
		return intake.Result{}, err
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
