package store

import (
	"context"
	"fmt"
	"time"
)

// UnansweredPodWakes is every wake of a released pod of project taken at or before bound whose
// woken role has taken no turn, oldest first: what the stranded report reads to tell a release a
// wake is still bringing back from one nothing is (workflow.Stranded).
//
// It is StuckPodResumes without that read's `not stuck` filter. That filter is the daemon's
// "this resume has been warned about once" mark (MarkPodResumeStuck), so a report reading
// through it would list a stuck release until the first warning and never again; a wake that
// went unanswered stays unanswered here however many times the daemon has warned about it.
func (s *Store) UnansweredPodWakes(ctx context.Context, project string, bound time.Time) ([]PodResume, error) {
	rows, err := s.pool.Query(ctx, `select id, release_id, project, issue, role, kind, at, stuck
		from pod_resumes where project = $1 and first_turn_at is null and at <= $2 order by id`,
		project, bound)
	if err != nil {
		return nil, fmt.Errorf("read the unanswered wakes of %s: %w", project, err)
	}
	defer rows.Close()
	var out []PodResume
	for rows.Next() {
		var r PodResume
		if err := rows.Scan(&r.ID, &r.Release, &r.Project, &r.Issue, &r.Role, &r.Kind, &r.At, &r.Stuck); err != nil {
			return nil, fmt.Errorf("read the unanswered wakes of %s: %w", project, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
