---
title: "A tolerance for a lagging timestamp ships first, inside the reader, and its doc states the rule, not the timeline"
category: envoy
tags:
  - rollout-order
  - shared-bucket
  - doc-comments
  - reaper
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-610"
  - "INFRA-514"
  - "sjawhar/legion#2374"
---

# A tolerance for a lagging timestamp ships first, inside the reader, and its doc states the rule, not the timeline

A writer change can make a stored timestamp lag the truth, for example by skipping rewrites of an
unchanged row. Before that change ships:

- Release the readers' tolerance on its own first, on every process that reads or reaps the shared
  store. Count those processes from the stored rows (their `machine_id` values) and the deploy
  host list, not from the docs. Write the release order in the package's `AGENTS.md`.
- Name the tolerance as one exported constant beside the writer's rule, and add it inside the
  reader (`Reap`), not at each call site, so no caller can leave it out.
- Write the constant's doc comment as the contract both sides keep ("a row's `UpdatedAt` may lag
  the last heartbeat by at most this; a writer may skip an unchanged row younger than this and must
  rewrite it at this age"). Never write "today every write…; a later build will…": that sentence
  is false the day the later build lands.
- In the same change, sweep every restatement of the old number, in digits and in words
  (`10*time.Minute`, `10 minutes`, `ten-minute`), across `packages/`, `docs/` and `.dispatch/`,
  client code in other languages included. Also sweep every caller and every test comment that
  passes a literal: with the tolerance inside the reader, a grace of `0` no longer means "delete
  every dead row". Write test ages as sums of the constants.

The sweep rule for contract numbers in
`docs/solutions/legion/a-contract-numbers-prose-is-declaration-history-or-current-write-current-as-a-pointer-and-sweep-docs-solutions-too-LEGION-578.md`
is the same discipline, applied there to contract numbers rather than durations.

## Evidence

- `packages/envoy/internal/store/kv.go:1089-1094` declares `InterestReapGrace` (10 minutes) and
  `InterestRefreshAge` (5 minutes). `kv.go:1106` adds the second inside `Reap`.
  `packages/envoy/AGENTS.md:2109` states the release order: no build that skips unchanged interest
  writes runs while any listener on the shared `envoy_interests` bucket reaps at 10 minutes.
- The plan counted three listeners from the rows' machine ids (envoy-hub 367, gke 224) plus the
  hub's NATS host list, not from the repository's descriptions of other runtimes.
- The architect sent the change back at acceptance (commit `aad325ea`) for two comments it had
  made stale. One was `InterestRefreshAge`'s doc ("Today every heartbeat rewrites the row; a later
  build will skip…"). The other was `TestInterestReaper`'s "Reap with 0 grace window — all dead
  sessions should be reaped", which became false once the 5-minute allowance moved inside `Reap`.
- `packages/pi-envoy/extensions/envoy.ts` around line 2092 said "ten-minute stale-interest grace
  window" and also wrongly said the role claim is reaped with the row. Only a sweep for the number
  in words found it.
