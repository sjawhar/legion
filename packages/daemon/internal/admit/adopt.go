package admit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// adoptChildren applies an architect's release of children recorded outside its tree: each root
// that queued in admission before a person made it a child of fact.Tree joins that tree with every
// record of its own (workflow's Engine.Adopt), under the Dispatch parent the release read, and the
// line is promoted as after any other fact. A child already recorded in fact.Tree needs nothing,
// and a member of another child's queued tree joins with that child.
//
// The wave is one decision: when any child cannot join (workflow's AdoptionRefusal), that refusal
// is the answer and nothing is written, no adoption and no promotion, since intake commits a
// refused fact. Reconcile's holds do not apply: the release's own Dispatch read just proved each
// child's parent chain reaches the tree.
func (a *Admission) adoptChildren(ctx context.Context, tx pgx.Tx, fact intake.ChildAdoption) (*intake.Refusal, error) {
	type adoption struct {
		issue  record.Issue
		parent string
	}
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
		roots = append(roots, child)
	}
	if len(roots) > 0 {
		// AdoptionRefusal found the tree's root recorded and live.
		root, err := a.store.Issue(ctx, tx, fact.Tree)
		if err != nil {
			return nil, fmt.Errorf("read the root of %s: %w", fact.Tree, err)
		}
		for _, child := range roots {
			if err := a.engine.Adopt(ctx, tx, child.issue, *root, child.parent); err != nil {
				return nil, fmt.Errorf("adopt %s into the tree of %s: %w", child.issue.Key, fact.Tree, err)
			}
		}
	}
	_, err := a.promote(ctx, tx)
	return nil, err
}
