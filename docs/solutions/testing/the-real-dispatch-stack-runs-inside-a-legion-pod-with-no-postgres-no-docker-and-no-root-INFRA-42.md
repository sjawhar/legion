---
title: "The real Dispatch stack runs inside a Legion pod with no Postgres, no docker and no root"
category: testing
tags:
  - dispatch
  - e2e
  - postgres
  - pgvector
  - playwright
  - browser
  - legion-pod
date: 2026-10-20
status: active
module: packages/dispatch
related_issues:
  - "INFRA-42"
  - "sjawhar/legion#2086"
---

# The real Dispatch stack runs inside a Legion pod with no Postgres, no docker and no root

A `packages/dispatch` change is proved by driving the dashboard — the real Go server, the
production SPA bundle and a real browser — not by citing a CI run. An issue pod has no `psql`, no
Postgres, no docker and no root, which reads like a blocker and is not. This recipe stood the whole
stack up inside one; everything lives outside `$LEGION_WORKSPACE` so no tracked file moves.

1. **Postgres with pgvector, rootless.** Download five Debian trixie `.deb`s from
   `https://deb.debian.org/debian/pool/main` — `postgresql-17`, `postgresql-client-17` and
   `libpq5` of the same version (`p/postgresql-17/`), `postgresql-17-pgvector` (`p/pgvector/`) and
   `libicu76` (`i/icu/`) — and unpack each with `dpkg-deb -x <file> <root>`. pgvector is not
   optional: the server's migration 0072 runs `create extension if not exists vector` and boot
   refuses without it. `libicu76` is the other non-obvious one: the image ships no ICU and the
   postgres binary links it, so `initdb` fails with a loader error naming neither Postgres nor ICU.
   The extension lands at `root/usr/lib/postgresql/17/lib/vector.so`, which Postgres finds because
   it resolves `pkglibdir` relative to its own binary.
2. **Start it** with `PATH=<root>/usr/lib/postgresql/17/bin:$PATH` and
   `LD_LIBRARY_PATH=<root>/usr/lib/x86_64-linux-gnu:<root>/usr/lib/postgresql/17/lib`, then
   `initdb -D <data> -U postgres --locale=C --encoding=UTF8`, `pg_ctl -D <data> -o '-k <sock> -h
   127.0.0.1 -p 5433' -w start`, create the database and `create extension if not exists vector`
   in it.
   **Note 2026-10-25 (INFRA-221):** still the step — `skills/legion-worker/references/tools.md`
   draws the withheld-`operator-setup` line by what a prefix leaves resolving, and this one is
   reached through the `PATH` and `LD_LIBRARY_PATH` exported around the harness's own commands,
   not left on the shell a role goes on working in.
3. **The server:** `DATABASE_URL='postgres://postgres@127.0.0.1:5433/<db>?sslmode=disable'
   DISPATCH_E2E_PORT=8777 bash packages/dispatch/e2e/run-server.sh` from the workspace. That
   script is the whole fixture environment (dev sign-in, insecure cookie, test hooks, agent token,
   `web/dist`) and drops every inherited `DISPATCH_*`/`ENVOY_*`/`NATS_*` variable itself, so a
   Legion pane's own credentials never reach it. Start `bun e2e/fake-envoy.ts` (9021) for the
   Agents page's registry and `bun e2e/fake-broker.ts` (9024) for credential requests. **The fake
   broker is load-bearing for the empty states**: without it every `/api/v1/credential-requests`
   read is a 503, `credentials.status !== "listed"`, and `emptyStateMessage` returns undefined — so
   `Nothing needs you` and `Nothing matches this filter` are simply absent and read as a regression
   in the change under test.
4. **The SPA:** `cd packages/dispatch && bun run build:web`, which the server serves from
   `web/dist`, so the page under test is the production bundle. This pod's installed
   `proof-sdk-upstream` has no `src/` tree and the build fails on a path that looks like a missing
   file in this repository: clone `sjawhar/proof-sdk` at the commit `packages/proof-editor`'s
   dependency pins and copy its `src/` into
   `packages/proof-editor/node_modules/proof-sdk-upstream`. The same gap is why this pod's unit
   suite fails its Markdown tests while CI is green — not a finding.
5. **Seeding:** import `e2e/api.ts` and `e2e/agents.ts` from a script outside the repository and
   call `createProject`/`createIssue`/`patchIssue`/`createAsk`/`createArtifactAsk`/`createComment`
   plus `setLiveSessions`; each sign-in is `GET /auth/_dev/signin?login=<name>`, which is also what
   creates the person. Do not call `e2e/seed.ts`'s `resetDatabase` unless every harness listener is
   up — it resets the fake Envoy, GitHub and broker too, and dies with ECONNREFUSED otherwise;
   truncating every public table except the migration ledger is the cheap equivalent. A question on
   a project document is `createArtifactAsk(artifact.id, …)`; the issue ask route with an artifact
   anchor answers 404, which reads like a permissions problem and is not.
6. **The browser:** the harness's own Chromium (Playwright's browsers are not installed and need
   system libraries the image lacks). `tab.goto` often times out waiting for `load` on this SPA
   while the page is in fact rendered, so navigate with `{wait_until:'domcontentloaded'}` and
   settle with a short sleep; an `evaluate` fired while the app is still redirecting throws
   `Execution context was destroyed`, so retry the read rather than treating it as a page error.
   **One browser is one cookie jar**: switch reader by visiting the dev sign-in route again, never
   by opening a second tab — doing otherwise nearly recorded one person's inbox as another's.
7. **Reading the page:** rows are `[data-testid^=ask-]`, their band is `[data-inbox-section]`, the
   Inbox's filter is `button[aria-label="Filter by assignee"]` over
   `[role=listbox][aria-label="Assignee options"]`, and routing a method/status/body over a request
   is how a refusal is forced (a faked 500 on `PATCH /api/v1/issues/<KEY>` proved the Assign-to-me
   refusal path).

## Evidence (INFRA-42, sjawhar/legion#2086)

The tester built this rig when the implementer's proof was a CI run id, and drove eight of the
implementer's nine acceptance criteria on it criterion by criterion, finding one defect CI could
not have shown: an Agents card's pill was recorded as opening exactly the rows it counted, and the
list it opens is a superset (a pill of 2 opening 5 rows). The implementer then reused the running
rig to drive the criteria its own round added — among them a `/auth/whoami` held for several
seconds, which showed the app shell renders nothing but its title during that window, so a guard
being removed there had no user-visible effect either way. Both readings are facts about the
product that no unit suite or CI lane reports.
