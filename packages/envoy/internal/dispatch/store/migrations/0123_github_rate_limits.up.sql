-- 0123_github_rate_limits.up.sql
-- INFRA-562: the GitHub rate limits every Dispatch process obeys together. GitHub budgets the App
-- (its JWT calls) and each installation (its token's calls), not a process, and warns that requests
-- sent while limited may get the integration banned; while each process kept what GitHub answered
-- to itself, a limit one process met left the other asking GitHub until it met the limit too. Now
-- githubapp.Client reads the row of a request's scope before it sends anything and refuses the
-- request while a limit is in force, and records GitHub's own wait here when an answer is a rate
-- limit (githubapp/ratelimit).
--
-- One row per scope:
--   scope       'app' for the calls the App JWT signs (installation lookups, token mints, the App
--               webhook's delivery endpoints), 'installation:<id>' for the calls an installation's
--               token makes, and 'redelivery' for the webhook redelivery sweep's own backoff
--               (redeliver/sweep.go), which the sweep checks together with 'app', so a limit met by
--               redelivery requests doubles that sweep's wait and never the App's.
--   until       when a request under the scope may be sent again, judged against the database's
--               now(), as embeddings_rate_limit (0078) is: the one clock every process reads, so
--               they agree whatever their hosts' clocks say. A recorded limit never shortens one in
--               force (greatest of the two).
--   strikes     the limits met under the scope with no clear pass in between; the redelivery sweep
--               doubles its wait for each, up to 64 minutes, and deletes its row once a sweep
--               finishes clear. The App's and installations' scopes take GitHub's wait as given.
--   updated_at  the database's now() at the last write, so a sweep deletes only the row it read and
--               never a limit another process recorded after it.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"), each of its processes obeying GitHub's answers on its own as before.
create table github_rate_limits (
    scope text primary key,
    until timestamptz not null,
    strikes integer not null default 0,
    updated_at timestamptz not null default now()
);
