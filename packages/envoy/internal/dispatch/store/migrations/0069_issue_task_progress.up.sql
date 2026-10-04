-- 0069_issue_task_progress.up.sql
-- LEGION-542: an issue carries the count of its spec's task-list items.
--
-- `tasks_total` is every task item (`- [ ]` / `- [x]`, nested lists and callouts included) in the
-- issue's primary document as its latest version renders, and `tasks_done` every checked one. Both
-- are written together whenever a version of that document is written (docs.RecordTaskProgress),
-- so the issue read and list answer progress without parsing a document. Both null is an issue
-- whose spec has not been counted yet: the server counts those once after it starts
-- (docs.Service.RunTaskProgressBackfill), and the API answers `tasks: null` for a counted spec
-- with no task item (total 0) exactly as for one not yet counted. No existing row is rewritten
-- here; the backfill fills them after the deploy. No index: the backfill's `tasks_total is null`
-- read runs once over a table of a few hundred rows, and an index build would lock the hot table.
alter table issues
  add column tasks_done int,
  add column tasks_total int,
  add constraint issues_task_progress_complete check (
    (tasks_done is null) = (tasks_total is null)
    and (tasks_done is null or (tasks_done >= 0 and tasks_done <= tasks_total))
  );
