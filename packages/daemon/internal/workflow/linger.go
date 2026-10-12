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
// starts fresh. An issue leaving disarms its open pull request (disarmOpen), as a root's linger
// disarms every member's.
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
	if err := e.disarmOpen(ctx, tx, issue.Key); err != nil {
		return err
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
		if err := e.disarmOpen(ctx, tx, member.Key); err != nil {
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
