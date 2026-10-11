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

const strandedRoleRunBatchSize = 200

// Stranded is every claim of s's issues that nothing will wake, by issue and then role. A claim is
// stranded when its issue's tree runs — the tree's root is recorded and does not linger, since
// linger holds every member where it stood and re-admission starts each one again — no start the
// outbox still holds will wake it (StartFor, the rule promotion reads), and it is suspended or
// failed and one of:
//
//   - A worker of its issue's phase: the role RoleFor gives the phase its worker works
//     (workingPhase), which a held issue keeps from the phase it left. Only a start of that phase
//     retries a failed worker, and that start has run. A suspended one is stranded only when an
//     operator suspended it, or its source predates migration 0038 (supervise's RuleMayResume): the
//     daemon resumes one it or its own exit suspended by rule, at boot (DaemonBooted) and at the
//     phase's next start. An operator's suspend waits for the agent's turn to end, so it leaves no
//     task a delivery would resume the claim for.
//   - Its tree's architect: only its tree's admission starts it, whoever suspended it. A notice to a
//     suspended one is held until it holds its role again, which nothing in the daemon brings
//     about, and a notice to a failed one finishes undelivered (the notice executor). Its tree
//     moves through its phases without it, and stops at the first decision that needs it.
//
// The operator's resume (`legion claims resume`) brings back a suspended one. A failed one ignores
// resume and deliver: only a workflow start retries it, its architect's retry of the held phase for
// a worker and its tree's re-admission for an architect.
// It names no other claim: a worker whose phase is not its own is started again when its phase
// comes, and a lingering tree's members wait for its re-admission. claims are the caller's
// transaction snapshot; the daemon's boot logs what this returns, and the state route serves it as
// strandedClaims.
//
// Candidates' queued starts and their claims are read through RoleRuns in bounded batches: a fleet
// of hundreds of suspended claims makes a few snapshot queries, never one pair per claim.
func Stranded(ctx context.Context, tx pgx.Tx, s record.Store, claims []supervise.Claim) ([]supervise.Claim, error) {
	issues, err := s.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	recorded := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		recorded[issue.Key] = issue
	}
	type candidate struct {
		claim   supervise.Claim
		issue   record.Issue
		root    record.Issue
		current phase.Phase
	}
	candidates := make([]candidate, 0, len(claims))
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
			if c.State == supervise.StateSuspended && c.RuleMayResume() {
				continue
			}
			current = issue.Phase
		default:
			continue
		}
		candidates = append(candidates, candidate{claim: c, issue: issue, root: root, current: current})
	}

	var stranded []supervise.Claim
	for start := 0; start < len(candidates); start += strandedRoleRunBatchSize {
		end := min(start+strandedRoleRunBatchSize, len(candidates))
		batch := candidates[start:end]
		keys := make([]record.RoleRunKey, len(batch))
		for i, candidate := range batch {
			keys[i] = record.RoleRunKey{Token: candidate.claim.Token, Issue: candidate.issue.Key, Role: candidate.claim.Role, Generation: candidate.issue.Generation}
		}
		runs, err := s.RoleRuns(ctx, tx, keys)
		if err != nil {
			return nil, err
		}
		for _, candidate := range batch {
			run := runs[candidate.claim.Token]
			if StartFor(run, candidate.root, candidate.issue.Generation, candidate.current) {
				stranded = append(stranded, candidate.claim)
			}
		}
	}
	slices.SortFunc(stranded, func(a, b supervise.Claim) int {
		return cmp.Or(cmp.Compare(a.Issue, b.Issue), cmp.Compare(a.Role, b.Role))
	})
	return stranded, nil
}
