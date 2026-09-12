alter table asks drop constraint if exists asks_kind_check;
alter table asks add constraint asks_kind_check check (kind in ('question', 'approval', 'action'));
