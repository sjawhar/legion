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

// IdleClaim is one claim of an issue's pod as the idle rule reads it. Every field comes from the
// claims table and none from a live machine: the four activity columns are written by the
// session's activity route alone and are deliberately absent from putClaim's conflict update, so
// a machine's in-memory claim shows their zero values until the claim is read again.
type IdleClaim struct {
	Token             claim.Token
	Role              claim.Role
	State             supervise.ClaimState
	SuspendedBy       supervise.SuspendSource
	PluginContract    int
	ServingGeneration uint64
	// LastStartRow is the newest start row the outbox ran against the claim. The release records
	// the highest of the pod's as its mark's started_row, which is what tells a start queued
	// before the boundary and already run from one of the release's own rows.
	LastStartRow int64
	// LastTurnAt is the newest moment a turn of this claim is known to have been under way: the
	// latest of its pending delivery's send acknowledgement and the confirmation of the turn that
	// send started, zero for a claim holding no delivery. A session's report counts only when it
	// was read after it (IdleClaim.Reported).
	LastTurnAt time.Time
	// ActivityAt, ActivityBackground, ActivityKinds and ActivityNextReminderAt are the session's
	// last activity report (POST /legion/v1/claims/activity): when it read what it was doing,
	// whether it found background work of its own running, what kinds it named, and when the
	// phase reminder armed in it is due. ActivityAt is zero for a session that has reported none.
	ActivityAt             time.Time
	ActivityBackground     bool
	ActivityKinds          []string
	ActivityNextReminderAt time.Time
	// UpdatedAt is when the claim's row was last written: every state the machine persisted and
	// every activity report. The newest UpdatedAt of a pod's claims is when the pod last did
	// anything the daemon recorded, which is what the idle period is measured from.
	UpdatedAt time.Time
}

// Live is whether the claim still holds its place in the pod: a failed, retired or already
// suspended claim stays as it is and holds no pod.
func (c IdleClaim) Live() bool {
	switch c.State {
	case supervise.StateFailed, supervise.StateRetired, supervise.StateSuspended:
		return false
	}
	return true
}

// Reported is whether the session read what it was doing after its last turn: a claim with no
// report at all, or one whose newest report predates its last turn, has said nothing about the
// turn that just ran. Such a claim is never released, which is what makes a host that cannot
// report degrade safely rather than lose work it was still doing.
func (c IdleClaim) Reported() bool {
	return !c.ActivityAt.IsZero() && !c.ActivityAt.Before(c.LastTurnAt)
}

// IdleWaiting is one queued outbox row of the issue addressed to a role of its pod: the row's id,
// its kind, and the role its payload names ("" for a kind that names none).
type IdleWaiting struct {
	ID   int64
	Kind string
	Role claim.Role
}

// IdleRoot is the tree root of the issue the idle rule is deciding, read with it.
type IdleRoot struct {
	Key        string
	Phase      phase.Phase
	Status     string
	Generation uint64
	Lingering  bool
	// AwaitingPerson is whether the root waits on a person (phase awaiting_person) and Answered
	// whether that person has answered, which leaves the root waiting only for a slot.
	AwaitingPerson bool
	Answered       bool
}

// IdleView is one issue's pod as the idle rule reads it, in one read per issue: every claim of
// the pod, every queued outbox row addressed to one of its roles, the issue's own place in the
// workflow and its tree root's. Found is false for an issue the record does not hold, which is no
// pod of the workflow's to release.
type IdleView struct {
	Issue      string
	Tree       string
	Found      bool
	Phase      phase.Phase
	Status     string
	Generation uint64
	Lingering  bool
	Root       IdleRoot
	Claims     []IdleClaim
	Waiting    []IdleWaiting
	// Releasing is whether a release of this pod is already marked in progress, which no second
	// release starts behind.
	Releasing bool
}

// IsRoot is whether the issue is its tree's root, the one issue a root's own wait on a person
// holds: the children of a root that waits on a person are released like any other issue.
func (v IdleView) IsRoot() bool { return v.Issue == v.Tree }

// QuietSince is when anything of the pod was last recorded — the newest claim write of any of its
// claims, state and activity report alike — and false while the pod has no claim at all.
func (v IdleView) QuietSince() (time.Time, bool) {
	var newest time.Time
	for _, c := range v.Claims {
		if c.UpdatedAt.After(newest) {
			newest = c.UpdatedAt
		}
	}
	return newest, !newest.IsZero()
}

// IdleReleaseRow is one outbox row a release writes, encoded by its caller (record.NewOutboxRow),
// in the order the issue's lifecycle lane runs them: one role stop per live claim, then the
// teardown. The store writes them rather than record's Enqueue because the first row's id is the
// release's boundary and must be read back under the same transaction.
type IdleReleaseRow struct {
	Kind    string
	Payload []byte
}

