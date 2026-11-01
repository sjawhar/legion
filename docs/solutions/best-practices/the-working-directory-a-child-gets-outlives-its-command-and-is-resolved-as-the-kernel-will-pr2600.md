---
title: "The working directory you give a child outlives its command and is read as the kernel will read it"
category: best-practices
tags:
  - subprocess
  - working-directory
  - symlink
  - guard
  - jj
date: 2026-10-31
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2600"
---

# The working directory you give a child outlives its command and is read as the kernel will read it

A child process started with no working directory of its own inherits the one the starting process
happens to have.
Every rule below is about the directory you name instead.

- **Pick a directory that exists before the command starts, that the command does not create, and
  that no retry removes.**
  For a command that creates `X`, that directory is `filepath.Dir(X)`, not `X`.

- **A relative directory is the same defect as an empty one**, because it resolves against the very
  process directory you are trying to stop inheriting.
  Refuse both with one refusal.

- **Resolve the path physically before any guard walks it.**
  A child that has `chdir`ed into a path holding a symlink asks the kernel where it stands, and the
  kernel answers with the link's target, so a lexical walk with `filepath.Dir` passes over
  everything above that target.
  `filepath.EvalSymlinks` alone will not do when the caller names a directory the command is about
  to create: resolve the longest existing prefix and rejoin the components that are not there yet.

- **Grep your own package for that helper before you write it.**

- **Before you start filling a field that has always been empty, find every reader of it**, and
  order the change so no reader is armed by surprise.

- **A refusal that names a resolved path names the configured one too**, so an operator reading a
  symlinked directory sees the path it wrote and the path the child stands in as one fact.

## Evidence (sjawhar/legion#2600, INFRA-623)

- The choice of directory, each with its reason in the code: `fetch.go`'s `feedTransfer.dir` is
  never `destination`, because `transferWithRetries` removes that between attempts and a child
  whose working directory has been removed cannot start; `clone.go`'s `cloneSteps.dir` is
  `filepath.Dir(cloneDir)`, the directory `os.MkdirAll` made, which is there before the first step
  and outlives them all; `unpackCache`'s `git init --bare <staging>` runs in
  `filepath.Dir(staging)`, since what a command creates is no working directory of its own.

- `config.go`'s `execRunner.Run` refuses an empty and a relative `Dir` with one
  `!filepath.IsAbs(command.Dir)`. The plan's design refused only an empty one; the two were merged
  because they are one rule. `TestTheRunnerRefusesACommandWithNoAbsoluteWorkingDirectory` has the
  stub write a marker file, so a refusal is proven to have happened *before* exec rather than
  after a child ran and failed.

- The physical read was a second round's fix. The first `refuseEnclosingWorkspace` walked
  `filepath.Dir` lexically, and the tester's red test (`enclosingsymlink_test.go`, `b932555c3cdf`)
  showed a state directory reached through a symlink defeating it: the binary at that head exited 0
  and wrote `config-id` into a decoy repository. The fix calls `resolvedPath`.

- `resolvedPath` already existed in the same package, `gitworktree.go`, doing exactly this. The
  implementer wrote a second copy, `resolveExisting`, and a whole review round was spent deleting
  it.

- The ordering: giving the two rootless jj commands a `Dir` would have armed
  `guardWorkspace` → `refuseSymlinkedLayout` for them for the first time, and that walk would have
  `Lstat`ed the state directory itself — the one directory the code deliberately leaves unchecked,
  because an operator may reach it through a symlink. So `jjWorkspaceRoot` had to stop reading
  `Command.Dir` *first*. The plan named that order ("task 1 orders D2 before D1") after a gap
  analysis found it.
