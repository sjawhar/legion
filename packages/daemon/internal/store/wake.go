package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// releasedClaimSelect reads the columns a wake needs of a released claim: a claim this daemon's
// project holds, suspended with the source `released` (supervise.Claim.Released), which is the
// one state a wake acts on. Every other suspended claim is somebody else's to resume — an
// operator's, a close's — and a path that takes a released claim over (an operator's suspend, a
// linger) re-stamps its source, which takes it out of every read here.
//
// It is deliberately not claimSelect: a wake reads no delivery and no launch state, only who the
// claim is, the session it kept and the run it was serving.
const releasedClaimSelect = `select token, project, tree, issue, role, session, serving_generation
	from claims where project = $1 and state = $2 and suspended_by = $3`

func scanReleasedClaims(rows pgx.Rows) ([]supervise.Claim, error) {
	defer rows.Close()
	var out []supervise.Claim
	for rows.Next() {
		c := supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}
		var generation int64
		if err := rows.Scan(&c.Token, &c.Project, &c.Tree, &c.Issue, &c.Role, &c.Session, &generation); err != nil {
			return nil, err
		}
		if generation > 0 {
			c.ServingGeneration = uint64(generation)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReleasedClaimsOf is every released claim of issue, in token order, which is the set a wake of
// the whole pod resumes.
func ReleasedClaimsOf(ctx context.Context, tx pgx.Tx, project, issue string) ([]supervise.Claim, error) {
	rows, err := tx.Query(ctx, releasedClaimSelect+` and issue = $4 order by token`,
		project, string(supervise.StateSuspended), string(supervise.SuspendedByReleased), issue)
	if err != nil {
		return nil, fmt.Errorf("read the released claims of %s: %w", issue, err)
	}
	claims, err := scanReleasedClaims(rows)
	if err != nil {
		return nil, fmt.Errorf("read the released claims of %s: %w", issue, err)
	}
	return claims, nil
}

// oneReleasedClaim is the one released claim of project that where selects, and whether there is
// one.
func oneReleasedClaim(ctx context.Context, tx pgx.Tx, where, project, value, what string) (supervise.Claim, bool, error) {
	if value == "" {
		return supervise.Claim{}, false, nil
	}
	rows, err := tx.Query(ctx, releasedClaimSelect+" "+where+` order by token limit 1`,
		project, string(supervise.StateSuspended), string(supervise.SuspendedByReleased), value)
	if err != nil {
		return supervise.Claim{}, false, fmt.Errorf("read the released claim of %s %s: %w", what, value, err)
	}
	claims, err := scanReleasedClaims(rows)
	if err != nil {
		return supervise.Claim{}, false, fmt.Errorf("read the released claim of %s %s: %w", what, value, err)
	}
	if len(claims) == 0 {
		return supervise.Claim{}, false, nil
	}
	return claims[0], true, nil
}

// MarkPodWake records that the wake of kind named by key has been taken for issue, and answers
// whether this call was the one that took it. A wake that can be reported more than once — one
// head settles again as each of its checks finishes, a listener that reports the same role
// message twice — is taken once, because the mark's key is the wake's own identity and the insert
// is conditional on it.
func MarkPodWake(ctx context.Context, tx pgx.Tx, project, issue, kind, key string, at time.Time) (bool, error) {
	if project == "" || issue == "" || kind == "" || key == "" {
		return false, errors.New("a pod wake mark needs its project, issue, kind and key")
	}
	tag, err := tx.Exec(ctx, `insert into pod_wake_marks (project, issue, kind, key, at)
		values ($1, $2, $3, $4, $5) on conflict do nothing`, project, issue, kind, key, at)
	if err != nil {
		return false, fmt.Errorf("mark the %s wake of %s: %w", kind, issue, err)
	}
	return tag.RowsAffected() > 0, nil
}

// CancelPodBackstops drops the open backstops of issue that role armed, and every open backstop
// of the issue when role is empty. A wake resumes one role, and it is that role's backstop alone
// that has nothing left to do: another role of the same pod may be waiting on a deploy run with
// nothing but its own backstop to bring it back, and a pod woken for one role may be held again
// before it ever reaches that other role's work. A path that takes the whole released pod over
// (ForgetPodWakes) names no role, because no backstop of it is left to act on.
func CancelPodBackstops(ctx context.Context, tx pgx.Tx, project, issue string, role claim.Role) error {
	_, err := tx.Exec(ctx, `delete from pod_wake_backstops
		where project = $1 and issue = $2 and not fired and ($3 = '' or role = $3)`,
		project, issue, string(role))
	if err != nil {
		return fmt.Errorf("cancel the backstops of %s: %w", issue, err)
	}
	return nil
}

// ForgetPodWakes drops every wake mark and every open backstop of issue, which is what the start
// of a release does — the marks of the release before it are spent — and what a path that takes
// a released claim over does, so no later wake acts on it.
func ForgetPodWakes(ctx context.Context, tx pgx.Tx, project, issue string) error {
	if _, err := tx.Exec(ctx, `delete from pod_wake_marks where project = $1 and issue = $2`, project, issue); err != nil {
		return fmt.Errorf("forget the wake marks of %s: %w", issue, err)
	}
	return CancelPodBackstops(ctx, tx, project, issue, "")
}

// ForgetPodWakesOf is ForgetPodWakes in a transaction of its own, for a caller outside one: the
// outbox's stops, which take a released claim over row by row.
func (s *Store) ForgetPodWakesOf(ctx context.Context, project, issue string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error { return ForgetPodWakes(ctx, tx, project, issue) })
}

// TakePodWake takes one wake outside any fact transaction, for the wakes the outbox takes: a
// notice or a phase-ended message that found the role it is for released. Inside a fact's
// transaction the workflow takes its four through PodRecords.TakePodWake, which is where taking
// a wake is written out.
//
// project is the project token the claim-keyed tables hold, as it is for every other
// project-keyed method of this file: the daemon derives it once at boot (claim.ProjectToken,
// internal/daemon/bootreads.go) and its outbox carries that, so nothing is derived again here.
func (s *Store) TakePodWake(ctx context.Context, project, issue string, role claim.Role, kind, key string, at time.Time) (bool, error) {
	records := &PodRecords{store: s, project: project}
	var taken bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		taken, err = records.TakePodWake(ctx, tx, issue, role, kind, key, at)
		return err
	})
	return taken, err
}

