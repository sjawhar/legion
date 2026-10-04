-- 0020_hold_reason_and_controller_notice.up.sql — why a held issue is held, and the controller
-- notice's own outbox kind.
--
-- The architect's escalation of a held issue is recorded as `escalated`, so the controller, which
-- learns of holds from the issue record at every start, can tell an escalation waiting on it from
-- a hold its architect is still deciding. A reason belongs to a hold: the trigger clears it on any
-- write that leaves the issue unheld, whichever build writes, so a hold's end ends its reason.
alter table issues add column hold_reason text;

create function issues_hold_reason_follows_hold() returns trigger language plpgsql as $$
begin
  if new.held_from is null then
    new.hold_reason := null;
  end if;
  return new;
end $$;

create trigger issues_hold_reason_follows_hold before insert or update on issues
  for each row execute function issues_hold_reason_follows_hold();

-- A notice for the project's controller topic alone (record.ControllerNotice) is an outbox row of
-- kind `controller_notice`, apart from the issue's `notice` row, so its publish retries alone.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in ('dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed', 'linger_close', 'workspace_remove'));
