---
title: "A constant that must equal a released number is pinned to a literal, never bounded"
category: legion
tags:
  - daemon-api-version
  - contract-bump
  - tests
  - forward-merge
  - pinning
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "INFRA-484"
  - "sjawhar/legion#2538"
---

# A constant that must equal a released number is pinned to a literal, never bounded

Extends docs/solutions/legion/a-contract-bumps-sweep-includes-test-expectations-spelling-the-bare-number-replace-them-with-the-packages-derived-constant-LEGION-636.md.

That document is right for an expectation that should track the current contract (`this daemon
requires <n>`): write it as the derived constant, and the next bump changes nothing. This rule is
about the other class, the one where the derived spelling hides the defect it exists to catch.

- When a constant must equal **the exact number at which a capability appeared**, and not merely
  sit below the current version, assert **equality against a hand-written literal** in the one
  test where both packages are importable. A bound (`<=`, `>=`) is satisfied by the right value
  and by the wrong one alike, so a forward merge that renumbers the version leaves the constant
  behind under a green suite.
- Keep the bound as a **second** assertion in the same test. It catches the other direction,
  which is a different failure.
- A test whose purpose is to catch a wrong constant spells its numbers **absolutely**. A relative
  spelling (`Constant-1`, `Constant+1`) moves with the constant, so reverting the constant keeps
  the test green: it inherits the defect instead of catching it.
- Relative spellings everywhere else become safe **because** one test pins the constant. Say so in
  a comment at both ends, so a later reader does not relax the pin to unblock a merge.
- Treat an edit to that literal as a contract change, not a test fix.

Renumbering is the normal case in this repository, not an accident: `api/version.go` records
contracts renumbered after a release took the number first. Any constant derived from a released
serial — a daemon contract, a plugin manifest's `daemonApiVersion`, a wire schema version — needs
this pin. Which number to take after a collision is
docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md;
this is the half about the constants derived from it.

## Evidence (INFRA-484, sjawhar/legion#2538)

`supervise.PodUpdateContract` names the contract that introduced the `ready_for_update` operation.
`api/version_test.go` asserted only `supervise.PodUpdateContract <= DaemonAPIVersion`. A forward
merge against main moved this change's own contract from 45 to 46 and left the constant at 45. The
suite stayed green.

What that would have done on the first boot of the release, from `supervise/plugin.go`'s own
comment: main's released plugin declares 45 and has no `ready_for_update`, so at 45 every one of
those workers read as **able to call an op its plugin does not contain**. No turn's end counted as
its answer, and the stale-plugin lane opened a plugin round instead of relaunching it. Every update
round of the first roll would have stalled and no pod would have been replaced — the one deploy the
change exists for.

The second half of why it hid: every other test spelled the number relatively
(`PodUpdateContract-1`, `PodUpdateContract+1`), so each moved with the constant and passed at
either value. The test added to catch this was written with literals for that reason, and when it
was first written relatively it passed with the constant reverted — the defect reproduced inside
the fix's own test.

The pin is `TestPodUpdateContractIsTheContractThatIntroducedTheOp`, which asserts `== 46` against a
local `const introducedReadyForUpdate = 46` and keeps the upper bound below it.
