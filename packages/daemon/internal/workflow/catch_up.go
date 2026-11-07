package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
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
//
// The catch-up a start writes in place of a launch (intake.ClaimReady.Paced) is a wake of the
// architect — it reads its whole context when the catch-up arrives, which is what the per-minute
// cap counts — so it takes its turn in the daemon's pacing (Config.Reserve) and the row is due at
// the moment that turn comes, rather than at once. The turn is taken here, inside the fact's
// transaction, so a start row that runs again after a crash takes no second one: the fact applies
// once for the row that wrote it. A launch's own ready takes none, since the launch took its turn
// at the machine's gate (supervise's reserveWake); so does every ready while the pacing is off.
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
	now := e.now()
	var paced time.Time
	if fact.Paced && e.cfg.Reserve != nil {
		paced = e.cfg.Reserve(now)
	}
	return intake.Result{}, QueueCatchUp(ctx, tx, e.store, e.cfg.Steps, *root, fact.Launch, now, paced)
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
// this transaction — with any kept since. A catch-up that carries kept decisions is not dropped
// while any row of its telling — itself, the row it copies, or a re-held copy of either — is under
// a live lease: that row may be publishing now, and the decisions the catch-up claims are that
// telling's to settle, so this one names only what nobody claims. The notice goes through the
// outbox like any other, so it is routed, held and re-held as they are.
//
// paced is the moment the daemon's wake pacing reserved for this catch-up, which is both the
// notice row's next_at and the moment the notice itself carries (record.Notice.PacedAt): the zero
// value, which every caller but the paced ready passes (CatchUpTheTreesOwed for HandOverKept, and
// the ready of a launch), is now, so a catch-up no pacing holds is due as it always was. The row
// keeps the moment of its own because a failed attempt rewrites next_at to its backoff: the
// moment is what keeps a ready from pulling the row forward before its turn (the daemon's
// releaseWaiting), and with now beside it (record.Notice.PacedFrom) it is the wait in the line
// that wake writes when it runs (the daemon's wakeRan). The pair is written only where the
// pacing reserved something: a catch-up it held to no moment reports no wait, having had none.
func QueueCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, root record.Issue, launch uint64, now, paced time.Time) error {
	catchUp, awaiting, err := treeCatchUp(ctx, tx, store, steps, root)
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
	decided, kept, err := ParkedDecisionTask(ctx, tx, store, steps, root)
	if err != nil {
		return err
	}
	catchUp.Decided, catchUp.Kept = decided, kept
	reason := fmt.Sprintf("the tree of %s at generation %d as the daemon records it now; start or resume it as your role says", root.Key, root.Generation)
	if len(awaiting) > 0 {
		reason += fmt.Sprintf("; in accepting, waiting for your acceptance, which no worker gives: %s (read the evidence, then sign_off or request_backward_move)", strings.Join(awaiting, ", "))
	}
	at := now
	notice := record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: reason, CatchUp: &catchUp}
	if !paced.IsZero() {
		at = paced
		notice.PacedAt, notice.PacedFrom = paced, now
	}
	return EnqueueNaming(ctx, tx, store, root.Key, root.Key, notice, kept, at)
}

// treeCatchUp is root's tree as the record holds it: whether a hand-over created the root at this
// generation, and whether its old root's gate came with it, each design-gate step the root must
// hold open and how it stands, and every issue of the tree in key order. With it come the keys,
// in key order, of the tree's issues waiting for the architect's acceptance: each in accepting
// and still in the workflow. No worker runs in accepting, and the one phase-finished notice that
// moved an issue there is ended undelivered when the architect's claim has failed (the outbox's
// notice), so a retried architect learns of the wait from its catch-up alone; the reason names
// these keys so that the catch-up, which a revived architect reads as the tree already running,
// says what is the architect's own to do.
func treeCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, root record.Issue) (record.CatchUp, []string, error) {
	catchUp := record.CatchUp{Generation: root.Generation}
	handed, err := store.HandedOverRoot(ctx, tx, root.Key)
	if err != nil {
		return record.CatchUp{}, nil, err
	}
	if handed != nil && handed.Generation == root.Generation {
		catchUp.HandedOver, catchUp.Carried = true, handed.GateCarried
	}
	gates, err := store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return record.CatchUp{}, nil, err
	}
	required := classify.RequiredSteps(steps)
	catchUp.Gates = make([]record.CatchUpGate, 0, len(required))
	for _, step := range required {
		stated := record.CatchUpGate{Step: step, Gated: slices.Contains(steps, step)}
		if recorded, ok := gates[step]; ok {
			stated.Artifact, stated.Version, stated.Open = recorded.ArtifactID, recorded.LatestVersion, classify.DesignGateOpen(recorded)
			if recorded.ApprovedVersion != nil {
				stated.Approved = *recorded.ApprovedVersion
			}
		} else {
			stated.Unregistered = true
		}
		catchUp.Gates = append(catchUp.Gates, stated)
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
		if issue.Phase == phase.Accepting && record.InWorkflow(issue) {
			awaiting = append(awaiting, issue.Key)
		}
	}
	return catchUp, awaiting, nil
}