// ReleasedClaimsIn is every released claim of issue, in a transaction of its own: what the
// release's own arming of backstops reads, and what a caller outside a fact transaction asks.
func (s *Store) ReleasedClaimsIn(ctx context.Context, project, issue string) ([]supervise.Claim, error) {
	var claims []supervise.Claim
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		claims, err = ReleasedClaimsOf(ctx, tx, project, issue)
		return err
	})
	return claims, err
}

// PodWakeBackstop is one armed backstop: the phase worker it wakes and when it comes due.
type PodWakeBackstop struct {
	ID      int64
	Project string
	Issue   string
	Role    claim.Role
	Release int64
	DueAt   time.Time
}

// ArmPodBackstop arms the backstop of issue's role at due, over the release release names. A role
// that already has an open backstop keeps the one it has, so a release retried after a failure
// arms one backstop and not two.
func (s *Store) ArmPodBackstop(ctx context.Context, b PodWakeBackstop) error {
	if b.Project == "" || b.Issue == "" || b.Role == "" || b.DueAt.IsZero() {
		return errors.New("a pod wake backstop needs its project, issue, role and due time")
	}
	_, err := s.pool.Exec(ctx, `insert into pod_wake_backstops (project, issue, role, release_id, due_at)
		values ($1, $2, $3, $4, $5) on conflict do nothing`,
		b.Project, b.Issue, string(b.Role), b.Release, b.DueAt)
	if err != nil {
		return fmt.Errorf("arm the backstop of %s's %s: %w", b.Issue, b.Role, err)
	}
	return nil
}

