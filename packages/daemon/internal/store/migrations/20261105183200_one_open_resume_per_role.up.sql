-- 20261105183200_one_open_resume_per_role.up.sql — one open resume per role is the whole of
-- "this pod is already coming back for it" (dispatch://INFRA-714/artifact/review-rounds-md v52).
--
-- Two dedupe layers stood where one rule serves. pod_wake_marks held a row per wake key, so a
-- wake reported twice woke a pod once; a guard over pod_resumes held a second wake of the same
-- role. Each wake's own event is already once-only upstream — the daemon's processed_events for
-- a fact, the conditional update that fires a backstop — so what is left to say is that a role
-- has at most one resume no first turn has stamped and nothing has closed. The index below says
-- it, and the insert that records a wake takes it with `on conflict do nothing` inside the fact's
-- own transaction, which is a lock rather than the read the guard was.
--
-- closed_at is how a resume ends without a turn. The release that stops a pod closes the resumes
-- of the pod it stops (CloseOpenResumes), and before this column there was nowhere to write that
-- but first_turn_at, which is the turn the daemon observed: the release records serve it as
-- firstTurnAt and the plugin measures each wake's wait by it, so closing a resume that way
-- reported a first turn that never happened.
alter table pod_resumes add column closed_at timestamptz;

-- A database upgrading can hold open resumes the new index refuses, of two kinds, and each is
-- closed here at the time of what ended it.
--
-- More than one open resume for a role, which the two layers allowed between them: the newest is
-- kept, since it is the wake the daemon is acting on, and every older one is closed at the time
-- of the newer, which is when that pod came back for the role.
update pod_resumes older set closed_at = newer.at
  from pod_resumes newer
  where older.first_turn_at is null and older.closed_at is null
  and newer.first_turn_at is null and newer.closed_at is null
  and older.project = newer.project and older.issue = newer.issue and older.role = newer.role
  and older.id < newer.id;

-- And a single open resume of a pod that has since been released again: no release closed a
-- resume before this migration, so a wake whose role's first turn the daemon never saw left one
-- behind, and that one row would refuse every later wake of the role for as long as the issue
-- lives. A resume older than the issue's newest release measures a pod that is gone, which is the
-- same reading UnfinishedPodResumes takes of it (release_records.go), so it is closed at the time
-- of the release that stopped its pod.
update pod_resumes r set closed_at = later.at
  from pod_releases later
  where r.first_turn_at is null and r.closed_at is null
  and later.project = r.project and later.issue = r.issue and later.id > r.release_id
  and later.id = (select max(id) from pod_releases
    where project = r.project and issue = r.issue);

create unique index pod_resumes_one_open_per_role on pod_resumes (project, issue, role)
  where first_turn_at is null and closed_at is null;

-- The marks have no reader left: the index above is the one rule, and a wake whose event was
-- already applied never reaches the take.
drop table pod_wake_marks;
