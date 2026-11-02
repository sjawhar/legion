package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PodRelease is one release of an issue pod: which roles were in it, when their claims were
// suspended as released, and how large the workspace bundle it saved was. It is the row every
// resume of that pod hangs from, so a reader can pair a release with what woke it and how long
// the agent waited.
type PodRelease struct {
	// ID is the row's own id, which the store fills.
	ID int64
	// Project and Issue name the pod.
	Project string
	Issue   string
	// Generation is the issue's generation at the release.
	Generation uint64
	// Roles are the roles whose claims the release suspended, sorted.
	Roles []string
	// At is when the release completed.
	At time.Time
	// BundleBytes is the size of the bundle the release saved; zero when it saved none because
	// the workspace held nothing unpushed.
	BundleBytes int
}

// PodResume is one wake of a released pod and what it cost: which wake brought it back, when the
// wake reached the daemon, when the pod was Ready, and when the woken role took its first turn.
// Stuck is a resume with no first turn ten minutes after its wake, which also writes a warning to
// the daemon's log.
type PodResume struct {
	ID int64
	// Release is the PodRelease this wake ended; zero when no release row is recorded for it.
	Release int64
	Project string
	Issue   string
	// Role is the role the wake was for.
	Role string
	// Kind names the wake: a daemon delivery, a CI settlement, a role message, an agent message
	// or the backstop.
	Kind string
	// At is when the wake reached the daemon or the Envoy listener.
	At time.Time
	// PodReadyAt and FirstTurnAt are zero until each is observed.
	PodReadyAt  time.Time
	FirstTurnAt time.Time
	// Stuck is whether the resume had no first turn ten minutes after its wake.
	Stuck bool
}

// PodHold is one pass of the idle rule that kept a pod past the idle period, with the reason it
// read. Holds are what tell a deployment whether release is working: a pod held for background
// work is working as designed, and one held by an old image or an unsent report is not.
type PodHold struct {
	ID      int64
	Project string
	Issue   string
	// Reason is the idle rule's own name for what held the pod (workflow.IdleHold's String).
	Reason string
	At     time.Time
}

// PodReleaseRecords is what the daemon serves a pod that reads the records of a window: every
// release, resume and hold recorded at or after a time, oldest first.
type PodReleaseRecords struct {
	Releases []PodRelease
	Resumes  []PodResume
	Holds    []PodHold
}

// RecordPodRelease writes one release and answers the row's id, which every resume of that pod
// names. A release records what it did even when nothing ever wakes the pod, so a window's count
// of releases is complete without reading any resume.
func (s *Store) RecordPodRelease(ctx context.Context, rel PodRelease) (int64, error) {
	if rel.Project == "" || rel.Issue == "" {
		return 0, errors.New("a pod release record needs its project and issue")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes)
		values ($1, $2, $3, $4, $5, $6) returning id`,
		rel.Project, rel.Issue, int64(rel.Generation), strings.Join(rel.Roles, ","), rel.At, rel.BundleBytes).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record the release of %s: %w", rel.Issue, err)
	}
	return id, nil
}

// RecordPodWake writes one wake of a released pod and answers the row's id, which the pod-ready
// and first-turn observations name. The row is written when the wake is taken, not when the pod
// comes back, so a resume that never reaches a first turn is in the records as a stuck one
// rather than missing.
func (s *Store) RecordPodWake(ctx context.Context, res PodResume) (int64, error) {
	if res.Project == "" || res.Issue == "" || res.Kind == "" {
		return 0, errors.New("a pod resume record needs its project, issue and wake kind")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
		values ($1, $2, $3, $4, $5, $6) returning id`,
		res.Release, res.Project, res.Issue, res.Role, res.Kind, res.At).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record the wake of %s: %w", res.Issue, err)
	}
	return id, nil
}

// MarkPodResumeReady stamps when the replacement pod was Ready. It is written once, by the first
// observation: a second observation of the same resume leaves the first time, which is the one
// R5 measures.
func (s *Store) MarkPodResumeReady(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update pod_resumes set pod_ready_at = $2 where id = $1 and pod_ready_at is null`, id, at)
	if err != nil {
		return fmt.Errorf("stamp pod resume %d ready: %w", id, err)
	}
	return nil
}

// MarkPodResumeFirstTurn stamps the woken role's first turn, which ends the resume R5 measures.
// It is written once, as the pod-ready stamp is.
func (s *Store) MarkPodResumeFirstTurn(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update pod_resumes set first_turn_at = $2 where id = $1 and first_turn_at is null`, id, at)
	if err != nil {
		return fmt.Errorf("stamp pod resume %d with its first turn: %w", id, err)
	}
	return nil
}

