package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// removableWorkspaces is specs.removable's production body (dispatch://LEGION-583): every member
// of tree but exclude — the issue this launch is for — and the root itself, whose own state makes
// it safe to re-clone without costing a child mid-work a multi-minute wait: its phase is done, and
// every role's claim on it is either never made or one of suspended, failed, or retired — never a
// live state, and never StateQueued, which is a claim the daemon still means to launch. The claim
// check applies even to a done phase: leave (workflow/linger.go) sets a left child's phase to done
// in the same transaction as the status write that takes it out of the workflow (done, backlog,
// icebox, or triage alike — every one of them forces phase done, so phase done already covers the
// spec's "done, or parked" without a separate Dispatch-status branch), but only *enqueues* the
// claim suspends; the outbox applies them later, so a probe in that window can see phase done on
// an issue whose claim is still working. A child merely between phases (awaiting_merge, a review
// round the architect has not yet decided) never reaches phase done at all, and is never a
// candidate even though every claim of it may be idle between tasks: its own next phase can resume
// it without a re-clone, and taking the volume out from under it would force one. Each candidate is
// paired with the merged pull request's head the daemon recorded, when it merged: GitHub deletes a
// squash merge's branch, so that commit carries no remote bookmark of its own, and
// workspace-init's push-safety check needs the head to tell that commit from one that was never
// pushed at all. The actual push-safety check — whether a candidate's workspace in fact holds no
// commit that is not on GitHub — is workspace-init's alone, on the tree volume this daemon cannot
// read; this function names only who is lifecycle-safe to ask it about.
//
// This runs inside Machine.Handle for the launching claim's own machine, which holds that
// machine's mutex for the whole transition (supervisor.go:188-189's rule: code reached this way
// reads no other machine). sup.Claims below reads the store directly and takes no machine's lock
// at all, so two done siblings launching at once — each inside its own Handle, each computing this
// same list — never wait on each other: calling sup.Machine(token) and Claim() here, as an earlier
// version did, deadlocks exactly that way (AB-BA on the two machines' mutexes) and was fixed by
// this function never touching a machine again.
func removableWorkspaces(pool *pgxpool.Pool, records record.Store, sup *supervisor, project string) func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error) {
	return func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error) {
		claims, err := sup.Claims(ctx)
		if err != nil {
			return nil, fmt.Errorf("read claims for tree %s: %w", tree, err)
		}
		live := liveIssues(claims)
		var candidates []runtime.RemovableWorkspace
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			issues, err := records.TreeIssues(ctx, tx, tree)
			if err != nil {
				return fmt.Errorf("list the issues of tree %s: %w", tree, err)
			}
			var issueKeys []string
			for _, issue := range issues {
				if issue.Key == tree || issue.Key == exclude || issue.Phase != phase.Done || live[issue.Key] {
					continue
				}
				issueKeys = append(issueKeys, issue.Key)
			}
			// One query for every candidate's pull request, instead of one query per candidate:
			// a tree this pass exists to shrink can hold many done siblings at once.
			prs, err := records.PullRequestsByIssue(ctx, tx, issueKeys)
			if err != nil {
				return fmt.Errorf("read the pull requests of %d candidate(s) of tree %s: %w", len(issueKeys), tree, err)
			}
			for _, key := range issueKeys {
				candidate := runtime.RemovableWorkspace{Issue: key}
				if pr, ok := prs[key]; ok && pr.State == record.PullRequestMerged {
					candidate.MergedHead = pr.HeadSHA
				}
				candidates = append(candidates, candidate)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("compute removable workspaces of tree %s: %w", tree, err)
		}
		return candidates, nil
	}
}

// liveIssues says, for every issue claims names, whether any of its role claims sits outside
// supervise.GoneStates (suspended, failed, or retired — never StateQueued, which is a claim the
// daemon still means to launch) — an issue with no claim at all is simply absent from the result,
// which reads as not live. claims is sup.Claims' own result, read from the store with no machine's
// lock taken (removableWorkspaces' own doc comment says why that matters): leave's phase-done
// write and its claim suspends are not one transaction (workflow/linger.go enqueues the suspends
// for the outbox to run), so a done issue can still show a live claim in the window before the
// outbox catches up; this is what keeps such an issue out of the result until it does.
func liveIssues(claims []supervise.Claim) map[string]bool {
	gone := supervise.GoneStates()
	live := make(map[string]bool, len(claims))
	for _, c := range claims {
		if !slices.Contains(gone, c.State) {
			live[c.Issue] = true
		}
	}
	return live
}
