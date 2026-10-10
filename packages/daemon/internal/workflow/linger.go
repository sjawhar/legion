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
// takes its tree with it into linger, and a root that is done hands each child still mid-run to
// Legion as a root of its own (beginLinger). A child ends only itself: it leaves the table, its phase
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
		return e.beginLinger(ctx, tx, issue, status)
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
// if it has one, as a child's leave does; a second call while it lingers changes nothing. A root
// leaving as done also hands over each child still mid-run (handOver): its architect is gone for
// good, so nothing else would ever run that child again.
func (e *Engine) beginLinger(ctx context.Context, tx pgx.Tx, root record.Issue, status string) error {
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
		if status == "done" && midRun(root, member) {
			if err := e.handOver(ctx, tx, root, member); err != nil {
				return err
			}
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

// midRun says whether member is a child of root's tree in the middle of a run: past admission, so
// its first phase started under the tree's open gate, and neither finished nor taken out of the
// workflow. A child still admitted never started under the design its tree was closed on, and a
// child that finished or was parked has no run to carry on.
func midRun(root, member record.Issue) bool {
	return member.Key != root.Key && member.Phase != phase.Admitted && member.Phase != phase.Done && !record.OutOfWorkflow(member.Status)
}

// handOver hands a child its finished root leaves mid-run to Legion as a root of its own, with the
// write a person makes to do so: the child todo, with the legion label, in one Dispatch write. The
// event that write makes admits the child as an orphan (admit's orphan), a new generation its own
// architect runs. The write is queued behind the linger's suspends and runs after any status write
// of the child still queued, as an issue's status writes run in order; a person's move of the
// child before it runs stands (record.StatusWrite).
func (e *Engine) handOver(ctx context.Context, tx pgx.Tx, root, child record.Issue) error {
	e.logOnCommit(ctx, "workflow: a finished root hands its unfinished child to Legion", "issue", child.Key, "tree", root.Key,
		"phase", child.Phase, "status", child.Status)
	return e.enqueue(ctx, tx, child.Key, record.StatusWrite{Status: "todo", ObservedStatus: child.Status, HandOver: true})
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
	return intake.Result{}, nil
}
