---
title: "The pod serves its own test Postgres now, and /legion does not survive a pod replacement"
category: legion
tags:
  - issue-pod
  - postgres
  - harness
  - dispatch-e2e
date: 2026-11-01
status: active
module: packages/dispatch/e2e
supersedes: docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md
related_issues:
  - "sjawhar/legion#2549"
---

# The pod serves its own test Postgres now, and /legion does not survive a pod replacement

Two clauses of
docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md are
no longer true: that a role unpacks Postgres from Debian packages, and that `/legion` survives a
pod replacement. Its other rules stand, and the one that matters most held here - one idempotent
`setup.sh` plus a sourced `env.sh` is what made a lost harness cheap to rebuild.

- **Read `LEGION_TEST_PG_DSN` before you build any database harness.** Where it is set, the pod's
  roles container is running the server and `skills/legion-worker/references/tools.md` forbids
  starting one beside it: make your own database on it
  (`createdb -h 127.0.0.1 -p 5433 -U legion_test "e2e_$(date +%s)"`) and point the suite there.
  Every database made from that server's `template1` already has pgvector. A connection refused
  there is a fault of the pod, not yours to repair.
- **Where it is unset and `legion-testpg` is on `PATH`,** export `LEGION_TESTPG_DIR` under the
  issue's volume and run `legion-testpg ensure`, which makes the cluster, the role and `vector`.
  Hand-rolling a postmaster is the last resort, not the first step.
- **Where you must run your own,** the worker image now carries PostgreSQL 18 with pgvector
  (`/usr/lib/postgresql/18/bin`, `lib/vector.so`), so no Debian unpack is needed, and the pod's
  `/var/run/postgresql` is not writable: pass
  `-c unix_socket_directories=<a directory you own>` or the server dies at startup with
  `could not create lock file "/var/run/postgresql/.s.PGSQL.<port>.lock": Permission denied`.
  Guard its start by the port, as
  docs/solutions/legion/a-shared-pod-postgres-is-guarded-by-its-port-and-the-hour-long-browser-suite-takes-no-timeout-pr2549.md
  says.
- **Expect nothing of `/legion` across a pod replacement.** Build the harness there all the same -
  it keeps a gigabyte of scratch off the container filesystem a pod is evicted over - but treat
  every long run as losable, and push every commit before you start one.
- **A forward merge can bring in the facility that obsoletes your harness.** Read main's delta
  under `skills/` and `docs/` beside its code: this round's merge carried
  `skills/legion-worker/references/tools.md` and `docs/kubernetes.md` with the pod's test Postgres
  in them, and the round went on running its own server because nothing re-read those pages.

## Evidence

In sjawhar/legion#2549's last rounds the pod's own `LEGION_TEST_PG_DSN` was
`postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable` and `/usr/local/bin/legion-testpg`
was on `PATH`, both arriving with the forward merge of main at `3c78856b`; the round's harness
nonetheless ran a postmaster of its own on 55432, which the skill at that same head forbids.

`/legion/harness` was built on the issue volume and was gone after the pod replacement of
2026-10-31, taking a whole-project browser run at 799 of its 953 rows. Nothing of the branch was
unpushed, and `setup.sh` rebuilt the harness in under a minute; why the volume did not survive was
not established here.

The image's Postgres: `/usr/lib/postgresql/18/bin/postgres --version` prints
`postgres (PostgreSQL) 18.6 (Debian 18.6-1.pgdg13+2)` and `/usr/lib/postgresql/18/lib/vector.so`
exists, while `apt-get download postgresql-17` answers `E: Unable to locate package`.
