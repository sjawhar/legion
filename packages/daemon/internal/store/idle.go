package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// idleWaitingKinds are the outbox kinds that reach a role's own session, which is what "a task, a
// notice, a message or a start waits for a role of the pod" means: a supervise row (a start, the
// task it carries, a stop), an architect's notice, and the message a phase's end sends the role
// whose phase ended. The issue's board writes — its Dispatch status, its merge-queue packet —
// reach no session and hold no pod. The same set is what the release's boundary is read over
// (IssueRelease), so the rule that holds a pod and the rule that cancels its release never
// disagree about which row speaks to a role.
var idleWaitingKinds = []string{"supervise", "notice", "phase_ended"}

// IdleReleaseRow is one outbox row a release writes, encoded by its caller (record.NewOutboxRow),
// in the order the issue's lifecycle lane runs them: the save of the workspace's unpushed work,
// then one role stop per live claim, then the teardown. The store writes them rather than
// record's Enqueue because the boundary row's id must be read back under the same transaction.
//
// Boundary marks the one row whose id is the release's boundary: the stop of the first role
// process of the pod, which is what every later decision about this release reads
// (IssueRelease). The save before it is not the boundary — it stops nothing, so a delivery
// queued while it runs still cancels the whole release.
type IdleReleaseRow struct {
	Kind     string
	Payload  []byte
	Boundary bool
}

// IdleReleaseMark is what a decision to release writes: the rows above and the role whose shim
// the bundle is asked of. Rows empty means the pod is not released, and nothing is written.
type IdleReleaseMark struct {
	Generation uint64
	Role       claim.Role
	Rows       []IdleReleaseRow
}

