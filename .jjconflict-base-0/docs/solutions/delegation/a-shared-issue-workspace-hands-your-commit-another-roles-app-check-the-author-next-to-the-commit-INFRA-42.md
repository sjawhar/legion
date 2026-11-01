---
title: "A shared issue workspace hands your commit another role's App: check the author next to the commit, not at the turn's start"
category: delegation
tags:
  - jj
  - workspaces
  - legion
  - github-apps
  - commit-authorship
date: 2026-10-20
status: active
module: legion
related_issues:
  - "INFRA-42"
  - "sjawhar/legion#2086"
---

# A shared issue workspace hands your commit another role's App: check the author next to the commit, not at the turn's start

Extends `docs/solutions/delegation/one-jj-actor-per-shared-workspace.md`, which covers what
concurrent edits in one workspace do to the *content* of a change. This is what they do to its
*authorship*, which Legion's two-App separation depends on: an implementer's commits must carry the
implement App and a planner's, tester's or reviewer's the review App.

- Every role of a Legion issue shares one jj workspace. The working-copy commit you find is
  whatever the last role's jj left there, and it carries **that role's App** as its author;
  editing files into it and `jj split`-ing produces a commit authored by the other App.
- `jj new` before the turn's **first edit**, not before its first commit:

  ```bash
  jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'empty'   # true -> jj new @- ; false -> jj new
  ```

- That is not enough on its own. Another role's jj **recreates the working-copy commit while you
  sit idle** — a reviewer or tester running `jj` in the same workspace between your turns is
  enough — so a working copy that was yours when you began carries another App when you come back
  to it. Read the author immediately before every commit:

  ```bash
  jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'author.email()'
  ```

- Before a push, `jj metaedit -r @ --update-author` is the whole repair. After a push: the identity
  check in `skill://legion-worker/references/default-branch-check.md`, the rewrite guard
  (`descendants(<commit>) ~ ::@` must be empty, so nothing of another tree is built on it),
  `jj metaedit -r <commit> --update-author`, then one `jj bookmark set … --allow-backwards` and a
  single push.

## Evidence (INFRA-42, sjawhar/legion#2086)

Twice in one issue. The first time the working-copy commit had been left by an earlier role and the
implementer edited into it: `d646564e8b4e` reached `origin` authored by the review App, was caught
by the post-push identity check, and was replaced by `9449cff1` through the guard, `metaedit` and
one `--allow-backwards` move. The second time the implementer had created its own commit, went idle
while the reviewer worked, and came back to a working copy the reviewer's jj had recreated — caught
before the commit this time, by reading `author.email()` next to it, and repaired with
`jj metaedit -r @ --update-author` with nothing pushed.
