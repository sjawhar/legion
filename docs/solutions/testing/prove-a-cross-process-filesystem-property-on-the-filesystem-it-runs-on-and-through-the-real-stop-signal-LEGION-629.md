---
title: "Prove a cross-process filesystem property on the filesystem it runs on (the tree volume reuses inodes; /tmp does not) and through the real stop signal; pin an identity with a hard link, drive an interleaving through a seam, and run the pre-fix file as the control"
category: testing
tags:
  - cross-process
  - filesystem
  - v9fs
  - overlayfs
  - inode-reuse
  - tempdir
  - hard-link
  - process-group
  - sigterm
  - go-test-overlay
  - mutation-testing
date: 2026-10-08
status: active
module: packages/daemon/internal/workspace/codegraph_test.go, packages/daemon/internal/shim/shim_test.go
applies_when:
  - A test guards a file-identity, rename, or lock property that production runs on a shared volume (the tree volume) while the test's TempDir is on /tmp
  - A property holds "when the owner is stopped" and the owner's stop is one SIGTERM to a process group
  - A two-contender interleaving is a microsecond window that live runs rarely land in
  - A regression test for a race or identity bug needs a negative control that a reviewer can re-run
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
---

# Prove a cross-process filesystem property on the filesystem it runs on, and through the real stop signal

Extends `docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md`:
the dimension to widen is not only the contender count but the filesystem and the signal the
production path runs under. A pod's worker image has `/tmp` on overlayfs and the issue workspace
on the tree volume, v9fs over the issue's volume; a `t.TempDir()` test runs on the first and the
code under test runs on the second.

- **Run a file-identity test on the filesystem the guard runs on.** `TMPDIR=<a directory on
  /legion/workspaces> go test …` puts `t.TempDir()` on the tree volume. Measure the difference
  once and record it: `touch a; stat; rm a; touch a; stat` gives the same inode back on v9fs
  (`95 → 95`, `81209 → 81209`) and a new one on overlayfs (`50962 → 50963`). An `os.SameFile` guard
  that passed on `/tmp` failed 3/3 on the tree volume.
- **Or pin the identity in the fixture so the test is filesystem-independent**: build the
  successor file with a hard link to the original (`os.Link` old → kept, remove, link kept → path,
  remove kept), so "the new file has the old inode" holds everywhere, then give it a fresh mtime.
  The tester's `TestATakeoverLeavesAFreshLeaseThatReusedTheStaleOnesInode` is the shape.
- **Drive an interleaving deterministically through a seam**, a package-level
  `func(step string)` hook the production code calls with the name of the step it is about to take
  (`warmLeaseStep("judged stale")`, `("marker held")`); the test runs the second contender inside
  the hook and clears the hook before doing so, since the second contender reaches the same steps.
  Live runs of two real shims over a stale lease landed the microsecond window in zero of nine;
  the seam lands it every time.
- **Prove a "when the owner stops" property through the owner's real stop.** A unit test of the
  goroutine passed while the real shim, started under `setsid` and sent `kill -TERM -- -<pgid>`
  (what the launcher does to a role's generation), lost the goroutine with the process. The drive:
  the branch's `legion worker-shim` with the flags `launcherCommand` passes, wrapping the real
  Oh My Pi (or a stand-in that prints a ready frame and `trap '' TERM`, the worst case), dialled to
  a throwaway daemon that acks the hello; read the pgid from `/proc/<pid>/stat` (the worker image
  has no `ps`), signal the group N seconds after the ready frame, time the shim's exit, and check
  the lease, `/proc` for leftover children, and `codegraph status --json`.
- **Run the pre-fix file as the control, not a weakened assertion.**
  `go test -overlay '{"Replace":{"<pkg>/file.go":"<old file from git show>"}}' -run <test>` runs
  the new test against the code it is meant to catch; the failure text in the handoff is the
  evidence that the test can fail (`A holds = true, B holds = true`; `B took the lease over although
  A, acting first, holds a fresh lease`).
- **An invariant assertion that recomputes the production formula is vacuous.** Assert
  hand-picked expected values for concrete inputs and flip the formula once to see the test fail
  (`docs/solutions/testing/mutation-proof-probe-tests.md`); the drain-bound test's
  `grace + drain + f(...) ≤ max(stopGrace, grace + drain)` could not fail for any input.
- **A measurement of "the default" must unset the pin being removed.** The plan measured
  `dev.autoqa = false` as Oh My Pi's default from inside a pod that still exported `PI_AUTO_QA=0`,
  the very variable the change removes; the default is `true`. Measure in `env -u <pin>` or
  `env -i`, and say which.

## Evidence

- LEGION-629, rounds 1–4 (PR sjawhar/legion#1848): the implementer's `TestTwoContenders…`
  interleaving test passed on `/tmp` and failed 3/3 with `TMPDIR` on the tree volume (tester,
  round 3); the tester's hard-link test fails on both filesystems against the pre-fix file and
  passes on both against the fix; the round-1 group-SIGTERM drive left the lease behind with no
  log line while the goroutine's unit test passed; six live two-shim runs on v9fs reused the
  inode every time and produced one builder each after the fix.
- The worker image has no `ps`, `pgrep`, `jq` or `python3`; `/proc/<pid>/stat`'s fifth field is the
  pgid, `/proc/*/cmdline` the process list, and `bun` the scripting runtime
  (`docs/solutions/testing/a-worker-pod-ships-no-go-or-tmux-provision-them-under-home-each-relaunch-and-record-every-figure-in-the-handoff.md`
  for Go; the pod is re-incarnated between rounds, so every drive script is rewritten).

## Related

- `docs/solutions/daemon/a-file-presence-lease-on-a-shared-volume-exclusive-create-is-the-only-atomic-step-judge-a-lease-by-mtime-and-token-never-inode-LEGION-629.md`
- `docs/solutions/daemon/a-background-task-the-shim-owns-ends-on-the-stop-signal-and-is-waited-for-inside-the-pods-real-stop-grace-LEGION-629.md`
- `docs/solutions/testing/two-contenders-through-a-rename-collision-gate-both-in-wake-on-the-rename-pin-the-winner.md`
  — gating two in-process contenders into a race; the seam above is the cross-process cousin.
