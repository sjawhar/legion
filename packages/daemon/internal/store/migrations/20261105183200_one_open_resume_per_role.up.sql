-- 20261105183200_one_open_resume_per_role.up.sql — one open resume per role is the whole of
-- "this pod is already coming back for it" (dispatch://INFRA-714/artifact/review-rounds-md v52).
--
-- Two dedupe layers stood where one rule serves. pod_wake_marks held a row per wake key, so a
-- wake reported twice woke a pod once; a guard over pod_resumes held a second wake of the same
-- role. Each wake's own event is already once-only upstream — the daemon's processed_events for
-- a fact, the conditional update that fires a backstop — so what is left to say is that a role
-- has at most one resume no first turn has stamped. The index below says it, and the insert that
-- records a wake takes it with `on conflict do nothing` inside the fact's own transaction, which
-- is a lock rather than the read the guard was.
--
-- A database upgrading can hold more than one open resume for a role, which the two layers
-- allowed between them: the newest is kept, since it is the wake the daemon is acting on, and
-- every older one is closed at the time of the newer, which is when that pod came back for the
-- role. first_turn_at is the column the stuck-resume report reads (OpenPodResume), so a row
-- closed here reports as a resume whose first turn the daemon did not see rather than as one
-- still open.
update pod_resumes older set first_turn_at = newer.at
  from pod_resumes newer
  where older.first_turn_at is null and newer.first_turn_at is null
  and older.project = newer.project and older.issue = newer.issue and older.role = newer.role
  and older.id < newer.id;

create unique index pod_resumes_one_open_per_role on pod_resumes (project, issue, role)
  where first_turn_at is null;

-- The marks have no reader left: the index above is the one rule, and a wake whose event was
-- already applied never reaches the take.
drop table pod_wake_marks;
