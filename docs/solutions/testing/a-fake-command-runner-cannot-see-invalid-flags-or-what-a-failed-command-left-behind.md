---
title: "A fake command runner cannot see an invalid flag or what a failed command left behind: pin argv with the fake, pin behaviour with both real binaries"
category: testing
tags:
  - fake-command-runner
  - real-binary-tests
  - jj
  - dual-binary
  - mise
  - negative-control
  - argv-pins
  - bun-test
  - ci-identity
date: 2026-09-13
status: active
module: packages/workspace
related_issues:
  - "sjawhar/legion#1023"
---

# A fake command runner cannot see an invalid flag or what a failed command left behind: pin argv with the fake, pin behaviour with both real binaries

`packages/workspace/src/workspace.test.ts` drives `provisionIssueWorkspace` two ways: a fake
`run` that records argv and returns scripted results, and `realJjRig`, which runs the real
command against two real jj binaries. #1023 showed exactly what each can and cannot prove.

## The two blind spots

1. **An invalid flag.** The recovery branch ran `jj workspace forget <name> --cleanup --force`.
   Every fake-runner test that reached it passed, because the fake returns `exitCode: 0` for any
   argv it is handed. On both real binaries the command exits 2 with
   `error: unexpected argument '--cleanup' found`; the branch had never once completed since it
   was written. The fake pins *what we asked for*; only the binary knows whether that is a
   command.
2. **What a failed command left behind.** `jj workspace add --revision <name>` for a name jj
   cannot resolve registers the workspace and creates its directory *before* exiting 1. A fake
   returning `{ exitCode: 1, stderr: "Error: Revision … doesn't exist" }` models the exit, not the
   side effect, so a test built on it cannot notice that the next call would find the directory
   and adopt it. The real-jj conflicted-bookmark test asserts what the fake cannot: after the
   refusal the directory does not exist and `jj workspace list` has no entry — on two consecutive
   attempts.

## The split that works

- **Fake runner: argv where argv is the observable.** Exact command lists, in order, including
  `cwd` (`bookmark set … -r @` must run in the *new* workspace) and — for a refusal — that
  *nothing follows*: `expect(calls.map((c) => c.cmd)).toEqual([fetch, resolution])`. The spec's
  acceptance lines are written as argv facts ("records no command line containing `bookmark`"),
  so these tests are the contract, not plumbing. The daemon's `processes.test.ts` carries the same
  fresh-path pin from the other side; an argv change is a population sweep across both files.
- **Real binaries: behaviour.** Commit positions (`commitOf(bookmark)` unchanged; the re-added
  `@`'s parent equals the bookmark's commit), op-log counts (`point bookmark legion/X` operations
  still 1), `jj bookmark list` empty after a merged branch's deletion, a directory absent and a
  registration absent after a refusal. These are what the user of the package observes.

## The rig

`realJjRig(command, stateDir)` builds a colocated scratch remote, a colocated clone at the
daemon's repo path with `main` set and `origin` added, and returns `{ repoCloneDir, workspaceDir,
remoteDir, calls, jj, commitOf, deps }`. Every test iterates
`JJ_BINARIES = [{ name: "local Sami JJ", command: ["jj"] }, { name: "stock JJ 0.44", command: STOCK_JJ }]`
where `STOCK_JJ = ["mise", "x", "github:jj-vcs/jj@0.44.0", "--", "jj"]` — the CI runner's version,
which the `test` job already installs. Four details that mattered:

- `jj()` asserts exit 0 with `${command} ${args}\n${stderr}` as the assertion message, so a
  failing setup step names itself; every `expect` in the loop body carries `name` so a failure
  says which binary.
- `deps.run` substitutes the binary for `cmd[0] === "jj"` and records `cmd` into `calls`, so the
  same test can pin argv *and* behaviour.
- **Identity.** `withIdentity(opts)` adds `JJ_USER`/`JJ_EMAIL` to every jj invocation, the rig's
  own and provisioning's. Without it the suite is green on a developer box (identity from
  `~/.config/jj`) and red on the runner: `jj git push` refuses
  `Won't push commit … since it has no author and/or committer set`. The first CI run of this
  package failed on exactly that. Reproduce the runner before pushing: run the suite with
  `JJ_CONFIG` pointing at an empty file and `XDG_CONFIG_HOME` at an empty directory.
- **Snapshot before you push or move the bookmark from outside.** `jj status` in the workspace
  right after the first provision starts from a clean working copy. If later uncommitted content
  accumulates in `@`, the next snapshot rewrites `@` and moves the bookmark with it, so a push
  before that snapshot leaves the local bookmark off the pushed commit (and the fetch then keeps
  it), and an external `bookmark set` before it makes the next `update-stale`'s divergent snapshot
  conflict the bookmark. The comment in the test says why the line is there; keep it.
- 60 s per test (`}, 60_000)`): two binaries, one via mise, ~30 subprocesses each.

## Choose the command so the rejection branch is real

The spec first named `present(legion/<KEY>)` for resolving the bookmark. Verified on both
binaries, it exits 1 on a conflicted bookmark instead of listing two commits — so the code's
"more than one commit → refuse" branch would have been reachable only from the fake runner and
never from a real repository. The implementation uses `bookmarks(exact:legion/<KEY>)`, which
lists one commit id per target, and the real-jj conflicted test constructs an actual conflict
(two `bookmark set`s from one operation, one `--at-op`; the next command reconciles) and asserts
the refusal names both ids. When a fake-runner test scripts an output shape, prove a real binary
produces it; otherwise the branch it defends is fiction.

## The negative control is the old code, not a commented-out assertion

Both rounds proved the new real-jj tests defend the contract by putting the old code back and
watching them fail — `jj file show -r <old head> packages/workspace/src/workspace.ts > …/workspace.ts`,
run the tests, restore, `cmp` against the saved copy. Round 1: the three "leaves the bookmark
alone" tests all fail with the unconditional `bookmark set --allow-backwards` restored (on
`commitOf(bookmark)`, and the merged-branch test on the stale working copy). Round 2: the two new
tests fail with round 1's `createWorkspace` (the surviving-bookmark test on `Refusing to move
bookmark backwards or sideways`, the conflicted test because the old code registered the
workspace). The reviewer repeated the round-1 check independently and additionally deleted the
argv assertions to confirm the behaviour assertions alone still fail. Record the negative control
in the PR body; it is the evidence that the test would catch the regression.

## Running "with PATH `jj` = 0.44" on this box

`/home/ubuntu/.dotfiles/bin/jj` is first on PATH and shadows mise's shims, so
`PATH="$(mise where …)/bin:$PATH"` does not change which `jj` runs. Symlink the real install
(`/home/ubuntu/.mise/installs/github-jj-vcs-jj/0.44.0/jj`) into a scratch directory, put that first,
and **verify with `command -v jj && jj --version`** before believing a run. The suite itself does
not depend on PATH for the second binary — `STOCK_JJ` goes through `mise x` — so this matters
only for "PATH jj = 0.44" gate runs and for the `configures the backing Git repository for a stock
JJ workspace` test's expectations about the ambient `jj`.
