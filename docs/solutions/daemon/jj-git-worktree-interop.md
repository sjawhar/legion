---
title: "jj/git worktree interop: remove only this workspace's git worktree entry, never prune the shared clone"
category: daemon
tags:
  - jj-workspaces
  - git-worktree
  - workspace-management
date: 2026-09-27
status: active
module: workspace
related_issues:
  - "LEGION-211"
symptoms:
  - "jj workspace add fails with 'is a missing but already registered worktree'"
  - "git in a workspace fails with 'fatal: not a git repository: <clone>/.git/worktrees/<id>' while jj keeps working"
  - "gh in a workspace runs unauthenticated because it cannot infer the repository from git"
---

# jj/git worktree interop

Every Legion workspace is a jj workspace of one shared clone per repository
(`<state>/repos/github.com/<owner>/<repo>`). On a colocated clone, jj 0.45's `jj workspace add`
also registers a git worktree: an admin entry `<clone>/.git/worktrees/<id>` whose `gitdir` file
names `<workspace>/.git`, and a `.git` file in the workspace pointing back at the entry. jj 0.44
creates no git worktree.

## What goes stale, and when

- The fork's `jj workspace forget` removes the workspace's git worktree only while its directory
  exists. Forgotten after the directory went, the entry stays, and the next `jj workspace add` at
  that path stops at git's `is a missing but already registered worktree`.
- A workspace jj still registers whose directory is gone makes `jj workspace add` fail with
  `Workspace named '<name>' already exists` before it creates any git worktree.

Both daemons (`createWorkspace` and `Remove`/`removeIssueWorkspace` in
`packages/daemon-go/internal/workspace/bookmark.go` and `packages/workspace/src/workspace.ts`)
therefore delete the workspace's own entry when its directory is gone: provisioning before the add
(it adds only when it finds no directory), removal after the forget (it deletes the directory
first).

## Never a bare `git worktree prune`

A bare prune deletes every entry whose `gitdir` target the pruning process cannot see. On the
Kubernetes runtime each tree's workspace sits on its own volume, and on a host shared by isolated
sessions each sees only its own mounts, so one process's prune deletes every other workspace's
entry: git then fails in those workspaces while jj keeps working.

The removal reads each entry's `gitdir` and deletes only the entries naming this workspace's
`.git`, the path as given or with its symlinks resolved (jj records the resolved path). git names
an entry after the directory's base name, with a number on a collision, so the id is read, never
derived. That is what `git worktree prune` does to that one entry, and it works whether or not the
entry is locked. `git worktree remove --force --force <dir>` would do the same through git, but it
exits 128 when git has no entry for the path, which would need stderr matching, and against a
directory that unexpectedly exists it deletes the directory with whatever it holds; deleting the
admin entry never touches a working tree.

Each workspace provisioning adds is then locked (`<entry>/locked`, what `git worktree lock`
writes; an existing lock and its reason are kept), so a bare prune anyone else runs on the clone
skips it.
