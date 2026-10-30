---
title: "A load recipe's dependency settings are part of the recipe: a fast Postgres closes the window you are hunting"
category: testing
tags:
  - race-conditions
  - reproduction
  - postgres
  - legion-pod
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/startup
related_issues:
  - "INFRA-757"
  - "sjawhar/legion#2517"
---

# A load recipe's dependency settings are part of the recipe: a fast Postgres closes the window you are hunting

Extends `docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md`,
whose recipe is busy loops, an fsync writer and a count of contenders.

**Name the settings of every dependency the window runs through, and record them with the
result.** A race whose window is the time a dependency takes to commit is closed by a dependency
tuned to be fast. A scratch Postgres started with `fsync=off synchronous_commit=off
full_page_writes=off` — the obvious thing to do for a test database — removes exactly the interval
a test of an advisory-lock protocol is racing against.

**So a clean loop is evidence about the recipe before it is evidence about the race.** Treat
0 failures in N under load as "the recipe did not reach the window" until the recipe's
dependencies are shown to be the production ones, and say which settings you used.

## Evidence (INFRA-757, sjawhar/legion#2517)

`TestTwoAnnouncersAtOnceAnnounceAndPublishOnce` fails when all twenty of the losing process's
passes run inside the winner's announcing transaction: a pass that loses
`pg_try_advisory_xact_lock` only reads and costs a fraction of a millisecond, while the winner must
insert and commit.

Against the pod's rootless PostgreSQL 17 with `fsync=off synchronous_commit=off
full_page_writes=off`, 30 runs of the pre-fix binary under the recipe gave **30 pass, 0 fail**
(wall 84 s). The same loop against the same server with its default durability gave **13 pass, 17
fail of 30** (wall 698 s), every failure `started_test.go:271: FleetCommit = <nil>`, which is the
failure CI had reported. The only difference was the three settings.
