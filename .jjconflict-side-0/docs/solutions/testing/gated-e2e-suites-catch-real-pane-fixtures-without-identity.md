---
title: "Run the LEGION_E2E/LEGION_TMUX_LIVE suites locally before every push of a lifecycle change, and bring every fixture main lands under the branch's contract at every rebase"
category: testing
tags:
  - e2e
  - tmux
  - fixtures
  - rebase
  - LEGION_E2E
  - LEGION_TMUX_LIVE
  - identity-contract
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-27"
  - "sjawhar/legion#981"
applies_when:
  - A branch changes what a recorded locator must carry, or what a probe/kill path trusts
  - Tests in the daemon package seed locators by hand (real or fake panes)
  - The branch is rebased over daemon PRs that landed with their own tests
---

# Run the LEGION_E2E/LEGION_TMUX_LIVE suites locally before every push of a lifecycle change, and bring every fixture main lands under the branch's contract at every rebase

## The gap unit tests cannot see

CI runs `bun test` with `LEGION_E2E=1 LEGION_TMUX_LIVE=1` (`.github/workflows/pr-and-main.yaml`);
a plain local `bun test` skips those rows. LEGION-27's first push went red on exactly two of them:
`real-shutdown-e2e.test.ts` seeds worker and root locators by hand from a **real** tmux pane, and
did so without `panePid`/`paneStartTicks`. Under the new contract an identity-less locator never
verifies, so `stopProcess` treated the real pane as already gone and never killed it -- the row's
`waitForPaneGone` timed out. No fake in the unit suite could have shown this: the fake `list-panes`
already reported whatever identity the fixture author put in the locator. Only the row that owns a
real pane, real `/proc`, and a real `kill-pane` exercises the seam between "what the locator says"
and "what the machine has".

Rule: before pushing a change to process liveness, identity, or the tmux runtime, run the package
the way CI does --

```bash
cd packages/daemon && LEGION_E2E=1 LEGION_TMUX_LIVE=1 bun test
```

-- and treat a red gated row as a contract finding, not a flake. The gated rows are the ones that
seed real panes; they are the rows a contract change is most likely to break and least likely to
be caught by the fakes.

## Every rebase brings new fixtures that predate the contract

A long-lived branch that changes what a locator must carry gets rebased over daemon PRs whose
tests were written against the old contract. Each one lands a fixture that is correct on main and
wrong on the branch, and each shows up as a failure that looks unrelated:

- #956's real e2e built `TmuxRuntime` with a dep the branch had deleted (a `tsc` error).
- #957-era's boot-retire `startDaemon` test seeded a dead worker's `%7` without identity and pinned
  `killedPanes === ["%7"]` -- under the contract the kill is refused for an identity-less pane.
- #991's ready-time bystander-retire test seeded a locator without identity and asserted the kill's
  `StopFailed` -- same shape.

The fix is always the same, and never a weakened assertion: seed the identity the fake `list-panes`
reports (`paneIdentity()` / `livePanes(command)` in `processes.test.ts`), so the pane verifies and
the kill the test is about is actually attempted. Put each such bring-up in its own commit, placed
first, so every commit in the chain stays green and the reviewer can see it is a fixture, not a
behaviour change. Sami's rule applies: a pre-existing test you break is yours to fix at the root,
with the reason in the commit message.

## The complementary fake rule

A fake that means "the pane is gone" must say so on stderr (`paneGone()`); a bare nonzero exit is a
listing failure the runtime refuses to act on. See
`../daemon/a-failed-list-panes-proves-nothing-about-the-pane.md`.

## Related

- `scratch-daemon-rig-proves-what-unit-tests-cannot.md` -- the next level up: a throwaway daemon on
  a real tmux server for the proof no test row can give.
- `argv-format-change-is-a-fixture-population-sweep.md` -- when the *fake's* format changes, sweep
  the whole population.
- `../daemon/a-tmux-pane-id-is-not-a-process-record-and-verify-its-identity.md` -- the contract the
  fixtures have to satisfy.
