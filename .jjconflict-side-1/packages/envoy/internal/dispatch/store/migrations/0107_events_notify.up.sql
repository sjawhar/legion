-- 0107: every committed event wakes each Dispatch task's delivery hub (LEGION-690), which then reads
-- the events after its cursor from this table. The in-process broker reaches only the task that
-- committed; during a rolling deploy the other task's hub learns of that task's issue edits only
-- from here. One notification per statement; Postgres folds repeats within a transaction.
create function events_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_events', '');
  return null;
end $$;
create trigger events_notify after insert on events for each statement execute function events_notify();
