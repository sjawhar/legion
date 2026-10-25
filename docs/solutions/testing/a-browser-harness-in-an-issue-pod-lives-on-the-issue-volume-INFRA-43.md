---
title: "A browser harness in an issue pod lives on the issue volume: Postgres with pgvector from Debian packages, and every cache moved off the container filesystem"
category: testing
tags:
  - issue-pod
  - playwright
  - postgres
  - pgvector
  - ephemeral-storage
  - harness
date: 2026-10-20
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-43"
  - "sjawhar/legion#2084"
---

# A browser harness in an issue pod lives on the issue volume: Postgres with pgvector from Debian packages, and every cache moved off the container filesystem

Extends docs/solutions/testing/an-embedded-postgres-in-a-worker-pod-the-zonky-jar-unpacked-with-python-gives-the-daemons-database-suites-a-run-LEGION-631.md.

That document's zonky-jar Postgres gives the daemon's Go suites a database and says it cannot give
pgvector, and that `/tmp` does not survive a relaunch. Dispatch's browser suite needs both: its
migrations create the `vector` extension, and a Playwright run needs a browser, two builds and a
Go binary. The delta is where the harness lives and how it is built.

- **Build it on the issue volume, never under `/tmp` or `~/.cache`.** `/legion` is the issue's own
  volume: it survives a pod replacement, which the container filesystem does not, and it keeps a
  gigabyte of scratch off the filesystem a pod is evicted over
  (see docs/solutions/legion/a-roles-scratch-on-the-container-filesystem-is-what-an-issue-pod-is-replaced-over-INFRA-43.md).
  Put nothing under `$LEGION_WORKSPACE`: six roles share that jj workspace and an untracked tree
  there is everyone's problem.
- **Postgres with pgvector comes from Debian packages, unpacked without root.** A pod has no
  Docker, no root and no Postgres. `apt-get download` into a private state/cache directory, then
  `dpkg-deb -x` each `.deb` into one root, gives `initdb`/`pg_ctl`/`psql` and
  `postgresql-17-pgvector`; run them with that root's `bin` on `PATH` and its
  `usr/lib/x86_64-linux-gnu` on `LD_LIBRARY_PATH`, and `create extension vector` then succeeds.
  The harness shells out to `psql`, so that `PATH` must be exported for the Playwright run too,
  or every row fails `spawn psql ENOENT`.
  **Note 2026-10-25 (INFRA-221):** still the step, judged by both conditions of
  `skills/legion-worker/references/tools.md`, "The test: where it lands, and what it leaves
  resolving": the root lands under `/legion`, the issue's volume, and a role reaches it only by
  sourcing the env file, so nothing resolves through it that did not name it. Keep it that way —
  the env file and the harness's own commands, not an export left in the shell you go on working
  in. `psql` is in the image now, so the `spawn psql ENOENT` above no longer depends on that
  export at all.
- **Move the caches with it:** `GOCACHE`, `GOMODCACHE` and `PLAYWRIGHT_BROWSERS_PATH`. They are
  the bulk of the scratch, and a cold Go cache also overruns Playwright's 60 s `webServer` window
  when `run-server.sh` rebuilds the Dispatch binary inside it — pre-build the binary once with the
  cache on the volume and the server starts in seconds.
- **Write it as one idempotent script plus one sourced env file**, and have every role source the
  env file rather than re-deriving it: a later role, or the same role after a replacement, re-runs
  the script and it rebuilds only what is missing. The tester and reviewer then inherit the
  harness instead of making their own.
- **Share the harness, not the lane.** Each role keeps its own ports and its own database and
  stops nothing a sibling is using
  (docs/solutions/legion/a-port-held-in-an-issue-pod-is-a-sibling-roles-harness-move-your-run-with-the-five-port-variables-and-your-own-database-and-never-stop-the-holder-LEGION-695.md).
  Branch the lane in the shared env file by `LEGION_ROLE` so a role that joins later gets a free
  lane without editing anyone else's.
- **Rebuild the SPA after every `web/src` change**: the browser rows serve
  `packages/dispatch/web/dist`, so an unrebuilt dist silently tests the previous head — including
  during a mutation check, where it would report the wrong answer.

## Evidence

INFRA-43's rows are a real browser against the real Go Dispatch server and a real Postgres. Built
under `/tmp` first, the harness was lost twice in one morning when the pod was replaced; rebuilt
under `/legion/harness` (`setup.sh`, `env.sh`) it survived every later replacement and was reused
unchanged by the tester, who re-ran `setup.sh` and found nothing to do. The implementer's second
round ran on its own lane (port 18787, database `dispatch_impl_e2e`) because a sibling role held
the default one; nothing was stopped and both runs stood.
