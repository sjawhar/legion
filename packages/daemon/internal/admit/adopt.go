package admit

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// adoption is a recorded root that joins the live tree of target, that tree's root, recorded under
// the Dispatch parent parent (workflow's Engine.Adopt).
type adoption struct {
	issue, target record.Issue
	parent        string
}

// adoptChildren applies an architect's release of children recorded outside its tree: each root
// that queued in admission before a person made it a child of fact.Tree joins that tree with every
// record of its own (workflow's Engine.Adopt), under the Dispatch parent the release read, and the
// line is promoted as after any other fact. A child already recorded in fact.Tree needs nothing,
// and a member of another child's queued tree joins with that child.
//
// The wave is one decision: when any child cannot join (workflow's AdoptionRefusal), or the tree
// is not live when the first child that could join is asked about, that refusal is the answer and
// nothing is written, no adoption and no promotion, since intake commits a refused fact.
// Reconcile's holds do not apply: the release's own Dispatch read just proved each child's parent
// chain reaches the tree.
func (a *Admission) adoptChildren(ctx context.Context, tx pgx.Tx, fact intake.ChildAdoption) (*intake.Refusal, error) {
	var outside []adoption
	seen := make(map[string]bool, len(fact.Children))
	for _, child := range fact.Children {
		if seen[child.Issue] {
			continue
		}
		seen[child.Issue] = true
		stored, err := a.store.Issue(ctx, tx, child.Issue)
		if err != nil {
			return nil, fmt.Errorf("read released child %s: %w", child.Issue, err)
		}
		if stored != nil && stored.Tree != fact.Tree {
			outside = append(outside, adoption{issue: *stored, parent: child.Parent})
		}
	}
	// The tree's root and its liveness are read once for every child the release asks about.
	var root *record.Issue
	live := false
	if len(outside) > 0 {
		var err error
		if root, err = a.store.Issue(ctx, tx, fact.Tree); err != nil {
			return nil, fmt.Errorf("read the root of %s: %w", fact.Tree, err)
		}
		if root != nil {
			if live, err = record.TreeLive(ctx, a.store, tx, *root); err != nil {
				return nil, fmt.Errorf("read whether the tree of %s is live: %w", fact.Tree, err)
			}
		}
	}
	queued := make(map[string]bool, len(outside))
	for _, child := range outside {
		if claim.IsTreeRoot(child.issue.Key, child.issue.Tree) {
			queued[child.issue.Key] = true
		}
	}
	var roots []adoption
	for _, child := range outside {
		if !claim.IsTreeRoot(child.issue.Key, child.issue.Tree) && queued[child.issue.Tree] {
			continue
		}
		refusal, err := a.engine.AdoptionRefusal(ctx, tx, child.issue, fact.Tree)
		if err != nil {
			return nil, fmt.Errorf("read whether %s joins the tree of %s: %w", child.issue.Key, fact.Tree, err)
		}
		if refusal != nil {
			return refusal, nil
		}
		if !live {
			return intake.ChildNotAdoptable(child.issue.Key, fact.Tree, fmt.Sprintf("the tree of %s is not live", fact.Tree)), nil
		}
		// A live tree's root is recorded.
		child.target = *root
		roots = append(roots, child)
	}
	for _, child := range roots {
		if err := a.engine.Adopt(ctx, tx, child.issue, child.target, child.parent); err != nil {
			return nil, fmt.Errorf("adopt %s into the tree of %s: %w", child.issue.Key, fact.Tree, err)
		}
	}
	_, err := a.promote(ctx, tx)
	return nil, err
}

// adoptQueuedRoots moves every todo root of this project, waiting in admission's queue or not yet
// handed to Legion, into the live tree its recorded parent runs in, when that root never ran as a
// tree of its own (workflow's AdoptionRefusal, Engine.Adopt): a person made it a child of that tree,
// so it takes no slot and no architect of its own, and runs in the tree as any of its todo children
// does. A candidate is a recorded root of this project in todo whose parent is recorded in a tree
// other than its own. One that ran as a tree of its own, or carries a run handed over to it, keeps
// queueing as its own root: taking it out of the line would strand its run with nobody told. So
// does one handed over as a root of its own (record.Issue.HandedOverFrom), its old root's
// re-admission included: the hand-over made it a tree of its own for good. A key Reconcile holds,
// the candidate's or its target's, is left for release to apply first, when respectHolds is set.
//
// It loops until a pass adopts nothing, and returns the issues and slots that pass read: it wrote
// nothing, so they are the record as it stands. Each adoption makes a root a member, never a root
// again, so the roots bound the passes. A candidate whose target root is itself a candidate waits
// for the next pass, so nested queued roots join the live tree at the top once each, rather than
// passing through the queued tree between; candidates whose parents form a cycle are each other's
// targets and stay roots of their own.
func (a *Admission) adoptQueuedRoots(ctx context.Context, tx pgx.Tx, respectHolds bool) ([]record.Issue, []record.Slot, error) {
	for {
		issues, err := a.store.Issues(ctx, tx)
		if err != nil {
			return nil, nil, fmt.Errorf("list admission issues: %w", err)
		}
		slots, err := a.store.Slots(ctx, tx)
		if err != nil {
			return nil, nil, fmt.Errorf("list admission slots: %w", err)
		}
		// A target whose tree is not live is skipped here, so every target asked about is live.
		live := record.LiveRoots(issues, slots)
		recorded := make(map[string]record.Issue, len(issues))
		for _, issue := range issues {
			recorded[issue.Key] = issue
		}
		var candidates []adoption
		for _, issue := range issues {
			if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Status != "todo" || issue.Parent == nil {
				continue
			}
			parent, ok := recorded[*issue.Parent]
			if !ok {
				continue
			}
			target, ok := recorded[parent.Tree]
			if !ok || target.Key == issue.Key || respectHolds && (a.held(issue.Key) || a.held(target.Key)) {
				continue
			}
			if _, ok := live[target.Key]; !ok {
				continue
			}
			refusal, err := a.engine.AdoptionRefusal(ctx, tx, issue, target.Key)
			if err != nil {
				return nil, nil, fmt.Errorf("read whether %s joins the tree of %s: %w", issue.Key, target.Key, err)
			}
			if refusal == nil {
				candidates = append(candidates, adoption{issue: issue, target: target, parent: *issue.Parent})
			}
		}
		adopted := false
		for _, candidate := range candidates {
			if slices.ContainsFunc(candidates, func(other adoption) bool { return other.issue.Key == candidate.target.Key }) {
				continue
			}
			if err := a.engine.Adopt(ctx, tx, candidate.issue, candidate.target, candidate.parent); err != nil {
				return nil, nil, fmt.Errorf("adopt %s into the tree of %s: %w", candidate.issue.Key, candidate.target.Key, err)
			}
			adopted = true
		}
		if !adopted {
			return issues, slots, nil
		}
	}
}
