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

// AdoptionRefusal says why issue cannot join tree for what its own record says, nil when it can. A
// recorded root that never ran as a tree of its own (RanAsRoot), whose tree's live root is tree's,
// is a member of that tree: whether tree is live is the caller's to read, once for every issue it
// asks about, and a release of children refuses a tree that is not live by name
// (intake.ChildNotAdoptable). A child already recorded in tree needs nothing; one recorded in any
// other tree is that tree's, not tree's to adopt (intake.ChildInOtherTree). Every refusal is a 409
// whose message names the issue and why.
//
// Admission's sweep of its queue (adoptQueuedRoots) leaves a refused root queueing as a root of
// its own; a release of children answers the refusal. Callers check it before Adopt.
func (e *Engine) AdoptionRefusal(ctx context.Context, tx pgx.Tx, issue record.Issue, tree string) (*intake.Refusal, error) {
	if issue.Tree == tree {
		return nil, nil
	}
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return intake.ChildInOtherTree(issue.Key, issue.Tree, tree), nil
	}
	ran, err := e.RanAsRoot(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	if ran != "" {
		return intake.ChildNotAdoptable(issue.Key, tree, ran), nil
	}
	return nil, nil
}

// RanAsRoot says how issue ran as a tree of its own, "" when it never did: it opened a tree
// lifecycle under its key (only its promotion opens one, or an operator's root spawn) or carries a
// run handed over to it (handed_over_roots). The members of a root that never ran cannot have
// started a phase either: a phase needs its gate, the gate needs the root's architect, and the
// architect needs a slot, whose promotion opens the lifecycle. A root that ran is never adopted
// (AdoptionRefusal) nor held under another tree (Ancestry).
func (e *Engine) RanAsRoot(ctx context.Context, tx pgx.Tx, issue record.Issue) (string, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	lifecycle, found, err := e.store.TreeLifecycle(ctx, tx, project, issue.Key)
	if err != nil {
		return "", err
	}
	if found {
		if lifecycle.HeldByOperator() {
			return "the operator runs a tree under its key", nil
		}
		return "it ran as a tree of its own", nil
	}
	handed, err := e.store.HandedOverRoot(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	if handed != nil {
		return "it carries a run handed over to it as a root of its own", nil
	}
	return "", nil
}

// Ancestry is the Dispatch chain issue's record takes when an observation's preparation read chain
// (record.Issue.Ancestry): chain, or nil when issue ran as a tree of its own (RanAsRoot). The
// waiting line holds a root while a live root is on its chain until that root's release adopts it,
// and a release refuses a root that ran, so such a root keeps queueing as its own instead: held,
// nothing would ever bring it in. Every write of an observation's chain to a record goes through
// this.
func (e *Engine) Ancestry(ctx context.Context, tx pgx.Tx, issue record.Issue, chain []string) ([]string, error) {
	if len(chain) == 0 {
		return nil, nil
	}
	ran, err := e.RanAsRoot(ctx, tx, issue)
	if err != nil || ran != "" {
		return nil, err
	}
	return chain, nil
}

// Adopt moves issue, a recorded root AdoptionRefusal found adoptable into root's live tree, into
// that tree with every record of its own tree, its parent recorded as parent; each member keeps
// its own. Each record takes its next generation, the fence every row of the old tree already
// checks (its linger's close, its supervise rows, its issue suspend), with no linger, hold,
// pending READY or Dispatch chain (record.Issue.Ancestry: a member waits in no line), no
// hand-over fence and its generation's facts cleared. A todo record enters the tree's run
// admitted, as an unrecorded todo child does (enterChild), so an open gate starts its planning;
// any other status keeps it as a parked child at done, which a todo runs again (ReenterChild).
// The tree's architect is told with a child-status notice. Everything is written in the caller's
// transaction.
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
			if err := e.enterChild(ctx, tx, root, RecordedFact(member), next, recordedResolution(member)); err != nil {
				return err
			}
			continue
		}
		member.Tree, member.Project, member.Generation, member.Phase = root.Tree, root.Project, next, phase.Done
		member.LingerUntil, member.Hold, member.ReadyPendingVersion, member.Ancestry = nil, nil, nil, nil
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
