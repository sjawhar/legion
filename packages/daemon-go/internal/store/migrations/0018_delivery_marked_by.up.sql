-- 0018_delivery_marked_by.up.sql - the prompt whose acknowledgement set a pending task's read mark
-- (delivered_at), kept with the task. A late refusal naming that prompt clears the mark; recorded
-- here rather than in the daemon's memory, a refusal replayed on a restarted daemon is still judged
-- against it. A mark set before this migration has no recorded prompt, so only a refusal naming the
-- pending delivery's own id clears it, as before.
alter table pending_task_deliveries add column marked_by text not null default '';
