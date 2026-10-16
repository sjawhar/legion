-- 0044_ready_merge_dedup.up.sql — an awaiting READY has one durable merge submission per issue and
-- head. Reopening at that head expedites the existing row; it never creates a duplicate mutation or
-- architect notice.
create unique index outbox_ready_merge_issue_head_unique
  on outbox (issue, (payload ->> 'head'))
  where kind = 'ready_merge';
