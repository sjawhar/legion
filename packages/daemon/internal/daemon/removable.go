package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// removableWorkspaces is specs.removable's production body (dispatch://LEGION-583): every member
// of tree but exclude — the issue this launch is for — and the root itself, whose own state makes
// it safe to re-clone without costing a child mid-work a multi-minute wait: its phase is done, or
// it is parked — its Dispatch status is backlog or icebox, with every role's claim either never
// made or one of suspended, failed, or retired, never a live state. A child between phases
// (awaiting_merge, a parked review round holding todo or in_progress) is neither, and is never a
// candidate even though every claim of it may be idle between tasks: its own next phase can
// resume it without a re-clone, and taking the volume out from under it would force one. Each
// candidate is paired with the merged pull request's head the daemon recorded, when it merged:
// GitHub deletes a squash merge's branch, so that commit carries no remote bookmark of its own,
// and workspace-init's push-safety check needs the head to tell that commit from one that was
// never pushed at all. The actual push-safety check — whether a candidate's workspace in fact
// holds no commit that is not on GitHub — is workspace-init's alone, on the tree volume this
// daemon cannot read; this function names only who is lifecycle-safe to ask it about.
func removableWorkspaces(pool *pgxpool.Pool, records record.Store, sup *supervisor, project string) func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error) {
	return func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error) {
		var candidates []runtime.RemovableWorkspace
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			issues, err := records.TreeIssues(ctx, tx, tree)
			if err != nil {
				return fmt.Errorf("list the issues of tree %s: %w", tree, err)
			}
			for _, issue := range issues {
				if issue.Key == tree || issue.Key == exclude {
					continue
				}
				parked, err := issueIsParked(sup, project, issue)
				if err != nil {
					return err
				}
				if issue.Phase != phase.Done && !parked {
					continue
				}
				candidate := runtime.RemovableWorkspace{Issue: issue.Key}
				pr, err := records.PullRequest(ctx, tx, issue.Key)
				if err != nil {
					return fmt.Errorf("read the pull request of %s: %w", issue.Key, err)
				}
				if pr != nil && pr.State == record.PullRequestMerged {
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

// parkedStatuses are the Dispatch lifecycle statuses issueIsParked treats as parked: out of the
// workflow and not waiting to be admitted, so a claim idle in either one is not mid-phase work the
// daemon will resume on its own.
var parkedStatuses = []string{"backlog", "icebox"}

// parkedClaimStates are the only states issueIsParked lets a parked issue's claim hold: suspended,
// failed, or retired — never a live state (supervise.LiveStates), and never StateQueued, which is
// a claim the daemon still means to launch.
var parkedClaimStates = []supervise.ClaimState{supervise.StateSuspended, supervise.StateFailed, supervise.StateRetired}

// issueIsParked says whether issue is parked: its Dispatch status is one of parkedStatuses, and
// every role's claim is either never made (empty) or sits in one of parkedClaimStates. A claim in
// any other state — StateQueued included — means the daemon still has work queued for this issue,
// so it is not parked however its Dispatch status reads.
func issueIsParked(sup *supervisor, project string, issue record.Issue) (bool, error) {
	if !slices.Contains(parkedStatuses, issue.Status) {
		return false, nil
	}
	for _, role := range claim.Roles {
		token, err := claim.NewToken(project, issue.Key, role)
		if err != nil {
			return false, fmt.Errorf("derive the %s claim of %s: %w", role, issue.Key, err)
		}
		machine, found := sup.Machine(token)
		if !found {
			continue
		}
		if !slices.Contains(parkedClaimStates, machine.Claim().State) {
			return false, nil
		}
	}
	return true, nil
}