// IdleReleaseMark is what a decision to release writes: the rows above and the role whose
// launcher the bundle is asked of. Rows empty means the pod is not released, and nothing is
// written.
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
// view the decision rests on and the mark that records it are one atomic act. The mark's
// boundary_row is the first row's id — the stop of the first role process of the pod — which is
// the one fact every later decision about this release reads (IssueRelease).
//
// It answers whether a release was marked. decide answering no rows writes nothing, which is the
// ordinary case: nearly every tick finds a pod that is working, or one whose idle period has not
// passed.
func (s *Store) ReleaseIdleIssue(ctx context.Context, project, issue string, decide func(IdleView) (IdleReleaseMark, error)) (bool, error) {
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
		var boundary int64
		for i, row := range mark.Rows {
			var id int64
			if err := tx.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
				values ($1, $2, $3, 0, now(), '', '') returning id`, row.Kind, issue, string(row.Payload)).Scan(&id); err != nil {
				return fmt.Errorf("queue the %s of %s's release: %w", row.Kind, issue, err)
			}
			if i == 0 {
				boundary = id
			}
		}
		var started int64
		for _, c := range view.Claims {
			started = max(started, c.LastStartRow)
		}
		if _, err := tx.Exec(ctx, `insert into issue_releases (project, issue, generation, boundary_row, started_row, role, marked_at)
			values ($1, $2, $3, $4, $5, $6, now())`, project, issue, int64(mark.Generation), boundary, started, string(mark.Role)); err != nil {
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
// cancelled. A mark already gone is success, so a retried finish finishes.
func (s *Store) ClearIssueRelease(ctx context.Context, project, issue string) error {
	if _, err := s.pool.Exec(ctx, `delete from issue_releases where project = $1 and issue = $2`, project, issue); err != nil {
		return fmt.Errorf("clear the release mark of %s: %w", issue, err)
	}
	return nil
}

// IdleView is issue's pod as the idle rule reads it, in one transaction of its own: the tick's
// read, and the same read ReleaseIdleIssue makes before it decides.
func (s *Store) IdleView(ctx context.Context, project, issue string) (IdleView, error) {
	var view IdleView
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		view, err = readIdleView(ctx, tx, project, issue)
		return err
	})
	if err != nil {
		return IdleView{}, fmt.Errorf("read the idle view of %s: %w", issue, err)
	}
	return view, nil
}

func readIdleView(ctx context.Context, tx pgx.Tx, project, issue string) (IdleView, error) {
	view := IdleView{Issue: issue}
	var issueGeneration, rootGeneration int64
	var issuePhase, rootPhase string
	err := tx.QueryRow(ctx, `select i.tree, i.phase, i.status, i.generation, i.linger_until is not null,
		root.key, root.phase, root.status, root.generation, root.linger_until is not null,
		root.phase = 'awaiting_person', root.awaiting_answered_at is not null
		from issues i join issues root on root.key = i.tree where i.key = $1`, issue).Scan(
		&view.Tree, &issuePhase, &view.Status, &issueGeneration, &view.Lingering,
		&view.Root.Key, &rootPhase, &view.Root.Status, &rootGeneration, &view.Root.Lingering,
		&view.Root.AwaitingPerson, &view.Root.Answered)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return view, nil
	case err != nil:
		return IdleView{}, fmt.Errorf("read %s for its idle view: %w", issue, err)
	}
	view.Found = true
	view.Phase, view.Root.Phase = phase.Phase(issuePhase), phase.Phase(rootPhase)
	view.Generation, view.Root.Generation = uint64(issueGeneration), uint64(rootGeneration)

	claims, err := tx.Query(ctx, `select c.token, c.role, c.state, c.suspended_by, c.plugin_contract, c.serving_generation,
		c.last_start_row, c.activity_at, c.activity_background, c.activity_kinds, c.activity_next_reminder_at, c.updated_at,
		greatest(d.delivered_at, d.confirmed_at)
		from claims c left join pending_task_deliveries d on d.claim_token = c.token
		where c.project = $1 and c.issue = $2 order by c.token`, project, issue)
	if err != nil {
		return IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
	}
	defer claims.Close()
	for claims.Next() {
		var c IdleClaim
		var token, role, state, by, kinds string
		var servingGeneration int64
		var activityAt, reminder, lastTurn *time.Time
		if err := claims.Scan(&token, &role, &state, &by, &c.PluginContract, &servingGeneration, &c.LastStartRow,
			&activityAt, &c.ActivityBackground, &kinds, &reminder, &c.UpdatedAt, &lastTurn); err != nil {
			return IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
		}
		c.Token, c.Role = claim.Token(token), claim.Role(role)
		c.State, c.SuspendedBy = supervise.ClaimState(state), supervise.SuspendSource(by)
		c.ServingGeneration = uint64(servingGeneration)
		c.ActivityAt, c.ActivityNextReminderAt, c.LastTurnAt = orZero(activityAt), orZero(reminder), orZero(lastTurn)
		if kinds != "" {
			c.ActivityKinds = strings.Split(kinds, ",")
		}
		view.Claims = append(view.Claims, c)
	}
	if err := claims.Err(); err != nil {
		return IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
	}

	waiting, err := tx.Query(ctx, `select id, kind, coalesce(payload->>'role', '') from outbox
		where issue = $1 and kind = any($2) order by id`, issue, idleWaitingKinds)
	if err != nil {
		return IdleView{}, fmt.Errorf("read the queued rows of %s for its idle view: %w", issue, err)
	}
	defer waiting.Close()
	for waiting.Next() {
		var row IdleWaiting
		var role string
		if err := waiting.Scan(&row.ID, &row.Kind, &role); err != nil {
			return IdleView{}, fmt.Errorf("read the queued rows of %s for its idle view: %w", issue, err)
		}
		row.Role = claim.Role(role)
		view.Waiting = append(view.Waiting, row)
	}
	if err := waiting.Err(); err != nil {
		return IdleView{}, fmt.Errorf("read the queued rows of %s for its idle view: %w", issue, err)
	}

	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_releases where project = $1 and issue = $2)`,
		project, issue).Scan(&view.Releasing); err != nil {
		return IdleView{}, fmt.Errorf("read whether %s is being released: %w", issue, err)
	}
	return view, nil
}
