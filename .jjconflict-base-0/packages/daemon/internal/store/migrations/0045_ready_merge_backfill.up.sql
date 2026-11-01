-- 0045_ready_merge_backfill.up.sql — the merge submission of every READY already accepted before
-- the daemon submitted READY merges. The workflow queues a ready_merge row only as an issue reaches
-- awaiting_merge or its pull request reopens at READY's head, so an issue that reached
-- awaiting_merge before 0043 would wait for a merge nothing submits. This queues one, once, at the
-- upgrade, as the workflow's READY would have (workflow's queueReadyMerge): for each issue awaiting
-- its merge at its merger's READY head, on an open pull request, still in the workflow (its status
-- not triage, icebox, backlog or done: record.OutOfWorkflow), in a tree whose root does not
-- linger, and with no ready_merge row already, which is kept as it is. A READY a person merges
-- (its by-hand note) queues none. The row decides by GitHub and the record when it runs, as every
-- ready_merge row does. It runs here rather than at every boot: a row that ended on GitHub's
-- refusal or a stalled disagreement has told the architect once, and a boot that queued it again
-- would tell them again.
--
-- A daemon still on 0043 or 0044 may enqueue the same READY merge while this runs, and its row,
-- not committed yet, is one the check for an existing row cannot see. The insert then meets it at
-- 0044's unique index, waits for it, and keeps it (do nothing), so the upgrade never fails on it.
insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
select 'ready_merge', issue.key, jsonb_build_object('number', pr.number, 'head', merger.handoff_commit), 0, now(), '', ''
from issues issue
join issues root on root.key = issue.tree
join phases merger on merger.issue = issue.key and merger.role = 'merger'
join pull_requests pr on pr.issue = issue.key
where issue.phase = 'awaiting_merge'
  and issue.status not in ('triage', 'icebox', 'backlog', 'done')
  and root.linger_until is null
  and merger.merge_note = ''
  and merger.handoff_commit <> ''
  and pr.state = 'open'
  and not exists (select 1 from outbox existing where existing.kind = 'ready_merge' and existing.issue = issue.key)
on conflict (issue, (payload ->> 'head')) where kind = 'ready_merge' do nothing;
