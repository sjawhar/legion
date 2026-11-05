---
title: "A guard the base adds to a shared TestMain disarms every product child that re-enters the test binary, and the guard is a test's last line, never its only one"
category: testing
tags:
  - test-isolation
  - fixtureenv
  - helper-child
  - legion-pod
  - TestMain
date: 2026-11-04
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2642"
---

# A guard the base adds to a shared TestMain disarms every product child that re-enters the test binary, and the guard is a test's last line, never its only one

Extends docs/solutions/testing/the-environment-is-gits-third-config-scope-and-a-pod-only-failure-may-be-a-missing-isolation-pr2600.md.

That page's rule is to isolate a test's environment. These are the two failure shapes that rule
grows once a package both guards its environment and re-enters its own binary as a product process.

- **Census every process that re-enters the test binary when the base adds an environment-stripping
  guard to a shared `TestMain`.** A guard that removes a whole prefix removes exactly the variables
  such a child is dispatched on, and the child then runs as an ordinary test selection: it returns,
  prints `PASS` and exits 0. Read that signature for what it is — the supervisor sees a process
  that died at once, not a test failure — and branch on the child's marker before the guard, with
  the transitivity argument written down (the child was started by a guarded process and inherits
  that environment, so it holds nothing of the host's either).
- **A test supplies the path the code reads, at the point the code merges its environment.** Where
  production merges a request's environment over the process's and then reads a path from the
  result, a test whose request names no path reaches whatever the process holds — in a Legion issue
  pod, the live workspace its roles work in. Put the entry in the fixture that builds every request,
  not at the one call site a review found, so the guard is the second line of defence rather than
  the only one.
- **A test whose subject is a guard declares, per case, whether it can fail with the guard removed.**
  A case that reads the isolated environment rather than the product's merge point passes either
  way; say so in its own comment. Set a decoy and assert it reads back before testing the isolation,
  so what passes is the isolation's doing and not an environment that held nothing anyway.
- **A fixture's home lives outside the directory under test, and the code under test is given the
  same home.** A home inside the directory puts the tools' own files into the change being asserted
  on, and two homes let the fixture and the code read different configurations.

## Evidence

In sjawhar/legion#2642, four of its twelve forward merges changed behaviour with no conflict marker at
the broken site. One was main's new `internal/daemon` `TestMain` calling `fixtureenv.Guard`, which
strips every `LEGION_*` variable — and that binary is also the acceptance rig's role process,
re-entered by its launcher with `LEGION_IDLE_RIG_URL`, `LEGION_SANDBOX`, `LEGION_ISSUE`,
`LEGION_ROLE` and `LEGION_DAEMON_URL`. Guarded, the child named no rig, returned, printed `PASS`
and exited 0, and the daemon read all nine of its roles as processes that died at once
(`supervise: claim failed … launch failures ran out`), with the rig timing out on its first pod.

Two review rounds then refused the launcher package's own isolation: `launcher.go:496` merges a
start's environment over the process's and `bundleRestore.apply` reads `LEGION_WORKSPACE` from the
result, so a start naming none reached the pod's live workspace with only the guard in between. The
fix put the entry in `childStart`, so every start of the rig names a workspace under the test's own
`t.TempDir()`, and the package is green with `LEGION_WORKSPACE` pointed at a decoy.
`TestNoTestOfThisPackageCanReachThePodsWorkspace` carries three cases and says which of them hold
without a guard.
