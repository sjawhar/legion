package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The two durable facts a wake of a released issue pod rests on: the mark that says a wake has
// already been taken, and the backstop row that brings a pod back when nothing else does
// (migration 20261102114500_pod_wakes).

// PodWakeBackstop is one released pod's backstop: the release it belongs to and when it falls
// due. It is a stored row rather than a timer in memory, so a backstop that came due while the
// daemon was down fires after the daemon starts.
type PodWakeBackstop struct {
	Project string
	Issue   string
	// Release is the pod_releases row the backstop belongs to, 0 when none was recorded.
	Release int64
	DueAt   time.Time
}

// TakePodWake is how a wake happens at most once. It answers true to the first caller that names
// this (issue, kind, key) and false to every later one, so a head that settles again as the rest
// of its checks finish, and a listener that reports one undeliverable message on each of its
// delivery attempts, wake the pod once between them. The key's meaning is the kind's: a
// settlement's is the head it settled, a listener report's the id the listener gave it, the
// backstop's its release.
func (s *Store) TakePodWake(ctx context.Context, project, issue, kind, key string, at time.Time) (bool, error) {
	if project == "" || issue == "" || kind == "" || key == "" {
		return false, errors.New("a pod wake mark needs its project, issue, kind and key")
	}
	tag, err := s.pool.Exec(ctx, `insert into pod_wake_marks (project, issue, kind, wake_key, at)
		values ($1, $2, $3, $4, $5) on conflict do nothing`, project, issue, kind, key, at)
	if err != nil {
		return false, fmt.Errorf("take the %s wake of %s: %w", kind, issue, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ArmPodWakeBackstop records that issue's released pod comes back at due if nothing else wakes it
// first. One issue has one backstop — the newest release's — so a release made again after a wake
// cancelled the first replaces it rather than leaving two due times behind.
func (s *Store) ArmPodWakeBackstop(ctx context.Context, project, issue string, release int64, due time.Time) error {
	if project == "" || issue == "" {
		return errors.New("a pod wake backstop needs its project and issue")
	}
	_, err := s.pool.Exec(ctx, `insert into pod_wake_backstops (project, issue, release_id, due_at)
		values ($1, $2, $3, $4) on conflict (project, issue) do update
			set release_id = excluded.release_id, due_at = excluded.due_at`, project, issue, release, due)
	if err != nil {
		return fmt.Errorf("arm the backstop of %s: %w", issue, err)
	}
	return nil
}

// DuePodWakeBackstops are the backstops of project due at or before now, oldest first: the pods
// the daemon brings back on this pass. A backstop that fell due while the daemon was down is
// among them on the first pass after it starts, which is what makes the backstop survive a
// restart.
func (s *Store) DuePodWakeBackstops(ctx context.Context, project string, now time.Time) ([]PodWakeBackstop, error) {
	rows, err := s.pool.Query(ctx, `select project, issue, release_id, due_at from pod_wake_backstops
		where project = $1 and due_at <= $2 order by due_at, issue`, project, now)
	if err != nil {
		return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
	}
	defer rows.Close()
	var out []PodWakeBackstop
	for rows.Next() {
		var b PodWakeBackstop
		if err := rows.Scan(&b.Project, &b.Issue, &b.Release, &b.DueAt); err != nil {
			return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ClearPodWakeBackstop forgets issue's backstop: it fired, another wake brought the pod back
// first, or the release it belonged to was cancelled. A backstop already gone is success.
func (s *Store) ClearPodWakeBackstop(ctx context.Context, project, issue string) error {
	if _, err := s.pool.Exec(ctx, `delete from pod_wake_backstops where project = $1 and issue = $2`,
		project, issue); err != nil {
		return fmt.Errorf("clear the backstop of %s: %w", issue, err)
	}
	return nil
}

// OpenPodResume is the newest wake of issue's role whose woken role has taken no turn yet, and
// false when there is none: the resume the pod-ready and first-turn observations stamp
// (MarkPodResumeReady, MarkPodResumeFirstTurn). It is read from the records rather than held in
// memory so a daemon restarted between a wake and the role's first turn still measures it.
func (s *Store) OpenPodResume(ctx context.Context, project, issue, role string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `select id from pod_resumes
		where project = $1 and issue = $2 and role = $3 and first_turn_at is null
		order by id desc limit 1`, project, issue, role).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the open resume of %s's %s: %w", issue, role, err)
	}
	return id, true, nil
}

// LatestPodRelease is the id of the newest release recorded for issue, and false when none is:
// the row every wake of that pod hangs from (PodResume.Release). A pod released before the
// records existed, or one whose release record could not be written, is woken all the same with
// a resume that names no release.
func (s *Store) LatestPodRelease(ctx context.Context, project, issue string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `select id from pod_releases where project = $1 and issue = $2
		order by id desc limit 1`, project, issue).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the last release of %s: %w", issue, err)
	}
	return id, true, nil
}
