-- 0016_ready_packet.up.sql — the summary each role reported with its completion, and the merge
-- queue publish.
--
-- The merger's summary is its READY packet: the daemon posts it on the Dispatch issue when the
-- issue reaches awaiting_merge, so the packet is kept with the completion across a design gate
-- that refused it until a human approves. When the project names a merge queue role, the daemon
-- also publishes the packet to that role (merge_queue_publish).
alter table phases add column summary text not null default '';

alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'supervise', 'gate_seed', 'linger_close',
  'workspace_remove', 'merge_queue_publish'));

-- A tree close and a workspace removal name the root generation of the linger they expire
-- ("linger"), and the outbox decodes rows strictly. A row queued in the older shape — a close
-- naming no linger, a removal naming its issue's generation — gets its tree root's generation when
-- that root still lingers, so it goes on to expire that linger; the rest are deleted, since their
-- linger has ended and a row that never decodes would fail on every attempt.
update outbox o set payload = o.payload || jsonb_build_object('linger', r.generation)
  from issues i join issues r on r.key = i.tree
  where o.kind = 'supervise' and o.payload->>'op' = 'tree_close' and not o.payload ? 'linger'
    and i.key = o.issue and r.linger_until is not null;
delete from outbox where kind = 'supervise' and payload->>'op' = 'tree_close' and not payload ? 'linger';

update outbox o set payload = (o.payload - 'generation') || jsonb_build_object('linger', r.generation)
  from issues i join issues r on r.key = i.tree
  where o.kind = 'workspace_remove' and o.payload ? 'generation'
    and i.key = o.issue and r.linger_until is not null;
delete from outbox where kind = 'workspace_remove' and payload ? 'generation';
