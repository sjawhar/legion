-- 0039_disarm_and_by_hand_note.up.sql — two parts of keeping a merge to the READY that covers it.
--
-- The outbox kind that takes a pull request out of the merge queue and disables its auto-merge
-- when no READY covers its head (auto_merge_disable), beside every kind 0034 admits. The check is
-- rewritten whole, naming every kind: this sets the list after every migration that adds a kind.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable'));

-- The by-hand note a READY was accepted with (record.PhaseRow.MergeNote), on the merger's row: the
-- packet the daemon tells starts with it, and a pull request it names is never kept armed. '' on
-- every existing row, a READY nobody marked by hand.
alter table phases add column merge_note text not null default '';
