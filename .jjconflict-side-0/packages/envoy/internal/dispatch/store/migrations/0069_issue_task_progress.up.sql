-- 0069_issue_task_progress.up.sql
-- LEGION-542: an issue carries the count of its spec's task-list items.
--
-- `tasks_total` is every task item (`- [ ]` / `- [x]`, nested lists and callouts included) in the
-- issue's primary document as its latest version renders, `tasks_done` every checked one, and
-- `tasks_version` the number of the version the count was taken from. All three are written
-- together whenever a version of that document is written (docs.RecordTaskProgress), so the issue
-- read and list answer progress without parsing a document. A row whose `tasks_version` is not
-- the document's latest version number - every row this migration finds, and any row a server
-- predating the column versioned during a deploy - is counted again by the reconciliation the
-- server runs at start and on an interval (docs.Service.RunTaskProgressReconciliation), so a
-- stale count is bounded by that interval. The API answers `tasks: null` for a counted spec with
-- no task item (total 0) exactly as for one not yet counted. No existing row is rewritten here.
-- No index: the reconciliation's drift read runs over a table of a few hundred rows, and an index
-- build would lock the hot table.
alter table issues
  add column tasks_done int,
  add column tasks_total int,
  add column tasks_version int,
  add constraint issues_task_progress_complete check (
    (tasks_done is null) = (tasks_total is null)
    and (tasks_done is null) = (tasks_version is null)
    and (tasks_done is null or (tasks_done >= 0 and tasks_done <= tasks_total))
  );
