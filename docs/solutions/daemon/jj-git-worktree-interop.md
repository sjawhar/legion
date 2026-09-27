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
names `<workspace>/.git`, and a `.git` file in the workspace pointing back at the entry. jj asks git
for relative worktree paths, which git 2.48 and later write: each pointer is then relative, computed
between the real paths of the two directories, so it is resolved from the real path of the
directory holding it. jj 0.44 creates no git worktree.

## What goes stale, and when

- The fork's `jj workspace forget` removes the workspace's git worktree only while its directory
  exists. Forgotten after the directory went, the entry stays, and the next `jj workspace add` at
  that path stops at git's `is a missing but already registered worktree`.
- A workspace jj still registers whose directory is gone makes `jj workspace add` fail with
  `Workspace named '<name>' already exists` before it creates any git worktree.

Both daemons (`createWorkspace` and `Remove`/`removeIssueWorkspace`) therefore delete the
workspace's own entry when its directory is gone: provisioning before the add (it adds only when it
finds no directory), removal after the forget (it deletes the directory first), and removal again
when a crash after the forget left the workspace neither registered nor present.

## Never a bare `git worktree prune`

A bare prune deletes every entry whose `gitdir` target the pruning process cannot see. Sessions on
one host that each mount only their own checkout (containers per session) see none of each other's
workspaces, so a prune from one deletes every other workspace's entry, and stock jj 0.45.1's
`jj workspace forget` runs such a prune on the whole clone. git then fails in those workspaces while
jj keeps working. On the Kubernetes runtime the clone and a tree's workspaces share the tree's one
volume (docs/kubernetes.md), so every pod of the tree sees them all.

The removal reads each entry's `gitdir` and deletes only the entries naming this workspace's
`.git`, the path as given or with its symlinks resolved (jj records the resolved path). git names
an entry after the directory's base name, with a number on a collision, so the id is read, never
derived. That is what `git worktree prune` does to that one entry, and it works whether or not the
entry is locked. `git worktree remove --force --force <dir>` would do the same through git, but it
exits 128 when git has no entry for the path, which would need stderr matching, and against a
directory that unexpectedly exists it deletes the directory with whatever it holds; deleting the
admin entry never touches a working tree.

Every provisioning then locks the workspace's entry, whether it added the workspace or found it
already there, so a workspace added before provisioning locked anything is locked the next time
Legion provisions it (`<entry>/locked`, what `git worktree lock` writes; an existing lock and its
reason are kept). A bare prune anyone else runs on the clone then skips it.

## Restoring an entry a prune already deleted

`git worktree repair` cannot rebuild a missing entry: from the clone it answers `.git file does not
reference a repository`, and inside the workspace git fails before it starts. So provisioning an
existing workspace whose `.git` names an entry that is gone re-creates it at that path, as `git
worktree add` would: `gitdir`, `commondir`, and `HEAD` at the working-copy commit's first parent
(where jj keeps a colocated workspace's HEAD), then the index from HEAD with `git read-tree`, which
writes no working-tree file: uncommitted files show as untracked or modified, and jj is untouched.
A pointer outside the clone's `.git/worktrees` is refused by name.
