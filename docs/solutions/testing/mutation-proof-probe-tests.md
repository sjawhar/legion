---
title: "Mutation-proof probe tests: prefix ids listed longer-first, and assert the contract rather than the retry cadence"
category: testing
tags:
  - bun-test
  - mutation-testing
  - fixtures
  - id-matching
  - prefix-match
  - event-sequence
  - worker-boot-watchdog
  - code-review
date: 2026-09-12
status: active
module: daemon
problem_type: testing
component: tmux.test.ts
severity: medium
applies_when:
  - A test proves exact matching of prefixed numeric ids (`%N`, `@N`, session or run ids that can be prefixes of one another)
  - A test asserts an event sequence that interleaves a retry/poll cadence with the behavior under test
  - A reviewer asks "which plausible bug would this test NOT catch?" and the answer is not obvious
related_issues:
  - "LEGION-9"
  - "sjawhar/legion#945"
---

# Mutation-Proof Probe Tests

Two findings from the review of `sjawhar/legion#945`, both found the same way: mutate the production code into a
plausible wrong implementation and see whether the test notices. Both were blind spots in tests that were green and
read correctly.

## 1. Id-matching fixtures need ids that are prefixes of one another, longer id listed first

`panePid` selects a `list-panes` row with `rows.find((r) => r[0] === target)`. The first fixture used pane ids
`%1531 / %1533 / %1534` with targets `%1533` and `%1535`. No id is a prefix of another, so **six** wrong
implementations passed all four cases: `r[0].startsWith(target)`, `target.startsWith(r[0])`, `r[0].includes(target)`,
`target.includes(r[0])`, `new RegExp(target).test(r[0])`, and a line-based `line.includes(target)`. The reversed
family is not academic: in Legion's real layout the architect is `%1` and every `%10`+ worker's target starts with it,
so `target.startsWith(r[0])` returns the architect's pid for every later worker — the original defect, back.

The fix is one case whose rows contain a prefix chain with the **longer id first**:

```ts
const prefixes = "%150 446716\n%1 3715931\n%15 4141285\n";
expect(await panePid(server({ stdout: prefixes, exitCode: 0 }), "%15")).toBe(4141285);
expect(await panePid(server({ stdout: prefixes, exitCode: 0 }), "%1")).toBe(3715931);
```

Order matters because `find` returns the first match. With ascending order (`%1, %15, %150`) the exact row is reached
before its extensions, so `r[0].startsWith(target)` still lands on the right row and only the reversed family is
caught. With `%150` first, a forward prefix match lands on `%150` for both targets and a reversed one lands on `%1`
for `%15`: every substring-style comparison in either direction picks a wrong row. Verified: the single case fails on
all six mutations and passes on `===`.

Rule: when a fixture proves exact matching of ids that can be prefixes of one another, include a real prefix chain
and put the ambiguous, longer id **before** the exact one in the underlying data.

## 2. Assert the contract, not the cadence the fixture happens to produce

The watchdog pid-probe case first asserted
`expect(events).toEqual(["workerClient", "isOmpPane:3003090", "workerClient", "retire"])`. The leading `workerClient`
count is `maxAttemptsPerInterval = max(1, ceil(intervalMs / BOOT_WATCHDOG_POLL_INTERVAL_MS))`
(`worker-boot-watchdog.ts`) — a consequence of `workerBootTimeoutSeconds: () => 0.01`, not of the row selection the
title is about. A poll-interval tuning change would fail a test named "probes the worker's own pane pid" with a
misleading diff.

The replacement asserts what the title claims — the only pid probe asked about the target's own pid, and the socket
probe and retirement followed it:

```ts
expect(events.filter((e) => e.startsWith("isOmpPane:"))).toEqual(["isOmpPane:3003090"]);
expect(events.slice(events.indexOf("isOmpPane:3003090"))).toEqual([
  "isOmpPane:3003090",
  "workerClient",
  "retire",
]);
```

It still kills the regression it exists for: with `const row = rows[0]` every interval asks about the architect's
`2363427`, which is alive, so the watch re-arms forever — the filter assertion receives eighteen
`isOmpPane:2363427` entries and no retirement.

Exact sequences are right when the order *is* the claim (the same file's timer-cleanup cases assert exact clear order
because that ordering is the bug being guarded). They are wrong when a fixed cadence parameter is interleaved with
the behavior under test; use `filter`/`slice`/`indexOf` to assert the invariant and let the cadence float.

## How to run the mutation check in place

No scratch copy is needed in a jj workspace; the working copy reverts by path:

```bash
sed -i 's/rows.find((r) => r\[0\] === target)/rows.find((r) => target.startsWith(r[0]))/' \
  packages/daemon/src/daemon/tmux.ts
bun test src/daemon/__tests__/tmux.test.ts        # expect: only the new case fails
jj -R "$LEGION_WORKSPACE" restore packages/daemon/src/daemon/tmux.ts   # run from the workspace root
bun test src/daemon/__tests__/tmux.test.ts        # expect: all pass
jj -R "$LEGION_WORKSPACE" diff --stat              # the mutated file must be absent
```

`jj restore <path>` resolves the path relative to the **current directory**, not the repo root — run from a package
directory with a root-relative path it prints `No matching entries for paths` and reverts nothing. Confirm the revert
with `diff --stat` before committing. Record the mutation's exact failure (`Expected: 4141285 Received: 3715931`) in
the handoff: "fails" alone does not show the test observes the right thing.
