package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// claimReady gives a tree's root architect its first instruction. Nothing else starts a turn of it:
// admission's start launches it with no task, or with only the decisions kept while nobody in its
// tree could hear them (ParkedDecisionTask), and a re-admitted tree resumes its session. So at each
// launch's ready the daemon sends it a catch-up notice of what its tree holds (QueueCatchUp), and
// so does a re-admission's start that finds the claim still running (the outbox's start), as the
// TypeScript daemon sends the overseer catch-up at every ready of an active root (onTreeReady). A
// relaunch in the same generation is told again: it may be an architect that died during the turn a
// catch-up started, resumed idle, or a fresh agent whose workspace was lost, and nothing else wakes
// either. The ready is one fact per launch (workflowRuntime.applyTerminal keys it by the claim's
// launch generation), so the boot's replay of a ready already applied tells nothing more, and one
// never applied tells it once. The catch-up this ready writes names every decision nobody else
// claims, so a decision a release left unclaimed reaches this architect here; one still claimed by
// a queued start or catch-up is that row's to settle. A tree that lingers or has left the workflow
// is told nothing, nor is a sub-architect (no workflow path starts one) or a phase worker.
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
	return intake.Result{}, QueueCatchUp(ctx, tx, e.store, e.cfg.DesignGate, *root, fact.Launch, e.now())
}

// QueueCatchUp is the one composer of a root architect's catch-up, which both the ready
// (claimReady) and a hand-over that finds the root with an architect to tell (HandOverKept) write
// through, so the two never compose one differently. It states root's tree as the record holds it
// (treeCatchUp) for launch, the launch it was written for, so a failed forward of an older one is
// not queued again behind it (the outbox's rehold).
//
// A catch-up an earlier launch never had delivered is dropped first (DropCatchUps, at the caller's
// now, which is the clock a lane's lease is written from), so the agent is given one catch-up,
// computed now, which names again the decisions the dropped one named — the drop releases them in
// this transaction — with any kept since. A catch-up that carries kept decisions and is being
// published under a live lease is not dropped: it settles what it named itself, and this one names
// only what nobody claims. The notice goes through the outbox like any other, so it is routed, held
// and re-held as they are.
func QueueCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, gate config.DesignGate, root record.Issue, launch uint64, now time.Time) error {
	catchUp, awaiting, err := treeCatchUp(ctx, tx, store, gate, root)
	if err != nil {
		return err
	}
	catchUp.Launch = launch
	if err := store.DropCatchUps(ctx, tx, root.Key, now); err != nil {
		return err
	}
	// A decision kept since the start's task was written (one a person made while the tree's
	// architect claim had ended, before this relaunch), and one a start or catch-up that reached no
	// architect handed over, are told here. The catch-up names only what no other start or catch-up
	// claims (a start still queued keeps its own), and claims what it names in this transaction; its
	// publish forgets those rows (the outbox's notice), and a catch-up a newer one supersedes hands
	// them to a fresh catch-up (HandOverKept).
	decided, kept, err := ParkedDecisionTask(ctx, tx, store, gate, root)
	if err != nil {
		return err
	}
	catchUp.Decided, catchUp.Kept = decided, kept
	reason := fmt.Sprintf("the tree of %s at generation %d as the daemon records it now; start or resume it as your role says", root.Key, root.Generation)
	if len(awaiting) > 0 {
		reason += fmt.Sprintf("; in accepting, waiting for your acceptance, which no worker gives: %s (read the evidence, then sign_off or request_backward_move)", strings.Join(awaiting, ", "))
	}
	return EnqueueNaming(ctx, tx, store, root.Key, root.Key, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: reason, CatchUp: &catchUp},
		kept, now)
}

// treeCatchUp is root's tree as the record holds it: whether a hand-over created the root at this
// generation, and whether its old root's gate came with it, the design gate policy and the root's
// gate, and every issue of the tree in key order. With it come the keys, in key order, of the
// tree's issues waiting for the architect's acceptance: each in accepting and still in the
// workflow. No worker runs in accepting, and the one phase-finished notice that moved an issue
// there is ended undelivered when the architect's claim has failed (the outbox's notice), so a
// retried architect learns of the wait from its catch-up alone; the reason names these keys so
// that the catch-up, which a revived architect reads as the tree already running, says what is
// the architect's own to do.
func treeCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, gate config.DesignGate, root record.Issue) (record.CatchUp, []string, error) {
	catchUp := record.CatchUp{Generation: root.Generation, Gate: record.CatchUpGate{Policy: string(gate)}}
	handed, err := store.HandedOverRoot(ctx, tx, root.Key)
	if err != nil {
		return record.CatchUp{}, nil, err
	}
	if handed != nil && handed.Generation == root.Generation {
		catchUp.HandedOver, catchUp.Carried = true, handed.GateCarried
	}
	recorded, err := store.Gate(ctx, tx, root.Key)
	if err != nil {
		return record.CatchUp{}, nil, err
	}
	if recorded != nil {
		catchUp.Gate.Artifact, catchUp.Gate.Version, catchUp.Gate.Open = recorded.ArtifactID, recorded.LatestVersion, classify.DesignGateOpen(*recorded)
	}
	issues, err := store.TreeIssues(ctx, tx, root.Tree)
	if err != nil {
		return record.CatchUp{}, nil, err
	}
	catchUp.Issues = make([]record.CatchUpIssue, 0, len(issues))
	awaiting := []string{}
	for _, issue := range issues {
		listed := record.CatchUpIssue{Key: issue.Key, Title: issue.Title, Phase: issue.Phase, Status: issue.Status}
		if issue.Parent != nil {
			listed.Parent = *issue.Parent
		}
		catchUp.Issues = append(catchUp.Issues, listed)
		if issue.Phase == phase.Accepting && InWorkflow(issue) {
			awaiting = append(awaiting, issue.Key)
		}
	}
	return catchUp, awaiting, nil
}
