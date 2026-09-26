package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// RoleFor is the role that works a phase: the one a transition starts, and the one it suspends
// when the issue moves on. A phase no role works — awaiting_merge, done, held — has none.
//
// It is exported for admission too, which starts the mid-phase children of a tree it re-admits
// and needs each child's own phase's role to start it on.
func RoleFor(p phase.Phase) claim.Role {
	switch p {
	case phase.Planning:
		return claim.RolePlanner
	case phase.Implementing, phase.Retro, phase.ProductionCheck:
		return claim.RoleImplementer
	case phase.Testing:
		return claim.RoleTester
	case phase.Reviewing:
		return claim.RoleReviewer
	case phase.Merging:
		return claim.RoleMerger
	default:
		return ""
	}
}

// SuspendApplies is whether a suspend still applies with the issue in phase current. leaves is the
// phase a transition's suspend ends (record.SuperviseRequest.Leaves): it stopped that phase's role
// because the issue left the role's phases, so once the issue is back in one of them the role has
// been handed its work again and the suspend must not stop it there. A suspend with no such phase
// (a linger's or a child's leave) stops every claim whatever phase its issue holds, and applies.
func SuspendApplies(leaves, current phase.Phase) bool {
	return leaves == "" || RoleFor(current) != RoleFor(leaves)
}

func (e *Engine) enqueue(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload) error {
	row, err := record.NewOutboxRow(issue, payload, e.now())
	if err != nil {
		return err
	}
	return e.store.Enqueue(ctx, tx, row)
}

func (e *Engine) status(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	if status == "" || issue.Status == status {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.StatusWrite{Status: status, ObservedStatus: issue.Status})
}

func (e *Engine) notice(ctx context.Context, tx pgx.Tx, issue string, notice record.Notice) error {
	return e.enqueue(ctx, tx, issue, notice)
}

// refused is a committed 409: the fact changed nothing, and the caller is told why.
func refused(code, message string) intake.Result {
	return intake.Result{Refusal: &intake.Refusal{Status: 409, Code: code, Message: message}}
}

// clearHandoff empties the handoff a role reported for its previous phase, keeping its claim and
// the review rounds, when a transition starts it on a new phase. A role's recorded handoff is then
// always its current phase's: the implementer's round-1 commit cannot advance round 2, and the
// production check is recorded only by the production check's own completion.
func (e *Engine) clearHandoff(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) error {
	if role == "" {
		return nil
	}
	row, err := e.phaseRow(ctx, tx, issue, role)
	if err != nil || row.HandoffCommit == "" && row.Verdict == "" && row.Decision == nil {
		return err
	}
	// The review round's decision ends with the round; the newest review id seen (ReviewSeen) does
	// not, since it orders every review the issue will have.
	row.HandoffCommit, row.Verdict, row.Decision = "", "", nil
	return e.store.PutPhase(ctx, tx, row)
}

// suspend stops the role the issue leaves, the transition's suspend stamped with the phase it ends.
func (e *Engine) suspend(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, leaves phase.Phase) error {
	if role == "" || role == claim.RoleArchitect {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "suspend", Tree: issue.Tree, Role: role, Generation: issue.Generation, Leaves: leaves})
}

// start starts the phase worker of the issue's current phase, a start stamped with that phase.
func (e *Engine) start(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string) error {
	if role == "" {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: task,
		Generation: issue.Generation, Phase: issue.Phase})
}

// task is what a started worker is told. It names the phase the worker starts, which the issue
// record already holds: one role runs several phases (the implementer runs implementing, retro,
// and production_check), and a resumed session cannot otherwise tell a new phase from its last.
func task(issue record.Issue, handoff record.PhaseRow, pr *record.PullRequest, reason string) string {
	text := continueLine(issue)
	if handoff.HandoffCommit != "" {
		text += " Handoff commit: " + handoff.HandoffCommit + "."
	}
	if handoff.Verdict != "" {
		text += " Handoff verdict: " + handoff.Verdict + "."
	}
	if handoff.HandoffCommit == "" && handoff.Verdict == "" && handoff.Claim != "" {
		text += " Resume the existing phase work."
	}
	if pr != nil && pr.Number != 0 {
		text += fmt.Sprintf(" Pull request: #%d.", pr.Number)
	}
	if reason != "" {
		text += " Reason: " + reason + "."
	}
	return text
}

func continueLine(issue record.Issue) string {
	return fmt.Sprintf("Continue %s. Issue: %s. Phase: %s.", issue.Title, issue.Key, issue.Phase)
}

// ResumePhaseTask is what a worker started for a phase already under way is told. No handoff
// begins that phase — the run that did is over — so the task names the phase and says to carry
// the work on, which is what a resumed session needs to tell this phase from its last. Admission
// sends it to the mid-phase children of a tree it re-admits, whose workers the tree's close
// retired and whose phase rows the new generation cleared.
func ResumePhaseTask(issue record.Issue) string {
	return continueLine(issue) + " Resume the existing phase work."
}

func (e *Engine) lingerAt() time.Time { return e.now().Add(e.cfg.Linger) }
