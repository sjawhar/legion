package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// claimReady gives a tree's root architect its first instruction. Nothing else starts a turn of it:
// admission's start launches it with no task, and a re-admitted tree resumes its session. So at
// each launch's ready the daemon sends it a catch-up notice of what its tree holds (catchUp), and
// so does a re-admission's start that finds the claim still running (the outbox's start), as the
// TypeScript daemon sends the overseer catch-up at every ready of an active root (onTreeReady). A
// relaunch in the same generation is told again: it may be an architect that died during the turn a
// catch-up started, resumed idle, or a fresh agent whose workspace was lost, and nothing else wakes
// either. The ready is one fact per launch (workflowRuntime.applyTerminal keys it by the claim's
// launch generation), so the boot's replay of a ready already applied tells nothing more, and one
// never applied tells it once. A catch-up an earlier launch never had delivered is dropped first,
// so the agent is given one catch-up, computed now; it names the launch it was written for, so a
// failed forward of an older one is not queued again behind it (the outbox's rehold). The notice
// goes through the outbox like any other, so it is routed, held and re-held as they are. A tree
// that lingers or has left the workflow is told nothing, nor is a sub-architect (no workflow path
// starts one). A phase worker's ready is answered differently: it is given its phase's task again
// when it holds that phase with nothing pending (readyWorkerTask).
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
	catchUp, awaiting, err := e.catchUp(ctx, tx, *root)
	if err != nil {
		return intake.Result{}, err
	}
	catchUp.Launch = fact.Launch
	if err := e.store.DropCatchUps(ctx, tx, root.Key); err != nil {
		return intake.Result{}, err
	}
	reason := fmt.Sprintf("the tree of %s at generation %d as the daemon records it now; start or resume it as your role says", root.Key, root.Generation)
	if len(awaiting) > 0 {
		reason += fmt.Sprintf("; in accepting, waiting for your acceptance, which no worker gives: %s (read the evidence, then sign_off or request_backward_move)", strings.Join(awaiting, ", "))
	}
	return intake.Result{}, e.notice(ctx, tx, root.Key, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: reason, CatchUp: &catchUp})
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

// catchUp is root's tree as the record holds it: whether a hand-over created the root at this
// generation, and whether its old root's gate came with it, the design gate policy and the root's
// gate, and every issue of the tree in key order. With it come the keys, in key order, of the
// tree's issues waiting for the architect's acceptance: each in accepting and still in the
// workflow. No worker runs in accepting, and the one phase-finished notice that moved an issue
// there is ended undelivered when the architect's claim has failed (the outbox's notice), so a
// retried architect learns of the wait from its catch-up alone; the reason names these keys so
// that the catch-up, which a revived architect reads as the tree already running, says what is
// the architect's own to do.
func (e *Engine) catchUp(ctx context.Context, tx pgx.Tx, root record.Issue) (record.CatchUp, []string, error) {
	catchUp := record.CatchUp{Generation: root.Generation, Gate: record.CatchUpGate{Policy: string(e.cfg.DesignGate)}}
	handed, err := e.store.HandedOverRoot(ctx, tx, root.Key)
	if err != nil {
		return record.CatchUp{}, nil, err
	}
	if handed != nil && handed.Generation == root.Generation {
		catchUp.HandedOver, catchUp.Carried = true, handed.GateCarried
	}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil {
		return record.CatchUp{}, nil, err
	}
	if gate != nil {
		catchUp.Gate.Artifact, catchUp.Gate.Version, catchUp.Gate.Open = gate.ArtifactID, gate.LatestVersion, classify.DesignGateOpen(*gate)
	}
	issues, err := e.store.TreeIssues(ctx, tx, root.Tree)
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
