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

// AdoptionRefusal says why issue cannot join the live tree tree, nil when it can. A recorded root
// that never ran as a tree of its own, whose tree's live root is tree's, is a member of that tree:
// it never opened a tree lifecycle under its key (only its promotion opens one, or an operator's
// root spawn) and carries no run handed over to it (handed_over_roots). Such a root's members
// cannot have started a phase either: a phase needs its gate, the gate needs the root's architect,
// and the architect needs a slot, whose promotion opens the lifecycle. A child already recorded in
// tree needs nothing; one recorded in any other tree is that tree's, not tree's to adopt. Every
// refusal is a 409 whose message names the issue and why.
//
// Admission's sweep of its queue (adoptQueuedRoots) leaves a refused root queueing as a root of
// its own; a release of children answers the refusal. Callers check it before Adopt.
func (e *Engine) AdoptionRefusal(ctx context.Context, tx pgx.Tx, issue record.Issue, tree string) (*intake.Refusal, error) {
	if issue.Tree == tree {
		return nil, nil
	}
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return &intake.Refusal{Status: 409, Code: "CHILD_IN_OTHER_TREE", Message: fmt.Sprintf("%s runs in the tree of %s, not %s", issue.Key, issue.Tree, tree)}, nil
	}
	notAdoptable := func(reason string) *intake.Refusal {
		return &intake.Refusal{Status: 409, Code: "CHILD_NOT_ADOPTABLE", Message: fmt.Sprintf("%s cannot join the tree of %s: %s", issue.Key, tree, reason)}
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return nil, err
	}
	lifecycle, found, err := e.store.TreeLifecycle(ctx, tx, project, issue.Key)
	if err != nil {
		return nil, err
	}
	if found {
		if lifecycle.HeldByOperator() {
			return notAdoptable("the operator runs a tree under its key"), nil
		}
		return notAdoptable("it ran as a tree of its own"), nil
	}
	handed, err := e.store.HandedOverRoot(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	if handed != nil {
		return notAdoptable("it carries a run handed over to it as a root of its own"), nil
	}
	root, err := e.store.Issue(ctx, tx, tree)
	if err != nil {
		return nil, err
	}
	live := false
	if root != nil {
		if live, err = record.TreeLive(ctx, e.store, tx, *root); err != nil {
			return nil, err
		}
	}
	if !live {
		return notAdoptable(fmt.Sprintf("the tree of %s is not live", tree)), nil
	}
	return nil, nil
}

// Adopt moves issue, a recorded root AdoptionRefusal found adoptable into root's live tree, into
// that tree with every record of its own tree, its parent recorded as parent; each member keeps
// its own. Each record takes its next generation, the fence every row of the old tree already
// checks (its linger's close, its supervise rows, its issue suspend), with no linger, hold or
// pending READY, no hand-over fence and its generation's facts cleared. A todo record enters the
// tree's run admitted, as an unrecorded todo child does (enterChild), so an open gate starts its
// planning; any other status keeps it as a parked child at done, which a todo runs again
// (ReenterChild). The tree's architect is told with a child-status notice. Everything is written
// in the caller's transaction.
func (e *Engine) Adopt(ctx context.Context, tx pgx.Tx, issue, root record.Issue, parent string) error {
	members, err := e.store.TreeIssues(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.Generation == ^uint64(0) {
			return fmt.Errorf("adopt %s into %s: generation overflows", member.Key, root.Tree)
		}
		if member.Key == issue.Key {
			member.Parent = record.ParentOf(parent)
		}
		next := member.Generation + 1
		if err := e.store.DeleteHandover(ctx, tx, member.Key); err != nil {
			return err
		}
		if err := e.store.ClearGeneration(ctx, tx, member.Key); err != nil {
			return err
		}
		if member.Status == "todo" {
			fact := intake.DispatchIssue{
				Key: member.Key, Seq: member.LastDispatchSeq, Status: member.Status, Title: member.Title,
				Rank: member.Rank, HandedOver: member.HandedOver, ResolvedRepo: member.Repo,
			}
			if member.Parent != nil {
				fact.Parent = *member.Parent
			}
			at := Resolution{Repo: member.Repo, DefaultBranch: member.DefaultBranch, GitHubRepo: member.GitHubRepo}
			if err := e.enterChild(ctx, tx, root, fact, next, at); err != nil {
				return err
			}
			continue
		}
		member.Tree, member.Project, member.Generation, member.Phase = root.Tree, root.Project, next, phase.Done
		member.LingerUntil, member.Hold, member.ReadyPendingVersion = nil, nil, nil
		if err := e.store.PutIssue(ctx, tx, member); err != nil {
			return err
		}
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "child-status", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("%s joined the tree of %s from the admission queue", issue.Key, root.Key)}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a queued root joined a live tree", "issue", issue.Key, "tree", root.Tree, "generation", issue.Generation+1)
	return nil
}
