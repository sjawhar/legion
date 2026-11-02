-- 20261102003600_status_rows_carry_their_kind.up.sql — every queued Dispatch status write carries
-- the kind that decides the rule its retry is judged by (dispatch://INFRA-714).
--
-- A status row's kind used to be read from three marks in its payload — personHandover, reassert
-- and handOver — and a row carrying none of them read as the ordinary kind. So a kind added
-- without a rule of its own compiled, read as ordinary, and was judged by the ordinary rule in
-- silence. The kind is a stored field now (record.StatusRowKind, one of `ordinary`,
-- `person-handover`, `reassert`, `hand-over` and `close`), the three marks are gone from the
-- payload, and the daemon refuses to queue a row whose kind is not one of the five.
--
-- A dispatch_status row the release before this one queued carries the old shape: no kind, which
-- the new validation refuses, and marks the payload no longer names, which its decode refuses
-- (DisallowUnknownFields). Either way the row could never run, and a status row that cannot run
-- holds every later write of its issue behind it. So each one is given its kind from the payload
-- it carries and loses all three marks in the same statement, leaving no stored row with the old
-- shape and none with an empty kind. The data step runs before any daemon of this release reads
-- these rows, for the reason 20261029120700's and 20261029120800's data steps run before their
-- own CHECK rewrites: the new code refuses the old value.
--
-- Two of the five kinds the release before this one wrote carry no mark at all, and reading them
-- as ordinary would leave each with the very rule it must not have:
--
--   * its re-assert of its own status over one a session wrote queued no `reassert` mark, and is
--     told by what it observes — triage, done, backlog or icebox, the statuses only a session's
--     own move out of the workflow puts an issue at, which the ordinary rule would read as an
--     issue that has left and drop the row that exists to undo that move;
--   * its own close, the done a merge or an architect's close_root writes, is told by the status
--     it writes, and the ordinary rule would drop it for the same reason, leaving the issue open
--     in Dispatch for good while the tree lingers.
--
-- A close given its kind here carries no `closeGeneration`, since the release that queued it
-- fenced the write on nothing: its rule reads a zero generation as that release's own unfenced
-- write.
--
-- The marked arms come first, and that order is what keeps a marked row from being read by a
-- statuses arm: a person's handover is queued observing triage, backlog or icebox, the very
-- statuses the re-assert arm names, so a `personHandover` row read by that arm would be judged
-- by the re-assert's rule and never dropped when the person took the label off again.
update outbox
set payload = (payload - 'personHandover' - 'reassert' - 'handOver') || jsonb_build_object('kind',
  case
    when payload->>'personHandover' = 'true' then 'person-handover'
    when payload->>'reassert' = 'true' then 'reassert'
    when payload->>'handOver' = 'true' then 'hand-over'
    when payload->>'observedStatus' in ('triage', 'done', 'backlog', 'icebox') then 'reassert'
    when payload->>'status' in ('done', 'backlog', 'icebox') then 'close'
    else 'ordinary'
  end)
where kind = 'dispatch_status';