// IdlePods is every issue of project whose pod has a live claim — one neither failed, retired nor
// already suspended — in key order: the issues the idle tick decides. An issue whose claims are
// all stopped has no role to ask for a bundle and no claim to suspend, so there is nothing for a
// release to do with it.
func (s *Store) IdlePods(ctx context.Context, project string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select distinct issue from claims
		where project = $1 and state not in ('failed', 'retired', 'suspended') and issue <> '' order by issue`, project)
	if err != nil {
		return nil, fmt.Errorf("list the pods of %s: %w", project, err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			return nil, fmt.Errorf("list the pods of %s: %w", project, err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the pods of %s: %w", project, err)
	}
	return issues, nil
}

// ReleaseIdleIssue is the release's one transaction: it reads issue's idle view and hands it to
// decide, and when decide answers rows it writes them and the release's mark together, so the
// view the decision rests on and the mark that records it are one atomic act.
//
// The mark records the ids the rows were given: first_row and last_row are the release's own
// first and last, which is how a row of this release is told from a row of one that is over
// (IssueReleaseStop, IssueRelease), and boundary_row is the row the mark names its boundary —
// the stop of the first role process of the pod — which is where a delivery that reached the
// pod after the mark cancels the release.
//
// It answers whether a release was marked. decide answering no rows writes nothing, which is the
// ordinary case: nearly every tick finds a pod that is working, or one whose idle period has not
// passed.
func (s *Store) ReleaseIdleIssue(ctx context.Context, project, issue string, decide func(pods.IdleView) (IdleReleaseMark, error)) (bool, error) {
	if project == "" || issue == "" {
		return false, errors.New("an idle release needs its project and its issue")
	}
	var marked bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		view, err := readIdleView(ctx, tx, project, issue)
		if err != nil {
			return err
		}
		mark, err := decide(view)
		if err != nil || len(mark.Rows) == 0 {
			return err
		}
		if view.Releasing {
			return nil
		}
		var first, boundary, last int64
		for _, row := range mark.Rows {
			var id int64
			if err := tx.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
				values ($1, $2, $3, 0, now(), '', '') returning id`, row.Kind, issue, string(row.Payload)).Scan(&id); err != nil {
				return fmt.Errorf("queue the %s of %s's release: %w", row.Kind, issue, err)
			}
			if first == 0 {
				first = id
			}
			last = id
			if row.Boundary {
				boundary = id
			}
		}
		if boundary == 0 {
			return fmt.Errorf("the release of %s named no boundary row", issue)
		}
		var started int64
		for _, c := range view.Claims {
			started = max(started, c.LastStartRow)
		}
		if _, err := tx.Exec(ctx, `insert into issue_releases
			(project, issue, generation, first_row, boundary_row, last_row, started_row, role, marked_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
			project, issue, int64(mark.Generation), first, boundary, last, started, string(mark.Role)); err != nil {
			return fmt.Errorf("mark the release of %s: %w", issue, err)
		}
		marked = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("release the idle pod of %s: %w", issue, err)
	}
	return marked, nil
}

// ClearIssueRelease forgets the mark of issue's release, whether the release finished or was
// cancelled, on the pool: the teardown's own call, outside any transaction. A mark already gone
// is success, so a retried finish finishes.
func (s *Store) ClearIssueRelease(ctx context.Context, project, issue string) error {
	if err := deleteIssueRelease(ctx, s.pool, project, issue); err != nil {
		return fmt.Errorf("clear the release mark of %s: %w", issue, err)
	}
	return nil
}

// deleteIssueRelease forgets the mark of issue's release through db, which is the pool for the
// teardown's own call and the caller's transaction for a role stop that cancels the release
// where it reads the boundary (IssueReleaseStop). One mark, one delete, whichever runs it.
func deleteIssueRelease(ctx context.Context, db execer, project, issue string) error {
	_, err := db.Exec(ctx, `delete from issue_releases where project = $1 and issue = $2`, project, issue)
	return err
}

func readIdleView(ctx context.Context, tx pgx.Tx, project, issue string) (pods.IdleView, error) {
	view := pods.IdleView{Issue: issue}
	var issueGeneration, rootGeneration int64
	var issuePhase, rootPhase string
	err := tx.QueryRow(ctx, `select i.tree, i.phase, i.status, i.generation, i.linger_until,
		root.phase, root.status, root.generation, root.linger_until,
		root.phase = 'awaiting_person', root.awaiting_answered_at is not null
		from issues i join issues root on root.key = i.tree where i.key = $1`, issue).Scan(
		&view.Tree, &issuePhase, &view.Status, &issueGeneration, &view.LingerUntil,
		&rootPhase, &view.Root.Status, &rootGeneration, &view.Root.LingerUntil,
		&view.Root.AwaitingPerson, &view.Root.Answered)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return view, nil
	case err != nil:
		return pods.IdleView{}, fmt.Errorf("read %s for its idle view: %w", issue, err)
	}
	view.Found = true
	view.Phase, view.Root.Phase = phase.Phase(issuePhase), phase.Phase(rootPhase)
	view.Generation, view.Root.Generation = uint64(issueGeneration), uint64(rootGeneration)

	claims, err := tx.Query(ctx, `select c.role, c.state, c.plugin_contract, c.serving_generation,
		c.last_start_row, c.activity_at, c.activity_background, c.activity_kinds, c.activity_next_reminder_at, c.updated_at,
		greatest(d.delivered_at, d.confirmed_at)
		from claims c left join pending_task_deliveries d on d.claim_token = c.token
		where c.project = $1 and c.issue = $2 order by c.token`, project, issue)
	if err != nil {
		return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
	}
	defer claims.Close()
	for claims.Next() {
		var c pods.IdleClaim
		var role, state, kinds string
		var servingGeneration int64
		var activityAt, reminder, lastTurn *time.Time
		if err := claims.Scan(&role, &state, &c.PluginContract, &servingGeneration, &c.LastStartRow,
			&activityAt, &c.ActivityBackground, &kinds, &reminder, &c.UpdatedAt, &lastTurn); err != nil {
			return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
		}
		c.Role, c.State = claim.Role(role), supervise.ClaimState(state)
		c.ServingGeneration = uint64(servingGeneration)
		c.ActivityAt, c.ActivityNextReminderAt, c.LastTurnAt = orZero(activityAt), orZero(reminder), orZero(lastTurn)
		if kinds != "" {
			c.ActivityKinds = strings.Split(kinds, ",")
		}
		view.Claims = append(view.Claims, c)
	}
	if err := claims.Err(); err != nil {
		return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
	}

	if err := tx.QueryRow(ctx, `select exists (
		select 1 from outbox where issue = $1 and kind = any($2))`, issue, idleWaitingKinds).Scan(&view.Waiting); err != nil {
		return pods.IdleView{}, fmt.Errorf("read the queued rows of %s for its idle view: %w", issue, err)
	}
	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_releases where project = $1 and issue = $2)`,
		project, issue).Scan(&view.Releasing); err != nil {
		return pods.IdleView{}, fmt.Errorf("read whether %s is being released: %w", issue, err)
	}
	return view, nil
}
