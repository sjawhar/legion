---
title: One jj actor per shared workspace when parallel agents edit files
category: delegation
tags:
  - jj
  - workspaces
  - subagents
  - parallel-edits
  - divergent-change
date: 2026-09-09
status: active
module: legion
problem_type: workflow_issue
component: tooling
severity: high
applies_when:
  - Two or more agents edit files concurrently inside one jj workspace
  - Another session works the same jj repo (other workspaces, bookmark pushes, rebases) at the same time
  - A worker sees "The working copy is stale" and is tempted to run `jj workspace update-stale`
---

# One jj Actor per Shared Workspace When Parallel Agents Edit Files

## Context

A coordinator ran four read-write analysis agents in one jj workspace (`dn-an`)
while a second session rebased and pushed bookmarks in the same repository from
its own workspaces. jj snapshots the working copy at every operation; with
concurrent operations from two sessions it recorded two snapshots of the same
working-copy change and marked the change **divergent**. When one agent then
obeyed jj's "working copy is stale" hint and ran `jj workspace update-stale`,
jj reset the tree to one of the two snapshots and its siblings lost in-flight
hunks — some files kept a subset of edits, which looked like a targeted revert.

Nothing was lost: both snapshots stayed in the repo as two commits with the
same change id (`jj log -r 'change_id(<id>)'`). Squashing the stale copy into
the live one (`jj squash --from <stale-commit> --into @ -u`) recovered every
hunk; the only real conflict was one agent's own file captured at two moments.

## Guidance

- **The coordinator is the only jj actor in a shared workspace.** Agents that
  edit files run no jj write command at all: no `describe`, `new`, `restore`,
  `abandon`, `undo`, `squash`, `rebase`, and above all no
  `jj workspace update-stale`. They report "working copy is stale" to the
  coordinator and keep editing files; the coordinator repairs and commits.
- Say so in every dispatch prompt for parallel editors in one tree, alongside
  the "claim a file before editing" rule. The default worker instinct is to
  follow jj's hint, and the hint is wrong for this situation.
- Prefer one workspace per writing agent when their file sets are disjoint
  (`jj workspace add`), and linearize afterwards. Share a workspace only when
  agents must see each other's edits live (a quality pass over one diff).
- When a change does go divergent: list both commits with
  `jj log -r 'change_id(<id>)'`, diff each against the base to see which hunks
  each holds, squash the stale one into `@`, then resolve any conflict by
  reading both sides (they are usually the same edit at two moments — take the
  later one).

## Why This Matters

Agents cannot tell a divergence-induced reset from a sibling reverting their
work, so they re-apply edits and fight each other, and a second agent running
`jj workspace update-stale` while the first re-applies makes the tree diverge
again. Fifteen minutes of four agents' work were at risk; the repair took the
coordinator ten minutes and required reading jj's op log to prove no one had
run `restore`.

## When to Apply

Any `task()` batch whose agents edit files in the same jj workspace, and any
coordinator working a repo that another live session also works. It does not
apply to read-only agents, or to agents in their own workspace with no other
session touching the repo.

## Examples

Prompt line that prevents it:

```
Run no jj write command at all (no describe/new/restore/abandon/undo/squash,
and never `jj workspace update-stale`) — if jj says the working copy is stale,
tell Main and keep editing; Main owns the working copy and commits.
```

Recovery:

```bash
jj log -r 'change_id(qxkwsuql)' -T 'commit_id.short() ++ " " ++ committer.timestamp().ago() ++ "\n"'
jj diff --from <base> --to <stale-commit> --stat    # what only the stale copy holds
jj squash --from <stale-commit> --into @ -u         # merge it; conflicts are rare and local
```

## Related

- `docs/solutions/delegation/subagent-plan-compliance.md` — dispatch prompts
  carry the constraints workers will not infer.
