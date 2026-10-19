-- 0047_accepting_and_production_checks.up.sql — the architect accepts before the merge, the merge
-- closes the issue, and the production check becomes work after the close (LEGION-781).
--
-- Phases. `accepting` sits between `reviewing` and `retro`; `production_check` is no phase. An
-- issue the old daemon left in production_check has its pull request merged (that phase began at
-- the merge), so it stands where the new daemon closes it: awaiting_merge, whose merged pull
-- request the boot's sweep closes. The same for a hold taken from production_check. The move runs
-- before the constraints are swapped, since the new lists refuse the old value.
update issues set phase = 'awaiting_merge' where phase = 'production_check';
update issues set held_from = 'awaiting_merge' where held_from = 'production_check';

alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'accepting', 'retro', 'merging',
  'awaiting_merge', 'done', 'held'));
alter table issues drop constraint issues_held_from_check;
alter table issues add constraint issues_held_from_check check (held_from in (
  'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'accepting', 'retro', 'merging',
  'awaiting_merge', 'done'));
