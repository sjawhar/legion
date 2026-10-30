-- 0072_merging_closes_the_issue.up.sql — the merger merges, so `merging` runs to the merge and the
-- merge closes the issue (dispatch://INFRA-714 R1, R2).
--
-- Phase `awaiting_merge` is gone. It existed for the stretch between the merger's READY and the
-- merge the daemon submitted for it; the daemon submits none, so there is nothing between the two:
-- an issue stands in `merging` until GitHub reports its pull request merged, and a merged root
-- stands there until the rest of its tree leaves the workflow (workflow's closeMerged). Every
-- issue the old daemon left in `awaiting_merge`, and every hold taken from it, moves to `merging`,
-- where the new daemon reads it: one whose pull request already merged is closed by the boot's
-- sweep (workflow's sweepMerged), and one still open is its merger's to merge. The moves run
-- before the constraints are swapped, since the new lists refuse the old value.
update issues set phase = 'merging' where phase = 'awaiting_merge';
update issues set held_from = 'merging' where held_from = 'awaiting_merge';

alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'awaiting_person', 'planning', 'implementing', 'testing', 'reviewing', 'accepting',
  'retro', 'merging', 'done', 'held'));
alter table issues drop constraint issues_held_from_check;
alter table issues add constraint issues_held_from_check check (held_from in (
  'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'accepting', 'retro', 'merging',
  'done'));

-- `ready_pending_version` and `phases.merge_note` are stopped, not dropped: the daemon neither
-- writes nor reads either, and the columns keep what they hold so a rollback to a daemon that
-- still reads them finds its data.
