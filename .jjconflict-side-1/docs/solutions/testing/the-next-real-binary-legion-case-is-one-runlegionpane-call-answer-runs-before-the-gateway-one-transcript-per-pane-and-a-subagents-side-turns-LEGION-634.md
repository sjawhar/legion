---
title: "The next real-binary Legion case is one runLegionPane call: answer runs before the gateway, one transcript per pane, and a subagent's side turns"
category: testing
tags:
  - omp-harness
  - real-binary
  - runLegionPane
  - stand-in-daemon
  - task-subagent
  - fixture-naming
date: 2026-10-09
status: active
module: packages/pi-shared/test/omp-harness.ts, packages/pi-legion/extensions
related_issues:
  - "LEGION-634"
  - "sjawhar/legion#1865"
  - "LEGION-630"
---

# The next real-binary Legion case is one runLegionPane call: answer runs before the gateway, one transcript per pane, and a subagent's side turns

Extends docs/solutions/testing/proving-a-refusals-absence-on-the-real-oh-my-pi-the-omp-harness-with-the-bases-extension-as-the-before-leg-LEGION-630.md.
That record names the harness's primitives (`ompRoot`, `serveStandin`, `writeStandinProfile`,
`spawnRpc`, `messageStream`) and the per-file `tsconfig.json` include; since LEGION-634 the Legion
pane they were composed into twice is one export, `runLegionPane` in
`packages/pi-shared/test/omp-harness.ts`, and a new case calls it rather than copying the
composition.

## The rules

- **Build a Legion pane case on `runLegionPane(binary, replies, options, cleanup)`.** It owns the
  scratch root, `state/secrets` and `bin`, the stand-in server (the model gateway with the
  self-check short-circuit and the scripted replies, the daemon's `claims/register` from
  `contracts/fixtures/daemon-api/register.json`, `claims/ready`, `grants`, the Envoy listener
  catch-all), the profile, the stand-in `legion` on PATH that logs its arguments and the grant it
  read, both plugin entries by path, the settle (terminal `agent_end`, or `quietMs` of gateway
  silence) and the readers (`turns`, `selfChecks`, `toolResults`, `legionLog`,
  `transcriptEntries`). The request-shape readers only one test asserts with (`userText`,
  `toolNames`, `runJj`, `expectOnlyControlRefused`) stay in that test.
- **Every fixture string a case asserts derives from `name`.** `<name>-grant-<n>`,
  `<name>-secret`, `<name>-machine`, `<name>-boot`, `<name>-dispatch-token`, the claim token
  `legion-<name>-<issue lower-cased>-<role>` and the grant file under it. Pick the name before
  writing the assertions, and assert the derived strings, never a literal from another test.
- **Workspace setup goes in `prepare`**, which runs once the root, `bin` (first on the pane's PATH)
  and `state/secrets` exist and before the profile is written and Oh My Pi starts: link the `jj`
  this process finds into `bin`, `jj git init` the workspace, describe a commit — what an issue
  workspace holds.
- **Give `answer` only the case's own routes, never `/anthropic/v1/messages`.** `answer` is tried
  before every runner route, the gateway's included, and the reply counter (`answered`) and the
  `lastAnsweredAt` stamp `quietMs` waits on are advanced only inside the runner's own gateway
  branch; an `answer` that intercepts a Messages request leaves both stale. A Dispatch route such
  as `/api/v1/asks/open` is what `answer` is for.
- **`transcriptEntries(customType)` wants exactly one `.jsonl` under the sessions directory**, and
  throws naming what it found otherwise. A pane that spawns a `task` subagent has two (the
  subagent's sits in a directory named after the parent's file), so a case with a subagent reads
  the parent's transcript itself, or the helper first learns to pick the top-level file.
- **A `task` subagent's turns reach the same stand-in gateway, and so do the host's side requests.**
  `task` returns at once (`Spawned agent …; Results auto-deliver`) and the parent must call `wait`
  for the result; the host also sends label and skill-routing-hint requests to the gateway (no
  system prompt, no tools). Every non-self-check Messages request consumes the next scripted
  reply, so a case with a subagent routes replies by the request's content, not by position.
- **A harness that ordinary-session suites import reads Legion fixtures inside the runner, not at
  module scope.** `pi-envoy`'s `dispatch-first-omp.test.ts` imports `omp-harness.ts` for `spawnRpc`
  alone; a module-level `readFileSync` of `register.json` would run on its import for nothing.
- **A no-copy grep targets a definition, not a name.** `pathname === "/legion/v1/claims/register"`
  finds a route definition; `claims/register` also finds every assertion on the paths a pane
  called, which the hoist rightly leaves in place.

## Evidence

- `packages/pi-shared/test/omp-harness.ts` after LEGION-634: `const own = options.answer?.(url,
  body); if (own !== undefined) return own;` sits ahead of the `/anthropic/v1/messages` branch,
  where `lastAnsweredAt = Date.now()` and `answered += 1` are stamped; `transcriptEntries` throws
  `want one transcript under <sessions>, found <files>` unless the filter finds one `.jsonl`. The
  reviewer of sjawhar/legion#1865 named the `answer` ordering as its one harness finding (minor,
  fast-follow: a comment).
- `packages/pi-legion/extensions/legion-phase-stall-omp.test.ts` and
  `legion-role-tools-omp.test.ts` lost 318 and 380 lines each to the hoist and kept every
  assertion: `grant stall-grant-1`, `legion-tools-<issue>-<role>`, the `jj undo` refusal, the
  hashline edit landing on disk. The role-tools test's `jj` symlink, `git init` and `describe`
  moved into `prepare`; the stall test's open-asks route into `answer`.
- The tester's uncommitted round-1 drive of a `task` subagent through `runLegionPane`
  (`.legion/LEGION-634/test.json`, proof 4 and observations): the subagent's five turns offered
  `[read, bash, edit, eval, glob, grep, task, wait, web_search, write, yield]` and no `legion`
  tool; `task` returned at once; label and skill-routing-hint side requests reached the gateway;
  the sessions directory held two `.jsonl` files, so `transcriptEntries` could not be used and the
  parent's transcript was read directly.
- The plan's no-copy grep (`… |#!/bin/sh|claims/register`) printed one line after the hoist: the
  stall test's pre-existing `toEqual(["/legion/v1/claims/register", …])`. The route-definition
  form printed nothing, and 14 lines over `main@origin`'s two files.
