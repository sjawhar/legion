-- 0024_issue_priority.up.sql
alter table issues add column priority smallint check (priority between 0 and 3);
