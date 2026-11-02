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

// Wake kinds, the vocabulary of pod_resumes.kind and of a wake mark's kind: the five ways a
// released pod comes back. WakeDelivery is a delivery the daemon already had for a role of the
// pod — a phase start, a task, a backward move, a notice, a phase-ended message, a production
// check's look, a slot resume; WakeChecks is a CI settlement on the head of the issue's pull
// request; WakeRoleMessage and WakeAgentMessage are what only the Envoy listener sees, a message
// published to a role topic of the pod and one sent to the agent subject of a released session;
// WakeBackstop is the stored backstop a release armed.
const (
	WakeDelivery     = "delivery"
	WakeChecks       = "checks"
	WakeRoleMessage  = "role_message"
	WakeAgentMessage = "agent_message"
	WakeBackstop     = "backstop"
)

// ReleasedClaim is one claim of a released pod as a wake reads it: a claim this daemon's project
// holds, suspended with the source `released`, which is the one state a wake acts on. Every other
// suspended claim is somebody else's to resume — an operator's, a close's — and a path that takes
// a released claim over (an operator's suspend, a linger) re-stamps its source, which takes it out
// of every read here.
type ReleasedClaim struct {
	Token   claim.Token
	Project string
	Tree    string
	Issue   string
	Role    claim.Role
	// Session is the agent session the claim kept through the release, the one a message
	// addressed to that session names.
	Session string
	// ServingGeneration is the run the claim was serving when it was released.
	ServingGeneration uint64
}

// releasedClaimSelect reads the columns a wake needs of a released claim. It is deliberately not
// claimSelect: a wake reads no delivery and no launch state, only who the claim is and what it
// was serving.
const releasedClaimSelect = `select token, project, tree, issue, role, session, serving_generation
	from claims where project = $1 and state = $2 and suspended_by = $3`

