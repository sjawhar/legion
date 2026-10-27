-- 0121_server_processes.up.sql
-- INFRA-555: the registry of live envoy-dispatch server processes (internal/dispatch/processes),
-- one row per process, written by the process itself once it has bound its port and before it
-- serves its first request, kept alive by a heartbeat every few seconds and deleted once its HTTP
-- drain has ended. A row whose heartbeat_at is within the live window of the database's now() is a
-- live process; no process's own clock decides that, since two hosts' clocks differ by whatever
-- theirs do.
--
-- signing_keys holds the fingerprints of the process's DISPATCH_SIGNING_KEY set
-- (auth.SigningKeys.Fingerprints), the current key first, never a key. A process refuses to start
-- beside a live process that does not share its current key both ways, which is how a process
-- that minted a key of its own, or a pod rolled with a changed secret, is stopped before it signs
-- anyone out. commit and dashboard_build name the build each process serves, '' when the binary
-- carries no stamp or the process serves no dashboard; the announcement of a fleet-wide build and
-- the dashboard's "new version" notice read them.
--
-- server_build_announcements is the record of each fleet-wide build the server fleet has announced
-- on the bus (notifications.dispatch.server.started): one row each time every live process serves
-- one commit that the latest row does not name, published_at set once the stream acknowledged its
-- envelope. seq is the announcement's own order and part of its envelope's id.
--
-- Two new tables: nothing existing is read, locked or rewritten here, and the release before this
-- one neither reads nor writes them, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations").
create table server_processes (
    instance uuid primary key,
    commit text not null default '',
    dashboard_build text not null default '',
    signing_keys text[] not null default '{}',
    started_at timestamptz not null default now(),
    heartbeat_at timestamptz not null default now()
);

create table server_build_announcements (
    seq bigint generated always as identity primary key,
    commit text not null,
    announced_at timestamptz not null default now(),
    published_at timestamptz
);
