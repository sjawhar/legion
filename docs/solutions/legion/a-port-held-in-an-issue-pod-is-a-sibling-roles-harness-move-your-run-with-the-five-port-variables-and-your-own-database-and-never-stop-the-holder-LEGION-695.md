---
title: "A port held in an issue pod is a sibling role's harness: move your run with the five port variables and your own database, and never stop the holder"
category: legion
tags:
  - legion-worker
  - issue-pod
  - dispatch-e2e
  - harness-ports
  - postgres
  - playwright
date: 2026-10-12
status: active
module: packages/dispatch/e2e
related_issues:
  - "LEGION-695"
  - "sjawhar/legion#1928"
---

# A port held in an issue pod is a sibling role's harness: move your run with the five port variables and your own database, and never stop the holder

Extends `packages/dispatch/AGENTS.md` ("`e2e/harness-ports.ts` resolves …", the port-probe
paragraphs), whose remedy for a taken harness port is "stop whatever listens there, or move a
local run to free ports and its own `DATABASE_URL`". In a Legion issue pod only the second half
applies:

- Every role of an issue (planner, implementer, tester, reviewer, merger, architect) runs in its
  own container of one pod, and the containers share one network namespace. A `bun run
  dispatch:local` or a Playwright run the tester left listening on `8777` and `9021-9024` is
  invisible to your `ps` and is not yours to stop: it is that role's proof surface, possibly
  mid-run.
- Read the Playwright config's port refusal, or `local-instance.ts`'s `already in use`, as that
  sibling, and run yours with `DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`, `FAKE_GITHUB_PORT`,
  `PLAIN_HTTP_PORT` and `FAKE_BROKER_PORT` on free ports **and** a `DATABASE_URL` of your own
  (`DISPATCH_E2E_REUSE_SERVERS=1` against the sibling's server truncates your database under a
  server that holds another and fails every row with `409 PROJECT_EXISTS`). Name the ports and
  the database in your proof so the next role can tell the two instances apart.
- The pod has no Docker and no Postgres; a role that needs the harness builds one under `/tmp`
  from Debian packages (`apt-get download` with a user-writable `Dir::State`/`Dir::Cache`,
  `dpkg-deb -x`, `initdb --auth=trust`, `postgres -p <port> -h 127.0.0.1 -k /tmp/pgsock`,
  `create extension vector`) and leaves it running for the roles after it, recording the
  `LD_LIBRARY_PATH`, `PATH` and `DATABASE_URL` it needs in the handoff's `trickyParts`. Two roles
  each built one (55432 and 55433): the first role's note is what spares the next the build.
- Stop your own `dispatch:local` before you complete, so its ports are free for the next role; a
  Postgres you built stays up.

## Evidence (LEGION-695, sjawhar/legion#1928)

The implementer's round-1 runs used the default ports; by round 2 `playwright.config.ts` refused
them (`already in use`), with no process visible in the container — the tester's `dispatch:local`
on 8777 with its own Postgres on 55433/`dispatch_tester`. A `DISPATCH_E2E_REUSE_SERVERS=1` attempt
against it failed all ten rows with `POST /api/v1/projects failed: 409 PROJECT_EXISTS`, since
`resetDatabase()` truncated the implementer's database while that server served another. Every
later run and proof used `DISPATCH_E2E_PORT=18777 FAKE_ENVOY_PORT=19021 FAKE_GITHUB_PORT=19022
PLAIN_HTTP_PORT=19023 FAKE_BROKER_PORT=19024` with `DATABASE_URL` on the implementer's Postgres at
55432 (`dispatch_e2e` for the suite, `dispatch_local` for `dispatch:local`), named in the PR body's
`E2E (implementer)` lines (`.legion/LEGION-695/implement.json`, round-2 `trickyParts`).
