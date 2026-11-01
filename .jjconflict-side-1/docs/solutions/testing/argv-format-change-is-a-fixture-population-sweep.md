---
title: "Changing an argv format string is a fixture population sweep: `Array.includes` is whole-element equality and generic fixtures answer in the old shape"
category: testing
tags:
  - bun-test
  - fixtures
  - fake-runner
  - argv
  - tmux
  - array-includes
  - population-not-sample
date: 2026-09-12
status: active
module: daemon
problem_type: testing
component: processes.test.ts
severity: medium
applies_when:
  - A tmux/gh/jj argv element (a `-F` format string, a subcommand, a flag value) changes in production code
  - Test fixtures gate replies with `command.includes("<literal>")` over the argv array
  - Fixtures answer a subcommand generically (no format check) with a synthetic reply the parser must read
related_issues:
  - "LEGION-9"
  - "sjawhar/legion#945"
---

# Changing an Argv Format String Is a Fixture Population Sweep

## Context

`sjawhar/legion#945` changed one argv element in `panePid`: `-F "#{pane_pid}"` became
`-F "#{pane_id} #{pane_pid}"`, and the parser started reading a `<pane_id> <pid>` row instead of a bare pid. The
spec estimated "≈20 fixture sites" by counting predicates that name `#{pane_pid}`. The real population in
`packages/daemon/src/daemon/__tests__/processes.test.ts` was 46 edits plus one helper, and 3 more in
`worker-boot-watchdog.test.ts`. Two mechanisms, both invisible to the spec's count.

## Mechanism 1: `Array.prototype.includes` is whole-element equality

Fixtures in this repo emulate a command runner over `command: string[]` and branch with
`command.includes("#{pane_pid}")`. After the change the format element is the single string
`"#{pane_id} #{pane_pid}"`, so `includes("#{pane_pid}")` is **false** — not stale-but-true, false. The probe falls
through to the fixture's default reply (usually `{ stdout: "", exitCode: 0 }`), the parser reads `undefined`, and the
test sees "dead" where it meant "alive". Every predicate naming the old element must be rewritten to the new
whole element, and the argv pin (`toContainEqual([... "-F", "#{pane_pid}"])`) with it.

## Mechanism 2: generic fixtures answer in the old shape

~26 fixtures had no format check at all — `if (command[3] === "list-panes") return { stdout: "12345\n", exitCode: 0 }`
— and were correct for every `list-panes` probe as long as each parser tolerated a bare pid. The new parser reads the
pid from column 2; a bare-pid row has no column 2 → `undefined` → "dead". These sites never mention the format string,
so a grep for it cannot find them.

## Mechanism 3: tests that pass for the wrong reason

After the parser change and before the fixture cutover, only one of three affected watchdog cases failed. The other
two never observe the alive/re-arm path their titles describe: one asserts retirement, which a dead read also
produces; the other asserts only that every timer is cleared after `cancel()`, which holds whether the watch
re-armed or retired. A green suite mid-cutover is not evidence that the remaining fixtures are fine; the sweep is
complete when the grep sweep is clean, not when the run is green.

## The procedure

1. **Grep for the population, in two directions, before estimating scope.** (a) The old literal as an argv element
   (`#\{pane_pid\}"`) — every hit is a predicate or a pin to rewrite. (b) The synthetic reply the old parser accepted
   (`"12345\n"`, `"4242\n"`) inside any branch for that subcommand — every hit with no format guard is a generic
   fixture to cut over. Enumerate both lists in the plan; the count is what grep returns, not an estimate.
2. **Derive the reply from the argv in one helper** so fixtures and parser move together next time:

   ```ts
   function livePanes(command: string[], pid = 12345): { stdout: string; exitCode: number } {
     const target = command[command.indexOf("-t") + 1];
     return { stdout: `${target.startsWith("%") ? target : "%1"} ${pid}\n`, exitCode: 0 };
   }
   ```

   The helper answers the probe with the probed pane's own row, so cutting a generic site over is
   `return livePanes(command)`. (When the daemon still probed some panes through `-F "#{pane_id}"`-only
   formats -- the since-removed `firstPaneId` backfill and `windowAlive` -- the helper also kept those formats on
   the bare-pid reply they always had; a helper like this should answer every format the parser of the day
   distinguishes, and lose branches as the parser loses them.)
3. **Leave the unrelated population alone, by name.** Fixtures that always answer exit 1, or that serve
   `list-panes -a` for `listUnknownPanes`, are a third list in the plan, so the implementer can account for every
   `list-panes` site rather than sample.
4. **Sweep for leftovers with the same two greps after editing.** Every remaining `"12345\n"` must be a
   `new-window`/`split-window` reply or inside the helper; every remaining `#{pane_pid}"` must be the new whole
   element (or `new-window`'s three-column format).
5. **Record the expected red shape in the plan and compare against it.** The planner ran the new tests against the
   old parser in a scratch copy and wrote down the exact failures (2 fail / 2 pass; watchdog events
   `[workerClient, workerClient, retire]`). The implementer confirmed the same shapes before touching production code.
   "Fails" is weak evidence; "fails exactly as predicted" says the test observes what it claims to.

## Why the spec's count was wrong

The spec counted textual mentions of the format token. Mechanisms 1 and 2 are both about what the *parser* will
accept, not what the *predicate* says — a reply-shape change reaches every fixture whose reply the parser reads,
whether or not the fixture knows which format it is answering.
