---
title: "Dispatch Playwright harness from a Legion pane: the harness owns its Envoy, own your ports and database on a shared box, and never write test output to a fixed /tmp path"
category: testing
tags:
  - playwright
  - dispatch
  - e2e
  - envoy-url
  - worker-pane
  - shared-box
  - run-server.sh
  - fake-envoy
  - hermetic
date: 2026-09-14
status: active
module: packages/dispatch/e2e
related_issues:
  - "LEGION-94"
  - "sjawhar/legion#1084"
  - "LEGION-154"
symptoms:
  - "Envoy unreachable: envoy listener unavailable: GET /v1/sessions returned 503 in the Conversation composer during a local e2e run"
  - "GET /api/v1/agents 503 and GET /api/v1/issues/<key>/subscribers 503 in the trace's network log while every other request is 200"
  - "a scenario that passes 9 of 10 fails once with window.scrollY off by the height of a banner"
  - "EACCES: permission denied, open '/tmp/askcard-1280.png'"
  - "Playwright requires Node.js 20 or higher"
---

# Dispatch Playwright Harness from a Legion Pane

The Dispatch dashboard's browser suite (`cd packages/dispatch && bun run e2e`) is the package's
own integration harness: the Playwright config starts the fake Envoy (`e2e/fake-envoy.ts`) and the
real Go server (`e2e/run-server.sh` → `go run ./cmd/dispatch`) itself, against a Postgres the
developer supplies. It is allowed as a worker's local proof. Three things about running it from a
Legion pane on a shared box cost LEGION-94 an evening; none of them exists in CI.

## 1. Your pane exports `ENVOY_URL`; the harness no longer honours it

Every Legion pane carries `ENVOY_URL=http://127.0.0.1:9020` (the real Envoy listener, for the
pi-envoy extension). `run-server.sh` used to build the Go server's Envoy address as
`${ENVOY_URL:-http://127.0.0.1:${FAKE_ENVOY_PORT:-9021}}`, so from a pane the server bypassed the
fake Envoy the config had just started and talked to production Envoy. CI has no `ENVOY_URL`, so
CI never saw it.

What it looked like: one run in ten of the iPhone-project scenario
`doc.e2e.ts › tab round-trips keep one document connection …` failed with `window.scrollY` 664
where 500 was recorded. The trace's frames showed a red `Envoy unreachable: envoy listener
unavailable: GET /v1/sessions returned 503` banner in the Conversation composer — the real
listener was answering 503 at that hour — and the HAR showed `GET /api/v1/agents 503`. On the
tab return, the never-succeeded agents query refetched, the banner disappeared and reappeared,
and `ReaderPosition` compensated each height change to keep the anchored turn in place, which is
exactly what moves raw `window.scrollY`. With `ENVOY_URL` unset the scenario passed 10/10; with the
banner forced (`ENVOY_URL=http://127.0.0.1:9`, a closed port) it failed 10/10. Not a flake in the
code under test; a harness pointed at the wrong Envoy.

Beyond the wrong-answer risk, the `btw` and `message-agent` scenarios make the server POST
`/v1/messages/send` to its Envoy — from a misconfigured pane that was a write attempt against
production Envoy, not the fake.

Closed in the harness, not in the operator's habits (LEGION-154): `run-server.sh` now reads its
three real inputs (`DATABASE_URL` and the harness ports), unsets every inherited `DISPATCH_*`,
`ENVOY_*` and `NATS_*` variable, and launches the server with an environment it lists in full —
`ENVOY_URL` built from `FAKE_ENVOY_PORT` alone. The fake Envoy is the only Envoy this harness is
ever meant to talk to (`e2e:deployed` runs against `PLAYWRIGHT_BASE_URL` and starts no server),
and the same sweep drops the next variable the server learns to read: the dashboard origin from
`~/.config/opencode/envoy.json`, an `ENVOY_TOKEN`, a cookie signing key or a real GitHub App were
all reachable the same way. Proof: with `ENVOY_URL=http://127.0.0.1:1` exported, the pre-fix
script answers `GET /api/v1/agents` with `dial tcp 127.0.0.1:1: connect: connection refused` and
the fixed one lists the fake's seeded sessions and records the targeted send in
`GET /__fixture/sends`.

So no `unset` is needed before a run, and a pane variable can no longer explain a harness
failure. What can: the ports and database below.

## 2. On a shared box, own the ports and the database

The defaults — Go server on `8777`, fake Envoy on `9021`, database `dispatch_c` on the
`dispatch-pg` container at `127.0.0.1:55432` — are shared by every agent running the suite on the
box, and `e2e/seed.ts` truncates the database before every scenario. Two agents on the defaults
corrupt each other's runs silently.

```sh
docker exec dispatch-pg createdb -U postgres dispatch_<issue>      # once
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
DISPATCH_E2E_PORT=87NN FAKE_ENVOY_PORT=90NN \
  bun run e2e
```

`DATABASE_URL`, `DISPATCH_E2E_PORT`, and `FAKE_ENVOY_PORT` are read by the config, the server
script, the seed, and the API helpers alike (`e2e/api.ts`, `e2e/agents.ts`, `e2e/seed.ts`), so the
three variables move the whole harness together. Start nothing else: the container from
`packages/envoy/scripts/dev-postgres.sh` and what the Playwright config starts are the harness.

## 3. Test output goes to `testInfo.outputPath()`, never a fixed `/tmp` name

Four scenarios (`inbox`, `agents`, `ask-blocks`, `polish`) wrote debugging screenshots to
`/tmp/askcard-1280.png`, `/tmp/agents-390.png`, `/tmp/specchrome-*.png`,
`/tmp/polish-*-${viewport}.png`. On this box those files already existed, owned by another user,
and every one of those scenarios failed on both projects with
`EACCES: permission denied, open '/tmp/askcard-1280.png'` — eight red scenarios that had nothing
to do with the change. CI's `/tmp` is fresh, so CI never sees this either. The paths now come from
`testInfo.outputPath("askcard-1280.png")`, the per-test directory under `e2e/test-results` that
the workflow uploads on failure — which is also where a debugging screenshot is useful.

Rule: a test writes files only under `testInfo.outputPath()` (or `testInfo.attach`). A fixed
absolute path is a shared resource, and a shared resource is someone else's on a shared box.

## 4. The pane's toolchain is not the CI runner's

Playwright 1.63 refuses Node 18 (`Playwright requires Node.js 20 or higher`); this box's pane
user had only `/usr/bin/node` 18 on `PATH`, and mise's Go was installed but not on the login-shell
`PATH`. Nothing may be installed on the box (operator rule), so the run used an ephemeral Node 22
extracted under `/tmp` and `~/.local/share/mise/installs/go/<version>/bin`, both prepended to
`PATH` for the run only. `bun run e2e:install` (Chromium) is per-user and had to be run once for
this user even though another user's Chromium was present.

## Reading a local failure

A local failure's trace is at `e2e/test-results/<scenario-dir>/trace.zip`; unzip it and read
`test.trace`, `*-trace.network`, and the frames under `resources/` exactly as for a CI artifact
(see
[`a-flake-spec-names-no-cause-before-the-retained-trace-is-read.md`](./a-flake-spec-names-no-cause-before-the-retained-trace-is-read.md)).
Before reading the code under test, look for a 503 in the HAR and a red banner in the frames: on
a pane, the harness is the first suspect.
