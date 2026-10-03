-- 0025_issue_dispatch_status.up.sql — the status Dispatch last showed an issue.
--
-- A Dispatch issue event carries the whole issue, so it writes a status only when its status differs
-- from the one Dispatch showed at the event before it (record.Issue.DispatchStatus). The status column
-- cannot answer that: the daemon moves it when it queues a status write, before Dispatch shows it. A
-- row written before this column shows the status its oldest unfinished status write was queued
-- over, which Dispatch still shows until that write lands, and otherwise its own status.
alter table issues add column dispatch_status text not null default '';
update issues i set dispatch_status = coalesce(
  (select o.payload->>'observedStatus' from outbox o
    where o.kind = 'dispatch_status' and o.issue = i.key order by o.id limit 1),
  i.status);
