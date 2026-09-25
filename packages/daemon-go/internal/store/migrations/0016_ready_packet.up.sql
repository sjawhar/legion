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
