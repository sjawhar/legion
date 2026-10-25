-- 0051_dispatch_issues.up.sql -- the daemon's mirror of what Dispatch says about every issue of
-- its project: the status it stands at, the keys it waits on, and the Dispatch sequence the row
-- was observed at (INFRA-225).
--
-- Admission decides whether to admit a root, and the workflow whether to start a child's first
-- phase, inside one transaction that does no I/O, so neither can ask Dispatch what an issue is
-- blocked by at the moment it decides. This table is what they read instead: every issue of the
-- project, recorded or not, since a child can name a blocker the daemon has never admitted.
--
-- Rows are never removed. Dispatch has no issue delete, and never moves an issue between
-- projects, so no key ever stops existing and none ever changes its project: nothing in the
-- daemon deletes or re-projects a row, and the mirror is write-only apart from the sequence-fenced
-- update each fresher observation makes.
create table dispatch_issues (
    key text primary key,
    project text not null,
    status text not null,
    blockers text[] not null default '{}',
    dispatch_seq bigint not null
);

-- Both consumers read one project's whole mirror in a single query and judge in memory.
create index dispatch_issues_project on dispatch_issues (project);
