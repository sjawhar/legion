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
)

// leave is an issue leaving the workflow for status (done, backlog, icebox, or triage). A root
// takes its tree with it into linger. A child ends only itself: it leaves the table, its phase
// parked in done and every one of its claims suspended, so no transition or status write follows
// the human's move; the tree's architect is told, and decides what the rest of its tree does, as
// the shipped daemon routes a child's close to the architect. A later todo re-enters the child.
func (e *Engine) leave(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return e.beginLinger(ctx, tx, issue)
	}
	if issue.Phase != phase.Done {
		issue.Phase, issue.HeldFrom, issue.ReadyPendingVersion = phase.Done, nil, nil
		if err := e.store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
	}
	if err := e.everyClaim(ctx, tx, issue, "suspend"); err != nil {
		return err
	}
	kind := record.NoticeKind("child-status")
	if status == "done" {
		kind = "child-closed"
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: kind, Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is %s", issue.Key, status)})
}

// beginLinger suspends the root's whole tree and arms its linger deadline; a second call while it
// lingers changes nothing.
func (e *Engine) beginLinger(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if root.LingerUntil != nil {
		return nil
	}
	until := e.lingerAt()
	root.LingerUntil = &until
	root.Phase = phase.Done
	if err := e.store.PutIssue(ctx, tx, root); err != nil {
		return err
	}
	members, err := e.treeMembers(ctx, tx, root)
	if err != nil {
		return err
	}
	for _, member := range members {
		if err := e.everyClaim(ctx, tx, member, "suspend"); err != nil {
			return err
		}
	}
	row, err := record.NewOutboxRow(root.Key, record.LingerClose{Generation: root.Generation}, until)
	if err != nil {
		return err
	}
	return e.store.Enqueue(ctx, tx, row)
}

func (e *Engine) lingerExpired(ctx context.Context, tx pgx.Tx, fact intake.LingerExpired) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Generation != fact.Generation || issue.LingerUntil == nil {
		return intake.Result{}, err
	}
	members, err := e.treeMembers(ctx, tx, *issue)
	if err != nil {
		return intake.Result{}, err
	}
	for _, member := range members {
		if err := e.everyClaim(ctx, tx, member, "tree_close"); err != nil {
			return intake.Result{}, err
		}
		if err := e.enqueue(ctx, tx, member.Key, record.WorkspaceRemove{Generation: member.Generation}); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

func (e *Engine) liveTree(ctx context.Context, tx pgx.Tx, root record.Issue) (bool, error) {
	if root.LingerUntil != nil {
		return false, nil
	}
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == root.Key }) {
		return true, nil
	}
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(record.Waiting(issues, slots), func(waiting record.Issue) bool { return waiting.Key == root.Key }), nil
}