func scanReleasedClaims(rows pgx.Rows) ([]ReleasedClaim, error) {
	defer rows.Close()
	var out []ReleasedClaim
	for rows.Next() {
		var c ReleasedClaim
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
func ReleasedClaimsOf(ctx context.Context, tx pgx.Tx, project, issue string) ([]ReleasedClaim, error) {
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

// ReleasedClaimOfRole is the released claim of issue's role, and whether there is one.
func ReleasedClaimOfRole(ctx context.Context, tx pgx.Tx, project, issue string, role claim.Role) (ReleasedClaim, bool, error) {
	claims, err := ReleasedClaimsOf(ctx, tx, project, issue)
	if err != nil {
		return ReleasedClaim{}, false, err
	}
	for _, c := range claims {
		if c.Role == role {
			return c, true, nil
		}
	}
	return ReleasedClaim{}, false, nil
}

// ReleasedClaimByToken is the released claim whose token is token, and whether this daemon's
// project holds one. The listener reports a role token it saw a message published to and knows
// nothing of which daemon owns it, so every daemon reads its own claims and the one that holds a
// released claim of that token is the one that acts.
func ReleasedClaimByToken(ctx context.Context, tx pgx.Tx, project, token string) (ReleasedClaim, bool, error) {
	return oneReleasedClaim(ctx, tx, `and token = $4`, project, token, "role token")
}

// ReleasedClaimBySession is the released claim that kept session, and whether this daemon's
// project holds one. A released claim keeps its session id, which is what lets a message
// addressed to that session find the pod that must come back for it.
func ReleasedClaimBySession(ctx context.Context, tx pgx.Tx, project, session string) (ReleasedClaim, bool, error) {
	return oneReleasedClaim(ctx, tx, `and session = $4`, project, session, "session")
}

func oneReleasedClaim(ctx context.Context, tx pgx.Tx, where, project, value, what string) (ReleasedClaim, bool, error) {
	if value == "" {
		return ReleasedClaim{}, false, nil
	}
	rows, err := tx.Query(ctx, releasedClaimSelect+" "+where+` order by token limit 1`,
		project, string(supervise.StateSuspended), string(supervise.SuspendedByReleased), value)
	if err != nil {
		return ReleasedClaim{}, false, fmt.Errorf("read the released claim of %s %s: %w", what, value, err)
	}
	claims, err := scanReleasedClaims(rows)
	if err != nil {
		return ReleasedClaim{}, false, fmt.Errorf("read the released claim of %s %s: %w", what, value, err)
	}
	if len(claims) == 0 {
		return ReleasedClaim{}, false, nil
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

// CancelPodBackstops drops every open backstop of issue: a wake that is bringing the pod back
// leaves nothing for one to do, and a path that takes a released claim over leaves nothing for
// one to act on.
func CancelPodBackstops(ctx context.Context, tx pgx.Tx, project, issue string) error {
	if _, err := tx.Exec(ctx, `delete from pod_wake_backstops where project = $1 and issue = $2 and not fired`,
		project, issue); err != nil {
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
	return CancelPodBackstops(ctx, tx, project, issue)
}

// ForgetPodWakesOf is ForgetPodWakes in a transaction of its own, for a caller outside one: the
// outbox's stops, which take a released claim over row by row.
func (s *Store) ForgetPodWakesOf(ctx context.Context, project, issue string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error { return ForgetPodWakes(ctx, tx, project, issue) })
}

// TakePodWake takes one wake outside any fact transaction, for the wakes the outbox takes: a
// notice or a phase-ended message that found the role it is for released. It marks the wake
// under its key and, when the mark is the first, records it against the newest release of the
// pod and answers that resume row's id. taken is false for a wake another pass already took,
// and then the caller resumes nothing: the first wake is already bringing the pod back.
func (s *Store) TakePodWake(ctx context.Context, project, issue, role, kind, key string, at time.Time) (int64, bool, error) {
	var id int64
	var taken bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		first, err := MarkPodWake(ctx, tx, project, issue, kind, key, at)
		if err != nil || !first {
			return err
		}
		release, err := LatestPodRelease(ctx, tx, project, issue)
		if err != nil {
			return err
		}
		id, err = RecordPodWakeTx(ctx, tx, PodResume{Release: release, Project: project, Issue: issue,
			Role: role, Kind: kind, At: at})
		if err != nil {
			return err
		}
		taken = true
		return CancelPodBackstops(ctx, tx, project, issue)
	})
	return id, taken, err
}

// CancelPodBackstopsOf is CancelPodBackstops in a transaction of its own, for the same callers.
func (s *Store) CancelPodBackstopsOf(ctx context.Context, project, issue string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error { return CancelPodBackstops(ctx, tx, project, issue) })
}

// ReleasedClaimsIn is every released claim of issue, in a transaction of its own: what the
// release's own arming of backstops reads, and what a caller outside a fact transaction asks.
func (s *Store) ReleasedClaimsIn(ctx context.Context, project, issue string) ([]ReleasedClaim, error) {
	var claims []ReleasedClaim
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		claims, err = ReleasedClaimsOf(ctx, tx, project, issue)
		return err
	})
	return claims, err
}

// PodWakeBackstop is one armed backstop: the phase worker it wakes and when it comes due.
type PodWakeBackstop struct {
	ID         int64
	Project    string
	Issue      string
	Role       claim.Role
	Release    int64
	DueAt      time.Time
	Generation uint64
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

// FirePodBackstop marks backstop id fired and answers whether this call was the one that fired
// it, so a backstop read by two passes wakes its pod once.
func FirePodBackstop(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	tag, err := tx.Exec(ctx, `update pod_wake_backstops set fired = true where id = $1 and not fired`, id)
	if err != nil {
		return false, fmt.Errorf("fire backstop %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
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

// RecordListenerReady stamps when the Envoy listener last said it is alive and reporting, which
// is the whole of R7's gate: the daemon releases no pod while that stamp is older than the
// freshness window.
func RecordListenerReady(ctx context.Context, tx pgx.Tx, project string, at time.Time) error {
	if project == "" {
		return errors.New("a listener-ready report needs its project")
	}
	_, err := tx.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = greatest(listener_reports.ready_at, excluded.ready_at)`,
		project, at)
	if err != nil {
		return fmt.Errorf("record the listener's report for %s: %w", project, err)
	}
	return nil
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
