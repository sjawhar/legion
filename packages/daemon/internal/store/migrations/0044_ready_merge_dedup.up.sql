-- 0044_ready_merge_dedup.up.sql — an awaiting READY has one durable merge submission per issue and
-- head. Before adding its unique index, collapse rows 0043 could have left behind: prefer the
-- earliest unleased next_at and then the lowest id, so a duplicate mid-lease row loses to a row a
-- daemon can safely retry. If every duplicate is leased, retain the earliest next_at/id rather than
-- lose the only unfinished merge. Reopening at that head reuses the surviving row without pulling
-- its next_at before a GitHub rate-limit deadline; it never creates a duplicate mutation or
-- architect notice.
--
-- A daemon still on 0043 may work the outbox while this runs, and a row it commits after the
-- dedup's snapshot would fail the index. The lock holds every write off until the index exists,
-- which a held-off duplicate then meets. It is EXCLUSIVE so it also waits out a worker that has
-- locked rows to claim them (FOR UPDATE, then its lease write): a lock that let the dedup start
-- between the two would leave the lease write waiting on this migration while it waits on the
-- worker's row. Plain reads go on.
lock table outbox in exclusive mode;

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
