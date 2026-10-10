package workflow

import (
	"cmp"
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Stranded is every claim of s's issues that nothing will wake, by issue and then role. A claim is
// stranded when its issue's tree runs — the tree's root is recorded and does not linger, since
// linger holds every member where it stood and re-admission starts each one again — no start the
// outbox still holds will wake it (StartFor, the rule promotion reads), and it is suspended or
// failed and one of:
//
//   - A worker of its issue's phase: the role RoleFor gives the phase its worker works
//     (workingPhase), which a held issue keeps from the phase it left. Only a start of that phase
//     resumes a suspended worker or retries a failed one, and that start has run. An operator's
//     suspend waits for the agent's turn to end, so it leaves no task a delivery would resume the
//     claim for.
//   - Its tree's architect: only its tree's admission starts it. A notice to a suspended one is
//     held until it holds its role again, which nothing in the daemon brings about, and a notice to
//     a failed one finishes undelivered (the notice executor). Its tree moves through its phases
//     without it, and stops at the first decision that needs it.
//
// The operator's resume (`legion claims resume`) brings back a suspended one. A failed one ignores
// resume and deliver: only a workflow start retries it, its architect's retry of the held phase for
// a worker and its tree's re-admission for an architect.
//
// It names no other claim: a worker whose phase is not its own is started again when its phase
// comes, and a lingering tree's members wait for its re-admission. claims are the supervisor's;
// the daemon's boot logs what this returns, and the state route serves it as strandedClaims.
func Stranded(ctx context.Context, tx pgx.Tx, s record.Store, claims []supervise.Claim) ([]supervise.Claim, error) {
	issues, err := s.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	recorded := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		recorded[issue.Key] = issue
	}
	var stranded []supervise.Claim
	for _, c := range claims {
		if c.State != supervise.StateSuspended && c.State != supervise.StateFailed {
			continue
		}
		issue, ok := recorded[c.Issue]
		if !ok {
			continue
		}
		root, ok := recorded[issue.Tree]
		if !ok || root.Lingers() {
			continue
		}
		// current is the phase the start that would wake the claim names; the architect's names none.
		var current phase.Phase
		switch {
		case claim.IsTreeArchitect(c.Role, issue.Key, issue.Tree):
		case RoleFor(workingPhase(issue)) == c.Role:
			current = issue.Phase
		default:
			continue
		}
		run, err := s.RoleRun(ctx, tx, c.Token, issue.Key, c.Role, issue.Generation)
		if err != nil {
			return nil, err
		}
		if StartFor(run, root, issue.Generation, current) {
			stranded = append(stranded, c)
		}
	}
	slices.SortFunc(stranded, func(a, b supervise.Claim) int {
		return cmp.Or(cmp.Compare(a.Issue, b.Issue), cmp.Compare(a.Role, b.Role))
	})
	return stranded, nil
}