// DuePodBackstops are project's open backstops due at or before now, oldest first: the ones this
// pass fires. One that came due while the daemon was down is in this read at the next pass after
// the daemon starts, which is what makes the backstop a stored row rather than a timer.
func (s *Store) DuePodBackstops(ctx context.Context, project string, now time.Time) ([]PodWakeBackstop, error) {
	rows, err := s.pool.Query(ctx, `select id, project, issue, role, release_id, due_at
		from pod_wake_backstops where project = $1 and not fired and due_at <= $2 order by due_at, id`,
		project, now)
	if err != nil {
		return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
	}
	defer rows.Close()
	var out []PodWakeBackstop
	for rows.Next() {
		var b PodWakeBackstop
		var role string
		if err := rows.Scan(&b.ID, &b.Project, &b.Issue, &role, &b.Release, &b.DueAt); err != nil {
			return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
		}
		b.Role = claim.Role(role)
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecordPodWakeTx is RecordPodWake inside a caller's transaction: a wake decided by an intake
// fact records itself with the outbox row that brings the pod back, so a committed wake is a
// recorded wake and a rolled-back one is neither. It writes the same pod_resumes row.
func RecordPodWakeTx(ctx context.Context, tx pgx.Tx, res PodResume) (int64, error) {
	if res.Project == "" || res.Issue == "" || res.Kind == "" {
		return 0, errors.New("a pod resume record needs its project, issue and wake kind")
	}
	var id int64
	err := tx.QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
		values ($1, $2, $3, $4, $5, $6) returning id`,
		res.Release, res.Project, res.Issue, res.Role, res.Kind, res.At).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record the wake of %s: %w", res.Issue, err)
	}
	return id, nil
}

// LatestPodRelease is the newest release recorded for issue, or 0 when none is: the release a
// wake of that pod ends, which is what pairs a resume with the release it measures.
func LatestPodRelease(ctx context.Context, tx pgx.Tx, project, issue string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `select id from pod_releases where project = $1 and issue = $2
		order by id desc limit 1`, project, issue).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the newest release of %s: %w", issue, err)
	}
	return id, nil
}

// OpenPodResume is the newest wake of issue's role with no first turn yet, and whether there is
// one: the resume a pod-ready or first-turn observation stamps. It is read rather than remembered
// so a daemon that restarted between the wake and the first turn still stamps it.
func (s *Store) OpenPodResume(ctx context.Context, project, issue string, role claim.Role) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `select id from pod_resumes
		where project = $1 and issue = $2 and role = $3 and first_turn_at is null
		order by id desc limit 1`, project, issue, string(role)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the open resume of %s's %s: %w", issue, role, err)
	}
	return id, true, nil
}

// ListenerReadyAt is when the Envoy listener last reported itself ready for project, and whether
// it ever has. A daemon that has read no report releases nothing.
func (s *Store) ListenerReadyAt(ctx context.Context, project string) (time.Time, bool, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `select ready_at from listener_reports where project = $1`, project).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the listener's report for %s: %w", project, err)
	}
	return at.UTC(), true, nil
}

// ListenerLive is R7's verdict for the idle rule: whether the Envoy listener has reported itself
// ready for project within window of now. It answers the report's time and whether there was one
// too, so a caller can say in its log which of the three it is — never reported, reported too
// long ago, or live.
//
// A daemon with no fresh report releases nothing. Without a positive signal "the listener sends
// no reports" and "the listener has nothing to report" are the same silence, and a pod released
// into that silence would not come back for a role message or an agent message, because only the
// listener sees either.
func (s *Store) ListenerLive(ctx context.Context, project string, now time.Time, window time.Duration) (bool, time.Time, bool, error) {
	at, found, err := s.ListenerReadyAt(ctx, project)
	if err != nil || !found {
		return false, at, found, err
	}
	return now.UTC().Sub(at) <= window, at, true, nil
}

// PodResumeReadyAt is when resume id's replacement pod was observed ready, zero while nothing
// has observed it. The stuck warning reads it to name the step the resume waits on: the pod, or
// the woken role's first turn.
func (s *Store) PodResumeReadyAt(ctx context.Context, id int64) (time.Time, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `select pod_ready_at from pod_resumes where id = $1`, id).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read the pod-ready stamp of resume %d: %w", id, err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return at.UTC(), nil
}

// PodRecords is the pod tables of one project as a caller outside the store reads and writes
// them: the listener's liveness report, the claims a release left released, the wake that brings
// one back and the backstop a wake fires. It derives the project token the claim-keyed tables
// hold (claim.ProjectToken) once, from the project key its caller already has, so no caller
// carries a second spelling of the project — which is what the workflow engine's own copy of it
// was for, and what filed the listener's report under a project the daemon never read.
//
// The workflow takes it as an interface (workflow.Pods), so nothing of the engine reaches
// Postgres directly.
type PodRecords struct {
	store   *Store
	project string
}

// PodRecordsOf is project's pod tables, refusing a project key that is no project
// (claim.ProjectToken).
func (s *Store) PodRecordsOf(project string) (*PodRecords, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, fmt.Errorf("read the pod records of %s: %w", project, err)
	}
	return &PodRecords{store: s, project: token}, nil
}

// RecordListenerReady stamps when the Envoy listener last said it is alive and reporting, which
// is the whole of R7's gate: the daemon releases no pod while that stamp is older than the
// freshness window.
func (p *PodRecords) RecordListenerReady(ctx context.Context, tx pgx.Tx, at time.Time) error {
	_, err := tx.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = greatest(listener_reports.ready_at, excluded.ready_at)`,
		p.project, at)
	if err != nil {
		return fmt.Errorf("record the listener's report for %s: %w", p.project, err)
	}
	return nil
}

// ReleasedClaimOfRole is the released claim of issue's role, and whether there is one.
func (p *PodRecords) ReleasedClaimOfRole(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (supervise.Claim, bool, error) {
	claims, err := ReleasedClaimsOf(ctx, tx, p.project, issue)
	if err != nil {
		return supervise.Claim{}, false, err
	}
	for _, c := range claims {
		if c.Role == role {
			return c, true, nil
		}
	}
	return supervise.Claim{}, false, nil
}

// ReleasedClaimByToken is the released claim whose token is token, and whether this daemon's
// project holds one. The listener reports a role token it saw a message published to and knows
// nothing of which daemon owns it, so every daemon reads its own claims and the one that holds a
// released claim of that token is the one that acts.
func (p *PodRecords) ReleasedClaimByToken(ctx context.Context, tx pgx.Tx, token string) (supervise.Claim, bool, error) {
	return oneReleasedClaim(ctx, tx, `and token = $4`, p.project, token, "role token")
}

// ReleasedClaimBySession is the released claim that kept session, and whether this daemon's
// project holds one. A released claim keeps its session id, which is what lets a message
// addressed to that session find the pod that must come back for it.
func (p *PodRecords) ReleasedClaimBySession(ctx context.Context, tx pgx.Tx, session string) (supervise.Claim, bool, error) {
	return oneReleasedClaim(ctx, tx, `and session = $4`, p.project, session, "session")
}

// TakePodWake takes one wake of issue's role inside the caller's transaction, and is the whole
// of what taking a wake is: the mark under the wake's own key, the resume recorded against the
// release that wake ends, and the role's own backstop cancelled. It answers whether this call
// took the wake; false is a wake another pass already took, and then the caller resumes nothing,
// because the first wake is already bringing the pod back.
//
// Every wake runs through here — the four the workflow applies inside a fact's transaction and
// the delivery wake the outbox takes in one of its own (Store.TakePodWake) — so there is one
// account of what a wake writes and one order it writes it in.
func (p *PodRecords) TakePodWake(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, kind, key string, at time.Time) (bool, error) {
	first, err := MarkPodWake(ctx, tx, p.project, issue, kind, key, at)
	if err != nil || !first {
		return false, err
	}
	release, err := LatestPodRelease(ctx, tx, p.project, issue)
	if err != nil {
		return false, err
	}
	if _, err := RecordPodWakeTx(ctx, tx, PodResume{Release: release, Project: p.project, Issue: issue,
		Role: string(role), Kind: kind, At: at}); err != nil {
		return false, err
	}
	return true, CancelPodBackstops(ctx, tx, p.project, issue, role)
}

// FirePodBackstop marks backstop id fired and answers whether this call was the one that fired
// it, so a backstop read by two passes wakes its pod once.
func (p *PodRecords) FirePodBackstop(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	tag, err := tx.Exec(ctx, `update pod_wake_backstops set fired = true where id = $1 and not fired`, id)
	if err != nil {
		return false, fmt.Errorf("fire backstop %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}