// MarkPodResumeStuck records a resume with no first turn ten minutes after its wake.
func (s *Store) MarkPodResumeStuck(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `update pod_resumes set stuck = true where id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark pod resume %d stuck: %w", id, err)
	}
	return nil
}

// StuckPodResumes are the resumes whose wake is at or before the bound with no first turn and no
// stuck mark yet: the ones the daemon warns about and counts, read once per pass so each warns
// once. The stranded report reads UnfinishedPodResumes instead, which keeps a marked row.
func (s *Store) StuckPodResumes(ctx context.Context, project string, before time.Time) ([]PodResume, error) {
	return s.podResumesWithoutAFirstTurn(ctx, project, before, true)
}

// UnfinishedPodResumes are every resume of project whose wake is at or before the bound and whose
// woken role has not taken its first turn, the ones a stuck pass has already marked included.
// The stranded report reads this one: a resume marked stuck is exactly a pod a wake never brought
// back, so dropping it would take the claim out of the report the moment the warning was written.
func (s *Store) UnfinishedPodResumes(ctx context.Context, project string, before time.Time) ([]PodResume, error) {
	return s.podResumesWithoutAFirstTurn(ctx, project, before, false)
}

func (s *Store) podResumesWithoutAFirstTurn(ctx context.Context, project string, before time.Time, unmarked bool) ([]PodResume, error) {
	query := `select id, release_id, project, issue, role, kind, at, stuck
		from pod_resumes where project = $1 and first_turn_at is null and at <= $2`
	if unmarked {
		query += ` and not stuck`
	}
	rows, err := s.pool.Query(ctx, query+` order by id`, project, before)
	if err != nil {
		return nil, fmt.Errorf("read the unfinished resumes of %s: %w", project, err)
	}
	defer rows.Close()
	var out []PodResume
	for rows.Next() {
		var r PodResume
		if err := rows.Scan(&r.ID, &r.Release, &r.Project, &r.Issue, &r.Role, &r.Kind, &r.At, &r.Stuck); err != nil {
			return nil, fmt.Errorf("read the unfinished resumes of %s: %w", project, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordPodHold writes one pass of the idle rule that kept a pod past the idle period.
func (s *Store) RecordPodHold(ctx context.Context, hold PodHold) error {
	if hold.Project == "" || hold.Issue == "" || hold.Reason == "" {
		return errors.New("a pod hold record needs its project, issue and reason")
	}
	_, err := s.pool.Exec(ctx, `insert into pod_holds (project, issue, reason, at) values ($1, $2, $3, $4)`,
		hold.Project, hold.Issue, hold.Reason, hold.At)
	if err != nil {
		return fmt.Errorf("record the hold of %s: %w", hold.Issue, err)
	}
	return nil
}

// PodReleaseRecords are every release, resume and hold of project recorded at or after since,
// oldest first, which is what a pod reads to measure a window.
func (s *Store) PodReleaseRecords(ctx context.Context, project string, since time.Time) (PodReleaseRecords, error) {
	var out PodReleaseRecords
	releases, err := s.pool.Query(ctx, `select id, project, issue, generation, roles, at, bundle_bytes
		from pod_releases where project = $1 and at >= $2 order by id`, project, since)
	if err != nil {
		return out, fmt.Errorf("read the release records of %s: %w", project, err)
	}
	for releases.Next() {
		var r PodRelease
		var generation int64
		var roles string
		if err := releases.Scan(&r.ID, &r.Project, &r.Issue, &generation, &roles, &r.At, &r.BundleBytes); err != nil {
			releases.Close()
			return out, fmt.Errorf("read the release records of %s: %w", project, err)
		}
		r.Generation = uint64(generation)
		if roles != "" {
			r.Roles = strings.Split(roles, ",")
		}
		out.Releases = append(out.Releases, r)
	}
	releases.Close()
	if err := releases.Err(); err != nil {
		return out, fmt.Errorf("read the release records of %s: %w", project, err)
	}

	resumes, err := s.pool.Query(ctx, `select id, release_id, project, issue, role, kind, at, pod_ready_at, first_turn_at, stuck
		from pod_resumes where project = $1 and at >= $2 order by id`, project, since)
	if err != nil {
		return out, fmt.Errorf("read the resume records of %s: %w", project, err)
	}
	for resumes.Next() {
		var r PodResume
		var ready, first *time.Time
		if err := resumes.Scan(&r.ID, &r.Release, &r.Project, &r.Issue, &r.Role, &r.Kind, &r.At, &ready, &first, &r.Stuck); err != nil {
			resumes.Close()
			return out, fmt.Errorf("read the resume records of %s: %w", project, err)
		}
		if ready != nil {
			r.PodReadyAt = *ready
		}
		if first != nil {
			r.FirstTurnAt = *first
		}
		out.Resumes = append(out.Resumes, r)
	}
	resumes.Close()
	if err := resumes.Err(); err != nil {
		return out, fmt.Errorf("read the resume records of %s: %w", project, err)
	}

	holds, err := s.pool.Query(ctx, `select id, project, issue, reason, at from pod_holds
		where project = $1 and at >= $2 order by id`, project, since)
	if err != nil {
		return out, fmt.Errorf("read the hold records of %s: %w", project, err)
	}
	defer holds.Close()
	for holds.Next() {
		var h PodHold
		if err := holds.Scan(&h.ID, &h.Project, &h.Issue, &h.Reason, &h.At); err != nil {
			return out, fmt.Errorf("read the hold records of %s: %w", project, err)
		}
		out.Holds = append(out.Holds, h)
	}
	return out, holds.Err()
}
