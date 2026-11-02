-- 20261102024952_report_settings.up.sql
-- Where a person's report of a problem with Dispatch itself goes (INFRA-971): the project its
-- issue is filed in, and the agent that fixes one when the reporter asks. Like delivery_settings
-- (0079) it belongs to the deployment rather than to any project, so it is one row, the singleton
-- key and its check holding it to one.
--
--   project_key  the project a report's issue is filed in; null while reports are not set up, and
--                the report control then says so. The write checks the project exists, and a
--                project is never deleted or renamed, so no foreign key holds it: one would take
--                a SHARE ROW EXCLUSIVE lock on projects while this migration runs, which every
--                issue create's update of its project row waits behind.
--   agent_route  the issue route (issues.route) a report is given when its reporter asks an agent
--                to fix it: always a role route, `role:<name>`, since a role outlives the session
--                that holds it and a session id does not survive the agent's restart
--                (api/settings_reports.go checks it). Null hides the report form's box.
--   updated_by   who last wrote the row, as an actor.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table report_settings (
    singleton boolean primary key default true check (singleton),
    project_key text,
    agent_route text,
    updated_by jsonb not null,
    updated_at timestamptz not null default now()
);
