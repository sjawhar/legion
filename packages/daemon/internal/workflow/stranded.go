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
// stranded when its issue's tree runs — the tree's root holds an admission slot and does not
// linger, the rule admittedRoot applies — nothing will wake it, and it is suspended or failed and
// one of:
//
//   - A worker of its issue's phase: the role RoleFor gives the phase its worker works
//     (workingPhase), which a held issue keeps from the phase it left. Only a start of that phase
//     retries a failed worker, so one is stranded once no such start is queued (StartFor, the rule
//     promotion reads). A suspended one is stranded when an operator suspended it, or when its
//     suspension was recorded before migration 0038 (unknown) and no start is queued for it: the
//     boot resumes neither, and a start queued for an unknown one resumes it. The daemon resumes
//     one it or its own exit suspended by rule (supervise's ByRule), at boot (DaemonBooted) and at
//     the phase's next start.
//   - Its tree's architect: only its tree's admission starts it, whoever suspended it. A notice to a
//     suspended one is held until it holds its role again, which nothing in the daemon brings
//     about, and a notice to a failed one finishes undelivered (the notice executor). Its tree
//     moves through its phases without it, and stops at the first decision that needs it. One
//     the daemon or its own exit suspended, or one that failed, is stranded once no admission start
//     is queued (StartFor).
//
// A suspension that holds for the operator is stranded whatever starts are queued for it: each one
// waits, on its row's backoff, until the operator resumes the claim (the outbox's start answers
// supervise.ErrHeldByOperator), so a queued start wakes nothing. The claim is held by the hold
// itself, whatever task it kept: the stop timeout takes a confirmed task back for the resume, and
// an unconfirmed phase task stays through the suspension (supervise's settle).
//
// The move that brings each one back is api.StrandedClaim.Move's.
//
// It names no other claim: a worker whose phase is not its own is started again when its phase
// comes, and a tree whose root holds no slot runs nothing yet. That is a lingering tree — a root
// that leaves the workflow, closed or moved out by a person, always lingers first (leave) — whose
// members its re-admission starts again; and a root re-admitted and waiting for a slot, whose
// architect admission starts once one frees (resumed sooner, it would run outside the cap). claims
// are the caller's transaction snapshot, as are the slots read here; the daemon's boot logs what
// this returns, and the state route serves it as strandedClaims.
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
	slots, err := s.Slots(ctx, tx)
	if err != nil {
		return nil, err
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
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
		if _, running := slotted[root.Key]; !running {
			continue
		}
		// current is the phase the start that would wake the claim names; the architect's names none.
		var current phase.Phase
		switch {
		case claim.IsTreeArchitect(c.Role, issue.Key, issue.Tree):
		case RoleFor(workingPhase(issue)) == c.Role:
			if c.State == supervise.StateSuspended && c.SuspendedBy.ByRule() {
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
			if candidate.claim.RuleHolds() || StartFor(run, candidate.root, candidate.issue.Generation, candidate.current) {
				stranded = append(stranded, candidate.claim)
			}
		}
	}
	slices.SortFunc(stranded, func(a, b supervise.Claim) int {
		return cmp.Or(cmp.Compare(a.Issue, b.Issue), cmp.Compare(a.Role, b.Role))
	})
	return stranded, nil
}
