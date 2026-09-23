package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func roleFor(p phase.Phase) claim.Role {
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

func (e *Engine) suspend(ctx context.Context, tx pgx.Tx, issue, tree string, role claim.Role) error {
	if role == "" || role == claim.RoleArchitect {
		return nil
	}
	return e.enqueue(ctx, tx, issue, record.SuperviseRequest{Op: "suspend", Tree: tree, Role: role})
}

func (e *Engine) start(ctx context.Context, tx pgx.Tx, issue, tree string, role claim.Role, task string) error {
	if role == "" {
		return nil
	}
	return e.enqueue(ctx, tx, issue, record.SuperviseRequest{Op: "start", Tree: tree, Role: role, Task: task})
}

// task is what a started worker is told. It names the phase the worker starts, which the issue
// record already holds: one role runs several phases (the implementer runs implementing, retro,
// and production_check), and a resumed session cannot otherwise tell a new phase from its last.
func task(issue record.Issue, handoff record.PhaseRow, pr *record.PullRequest, reason string) string {
	text := fmt.Sprintf("Continue %s. Issue: %s. Phase: %s.", issue.Title, issue.Key, issue.Phase)
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

func (e *Engine) lingerAt() time.Time { return e.now().Add(e.cfg.LingerHours) }
