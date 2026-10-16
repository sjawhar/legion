-- 0044_ready_merge_dedup.up.sql — an awaiting READY has one durable merge submission per issue and
-- head. Before adding its unique index, collapse rows 0043 could have left behind: prefer the
-- earliest unleased next_at and then the lowest id, so a duplicate mid-lease row loses to a row a
-- daemon can safely retry. If every duplicate is leased, retain the earliest next_at/id rather than
-- lose the only unfinished merge. Reopening at that head reuses the surviving row without pulling
-- its next_at before a GitHub rate-limit deadline; it never creates a duplicate mutation or
-- architect notice.
with ranked as (
  select id, row_number() over (
    partition by issue, payload ->> 'head'
    order by case when lease_until is null or lease_until <= now() then 0 else 1 end, next_at, id
  ) as position
  from outbox
  where kind = 'ready_merge'
)
delete from outbox
using ranked
where outbox.id = ranked.id and ranked.position > 1;

create unique index outbox_ready_merge_issue_head_unique
  on outbox (issue, (payload ->> 'head'))
  where kind = 'ready_merge';
