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

// closeRoot ends a tree its root architect closes before the tree's first phase starts: a human
// decided no change at the design gate, or the issue is moot. sign_off
// needs the production check and a backward move a phase worker, so without this only a human's
// status write could end such a tree, and it would hold its admission slot until then. The close
// writes done to Dispatch with the architect's reason, which the write posts on the issue first,
// and lingers the tree as any close does; the admission slot is released in the same fact.
//
// A root waiting on a person (phase.AwaitingPerson) closes the same way, because it has started
// no phase either: a root reaches that phase only from admitted, and it is where the answer
// "close this" arrives (dispatch://INFRA-545, whose architect could not end its tree and whose
// controller's own Dispatch close the daemon set back, as a session's write on a waiting root).
// The slot it gave up while it waits is already free, and leave releases nothing twice.
func (e *Engine) closeRoot(ctx context.Context, tx pgx.Tx, fact intake.CloseRoot) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	if !claim.IsTreeRoot(root.Key, root.Tree) {
		return refused("ROOT_REQUIRED", fmt.Sprintf("%s is a child of %s; close_root ends a tree root, and a child leaves with park_child", root.Key, root.Tree)), nil
	}
	if !closableAtPhase(root.Phase) || root.Lingers() || record.OutOfWorkflow(root.Status) {
		return refused("CLOSE_AFTER_PHASE_STARTED", fmt.Sprintf("%s is in phase %s with status %s; close_root ends a tree only while its root has started no phase — holding its slot at admitted, or waiting on a person — and a tree past that, or one already leaving, ends through its workflow or a human", root.Key, root.Phase, root.Status)), nil
	}
	if err := e.enqueue(ctx, tx, root.Key, record.OrdinaryStatusWrite("done", root.Status, fact.Message())); err != nil {
		return intake.Result{}, err
	}
	root.Status = "done"
	return intake.Result{}, e.leave(ctx, tx, *root, "done")
}

// closableAtPhase says whether a root at this phase has started no phase of its tree, which is
// what close_root ends. Both are states a root sits in with its design gate unanswered.
func closableAtPhase(p phase.Phase) bool {
	return p == phase.Admitted || p == phase.AwaitingPerson
}
