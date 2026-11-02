-- 20261102003600_status_rows_carry_their_kind.up.sql — every queued Dispatch status write carries
-- the kind that decides the rule its retry is judged by (dispatch://INFRA-714, review round 6).
--
-- A status row's kind used to be read from three marks in its payload — personHandover, reassert
-- and handOver — and a row carrying none of them read as the ordinary kind. So a kind added
-- without a rule of its own compiled, read as ordinary, and was judged by the ordinary rule in
-- silence. The kind is a stored field now (record.StatusRowKind, one of `ordinary`,
-- `person-handover`, `reassert` and `hand-over`), the three marks are gone from the payload, and
-- the daemon refuses to queue a row whose kind is not one of the four.
--
-- A dispatch_status row the release before this one queued carries the old shape: no kind, which
-- the new validation refuses, and marks the payload no longer names, which its decode refuses
-- (DisallowUnknownFields). Either way the row could never run, and a status row that cannot run
-- holds every later write of its issue behind it. So each one is given its kind from the marks it
-- carries and loses all three in the same statement, leaving no stored row with the old shape and
-- none with an empty kind. The data step runs before any daemon of this release reads these rows,
-- for the reason 20261029120700's and 20261029120800's data steps run before their own CHECK
-- rewrites: the new code refuses the old value.
--
-- After this migration, a row the outbox fails for the kind it carries can only mean a kind
-- somebody added to record.StatusRowKinds without giving it a rule. The daemon migrates its
-- database at boot and does not start when that fails, so no process of this release reads an
-- unmigrated outbox; and no older daemon is left running to write the old shape again.
update outbox
set payload = (payload - 'personHandover' - 'reassert' - 'handOver') || jsonb_build_object('kind',
  case
    when payload->>'personHandover' = 'true' then 'person-handover'
    when payload->>'reassert' = 'true' then 'reassert'
    when payload->>'handOver' = 'true' then 'hand-over'
    else 'ordinary'
  end)
where kind = 'dispatch_status';
