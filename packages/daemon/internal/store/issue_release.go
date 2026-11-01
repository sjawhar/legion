package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// IssueReleased is one idle release of an issue's pod as its issue_release outbox row names it:
// the run it released (the issue's generation and its tree root's) and the row, written after the
// release's per-role stops. It is the release's counterpart to IssueClose, and differs in what it
// authorizes: a close acts on an issue the workflow has taken out, a release on an issue that is
// still in it and comes back at the next wake for one of its roles.
type IssueReleased struct {
	Issue, Tree                     string
	IssueGeneration, TreeGeneration uint64
	Row                             int64
}

// IssueReleaseStop decides whether one role stop of a release still acts, and is the one place
// the release's boundary is read. The boundary is the row that stops the first role process of
// the pod — the release's own first row, recorded as issue_releases.boundary_row when the
// release was marked. At that row, and only there, an outbox row addressed to a role of the pod
// sitting below it is a task, notice, message or start that was queued while every role of the
// pod still ran: it cancels the release, which this call records by forgetting the mark, and the
// roles go on in the same pod. A row queued above the boundary waits behind the release on the
// issue's lifecycle lane and resumes the pod once the release is complete, so nothing here reads
// it.
//
// A row below the boundary the lane already ran is gone from the outbox by then, and what it
// left behind is a claim whose last_start_row is above the mark's started_row — the newest the
// pod's claims held when the release was marked. That start was queued before the boundary too,
// and it cancels the release in this same place, which is why both halves are read here and
// nowhere else.
//
// It answers false for a release no mark holds — one already cancelled, or already finished — so
// every later stop of a cancelled release finishes without acting, and false for the cancellation
// it makes itself. The caller resumes what it has suspended and deletes any bundle it stored.
func (s *Store) IssueReleaseStop(ctx context.Context, project, issue string, row int64) (bool, error) {
	if project == "" || issue == "" || row <= 0 {
		return false, errors.New("a release's role stop requires its project, its issue and its outbox row")
	}
	var act bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var boundary, started int64
		err := tx.QueryRow(ctx, `select boundary_row, started_row from issue_releases where project = $1 and issue = $2`,
			project, issue).Scan(&boundary, &started)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row != boundary {
			act = true
			return nil
		}
		var queued, restarted bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from outbox where issue = $1 and id < $2 and kind = any($3))`,
			issue, boundary, idleWaitingKinds).Scan(&queued); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from claims where project = $1 and issue = $2 and last_start_row > $3)`,
			project, issue, started).Scan(&restarted); err != nil {
			return err
		}
		if queued || restarted {
			_, err := tx.Exec(ctx, `delete from issue_releases where project = $1 and issue = $2`, project, issue)
			return err
		}
		act = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("authorize the role stop of %s's release: %w", issue, err)
	}
	return act, nil
}

// IssueRelease decides whether an idle release may now save the workspace and stop its issue's
// Sandbox, in one short transaction under the shared serializer: false, finishing the release
// without acting, once the mark the release was started under is gone (a role stop found a
// delivery below the boundary and cancelled it), the run it released has moved on, the issue has
// left the workflow or its tree lingers — each of which is a close's business, not a release's —
// or the tree's cleanup is reserved, which deletes the Sandbox this release would; a wait while
// one of the release's own role stops is unfinished or a stored claim of the issue may still hold
// a process. outOfWorkflow is the workflow's rule for a status that takes an issue out of it
// (record.OutOfWorkflow), which the store does not restate.
//
// This is IssueSuspension's shape with its authorization inverted: a close acts only once its
// issue has left the workflow or its tree lingers, and a release acts only while the issue is
// still in the workflow and its tree is live. The two order against each other because each
// waits on the other's role stops: both read the same `suspend`, `issue_close` and `release` ops.
func (s *Store) IssueRelease(ctx context.Context, project string, release IssueReleased, outOfWorkflow func(status string) bool) (bool, error) {
	if release.Issue == "" || release.Tree == "" || release.IssueGeneration == 0 || release.TreeGeneration == 0 || release.Row <= 0 {
		return false, errors.New("issue release requires issue, tree, both generations and its outbox row")
	}
	var act bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		var marked bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from issue_releases where project = $1 and issue = $2)`,
			project, release.Issue).Scan(&marked); err != nil {
			return err
		}
		if !marked {
			return nil
		}
		var issueGeneration, rootGeneration int64
		var issuePhase, status, rootPhase, rootStatus string
		var lingering bool
		err := tx.QueryRow(ctx, `select i.generation, i.phase, i.status, root.generation, root.phase, root.status,
			root.linger_until is not null
			from issues i join issues root on root.key = i.tree where i.key = $1 and i.tree = $2`,
			release.Issue, release.Tree).Scan(&issueGeneration, &issuePhase, &status, &rootGeneration, &rootPhase, &rootStatus, &lingering)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		inWorkflow := !outOfWorkflow(status) && phase.Phase(issuePhase) != phase.Done &&
			!outOfWorkflow(rootStatus) && phase.Phase(rootPhase) != phase.Done && !lingering
		if uint64(issueGeneration) != release.IssueGeneration || uint64(rootGeneration) != release.TreeGeneration || !inWorkflow {
			return nil
		}
		var reserved bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from tree_lifecycles
			where project = $1 and tree = $2 and cleanup_started)`, project, release.Tree).Scan(&reserved); err != nil {
			return err
		}
		if reserved {
			return nil
		}
		var stopping bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from outbox where issue = $1 and id < $2 and kind = 'supervise'
			and payload->>'op' in ('suspend', 'issue_close', 'release') and (payload->>'generation')::bigint = $3)`,
			release.Issue, release.Row, release.IssueGeneration).Scan(&stopping); err != nil {
			return err
		}
		if stopping {
			return wait.Errorf("issue %s release waits for its role stop effects", release.Issue)
		}
		var pending string
		err = tx.QueryRow(ctx, `select token || ':' || state from claims where project = $1 and issue = $2 and `+claimMayRun+`
			order by token limit 1`, project, release.Issue).Scan(&pending)
		if err == nil {
			return wait.Errorf("issue %s release waits for stored claim %s", release.Issue, pending)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		act = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("authorize issue release: %w", err)
	}
	return act, nil
}
