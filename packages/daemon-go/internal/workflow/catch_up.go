package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// claimReady gives a tree's root architect its first instruction. Nothing else starts a turn of
// it: admission's start launches it with no task, and a re-admitted tree resumes its session. So
// at each launch's ready the daemon sends it a catch-up notice of what its tree holds (catchUp), as
// the TypeScript daemon sends the overseer catch-up at every ready of an active root
// (onTreeReady). A relaunch in the same generation is told again: it may be an architect that died
// during the turn a catch-up started, resumed idle, or a fresh agent whose workspace was lost, and
// nothing else wakes either. The ready is one fact per launch (workflowRuntime.applyTerminal keys
// it by the claim's launch generation), so the boot's replay of a ready already applied tells
// nothing more, and one never applied tells it once. The notice goes through the outbox like any
// other, so it is routed, held and re-held as they are. A tree that lingers or has left the
// workflow is told nothing, nor is a sub-architect (no workflow path starts one) or a phase worker.
func (e *Engine) claimReady(ctx context.Context, tx pgx.Tx, fact intake.ClaimReady) (intake.Result, error) {
	if fact.Role != claim.RoleArchitect {
		return intake.Result{}, nil
	}
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if root == nil || !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() || record.OutOfWorkflow(root.Status) {
		return intake.Result{}, nil
	}
	catchUp, err := e.catchUp(ctx, tx, *root)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.notice(ctx, tx, root.Key, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("%s is admitted at generation %d; start your tree as your role says", root.Key, root.Generation), CatchUp: &catchUp})
}

// catchUp is root's tree as the record holds it: the design gate policy and the root's gate, and
// every issue of the tree in key order.
func (e *Engine) catchUp(ctx context.Context, tx pgx.Tx, root record.Issue) (record.CatchUp, error) {
	catchUp := record.CatchUp{Generation: root.Generation, Gate: record.CatchUpGate{Policy: string(e.cfg.DesignGate)}}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil {
		return record.CatchUp{}, err
	}
	if gate != nil {
		catchUp.Gate.Artifact, catchUp.Gate.Version, catchUp.Gate.Open = gate.ArtifactID, gate.LatestVersion, classify.DesignGateOpen(*gate)
	}
	issues, err := e.store.TreeIssues(ctx, tx, root.Tree)
	if err != nil {
		return record.CatchUp{}, err
	}
	catchUp.Issues = make([]record.CatchUpIssue, 0, len(issues))
	for _, issue := range issues {
		listed := record.CatchUpIssue{Key: issue.Key, Title: issue.Title, Phase: issue.Phase, Status: issue.Status}
		if issue.Parent != nil {
			listed.Parent = *issue.Parent
		}
		catchUp.Issues = append(catchUp.Issues, listed)
	}
	return catchUp, nil
}
