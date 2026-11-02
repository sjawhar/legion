---
title: "A shared pod Postgres is guarded by its port, and the hour-long browser suite takes no timeout"
category: legion
tags:
  - legion-worker
  - issue-pod
  - dispatch-e2e
  - postgres
  - playwright
date: 2026-10-31
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2549"
---

# A shared pod Postgres is guarded by its port, and the hour-long browser suite takes no timeout

Extends docs/solutions/legion/a-port-held-in-an-issue-pod-is-a-sibling-roles-harness-move-your-run-with-the-five-port-variables-and-your-own-database-and-never-stop-the-holder-LEGION-695.md.

Two rules for the one Postgres the roles of an issue pod share, each of which cost a run here.

- **Guard the start by the port, never by a pid file or a process list.** `pg_isready -h 127.0.0.1 -p <port>`
  is the only check that answers for another role's server. `pg_ctl status` reads
  `$PGDATA/postmaster.pid`, which a pod move leaves behind while a server from before the move still
  serves, so it reports "no server running" and the setup starts a second postmaster. `pgrep` cannot
  see the holder at all: each role of the pod has its own PID namespace. The second postmaster fails
  to bind, and the running one then finds its own lock file invalid and shuts itself down, taking the
  databases of every role with it.
- **Put no `timeout` around the whole browser project.** In a Legion issue pod
  `bun run e2e -- --project=chromium` takes about an hour for the Chromium project. A `timeout 2400`
  kills it at roughly 620 of its rows, and the wrapper reports exit 124 with the suite's own output
  ending `exited with code 143` - which reads like a failing suite and is not one. Run it as a
  background job and read its log, which is also how its rows are quoted.

When your own start does bounce the shared server, say so at once on the role topic of every role in
the pod, with the minute it went down and came back, so a sibling can tell its own red rows from
yours.

## Evidence

The guard: this pull request's harness `setup.sh` trusted `pg_ctl status`. After a pod move at 07:55Z
the stale `postmaster.pid` made it start a second server; `/legion/harness/postgres.log` holds
`could not bind IPv4 address "127.0.0.1": Address already in use`, then, from the running server,
`database system is shut down` and `could not open file "postmaster.pid": No such file or directory`.
The tester's harness and the implementer's databases went with it. The replacement guard is
`pg_isready`, with the two rejected checks and the reason named above it in the script.

The timeout: the whole-project run of round 10 was wrapped in `timeout 2400`. It was killed at about
620 green rows with `error: script "e2e" exited with code 143` and `DONE exit=124`; the uncapped
re-run of the same head finished 936 passed, 25 skipped, 0 failed in 1.0 h.
