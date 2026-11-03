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
// is told nothing, nor is a sub-architect (no workflow path starts one). A phase worker's ready is
// answered differently: it is given its phase's task again when it holds that phase with nothing
// pending (readyWorkerTask).
func (e *Engine) claimReady(ctx context.Context, tx pgx.Tx, fact intake.ClaimReady) (intake.Result, error) {
	if fact.Role != claim.RoleArchitect {
		return intake.Result{}, e.readyWorkerTask(ctx, tx, fact)
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
// this transaction — with any kept since. A catch-up that carries kept decisions is not dropped
// while any row of its telling — itself, the row it copies, or a re-held copy of either — is under
// a live lease: that row may be publishing now, and the decisions the catch-up claims are that
// telling's to settle, so this one names only what nobody claims. The notice goes through the
// outbox like any other, so it is routed, held and re-held as they are.
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

// queuedRowCovers is whether a row the outbox already holds for this role covers its phase: a
// start of the phase that will still act, which carries the phase's task itself, or a stop that
// will still act, which is about to end the claim this ready came from. Both are StartFor's rule
// about queued rows, read without its question about the claim's own liveness: this claim is
// live, which is precisely why it is being given its task.
func queuedRowCovers(run record.RoleRun, root record.Issue, current phase.Phase) bool {
	var lastStart int64
	if run.Claim != nil {
		lastStart = run.Claim.LastStartRow
	}
	var stop int64
	for _, queued := range run.Queued {
		if StopActs(queued.ID, queued.Request, lastStart, &root) {
			stop = max(stop, queued.ID)
		}
	}
	if stop != 0 {
		return true
	}
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" && queued.Request.Phase == current {
			return true
		}
	}
	return false
}

// readyWorkerTask gives a phase worker whose claim has just become ready its phase's task again,
// when it holds that phase and has nothing pending. Every start of a worker is a start of work it
// must be told to do: the start row that moved the issue into this phase ran once, long before
// this launch, and after a restart — the worker's process relaunched, its claim retried, its
// session resumed — nothing else sends it anything. A tester that came back from a restart with no
// task waited, and its issue waited with it, for 301 hours across 48 waits
// (dispatch://INFRA-714 R7).
//
// The conditions are the ones a resume by rule already uses (resume.go's booted), read for this
// one claim: the issue is this project's and in the workflow, its phase's role (RoleFor) is this
// claim's, its tree does not linger, and its phase's own terminating fact has not arrived
// (PhaseFinished) — a merged root stands in merging for as long as the rest of its tree runs, and
// its merger must not be handed merging again at every launch. A claim that holds any task is left
// alone: it has work, and a second task would be a second prompt for work under way. So is a role
// a queued row already covers (queuedRowCovers).
//
// The task goes through the outbox's start like every other, marked as the phase's resume task, so
// the one rule about already-delivered work stays the outbox's: it does not deliver a resume task
// to a claim that by then holds its phase's task for this run. No slot is read: a claim whose
// agent has just said it is ready is running, and admission has long since given its tree a slot.
func (e *Engine) readyWorkerTask(ctx context.Context, tx pgx.Tx, fact intake.ClaimReady) error {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Project != e.cfg.Project || record.OutOfWorkflow(issue.Status) {
		return err
	}
	role := RoleFor(issue.Phase)
	if role == "" || role == claim.RoleArchitect || role != fact.Role {
		return nil
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || root == nil || root.Lingers() {
		return err
	}
	finished, err := e.phaseFinished(ctx, tx, *issue)
	if err != nil || finished {
		return err
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return err
	}
	token, err := claim.NewToken(project, issue.Key, role)
	if err != nil {
		return err
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
	if err != nil {
		return err
	}
	if held := run.Claim; held == nil || held.Pending != nil {
		return nil
	}
	if queuedRowCovers(run, *root, issue.Phase) {
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role,
		Generation: issue.Generation, Phase: issue.Phase, Task: ResumePhaseTask(*issue), ResumeTask: true}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a ready worker that holds its issue's phase with nothing pending is given its phase's task",
		"issue", issue.Key, "role", role, "phase", issue.Phase, "launch", fact.Launch)
	return nil
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
