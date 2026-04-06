---
title: "Daemon-generated artifacts must write to XDG state dir, not tracked workspaces"
category: daemon
tags:
  - file-placement
  - codebase-index
  - xdg-state
  - legionDir
  - scanner-exclusions
date: 2026-04-06
status: active
module: daemon
related_issues:
  - "sjawhar-legion-172"
symptoms:
  - "index.json shows up as untracked in jj status"
  - "3.2MB index file from scanning vendored JS in .venv"
  - ".legion/daemon/index.json polluting tracked workspace"
---

# Daemon-Generated Artifacts Must Write to XDG State Dir

## Context

The `CodebaseIndexManager` was writing its index to `{legionDir}/.legion/daemon/index.json` —
inside the user's tracked repo. This caused the file to appear as untracked in `jj status` /
`git status`, and was in a location no worker workspace could reference (workers live under
`~/.local/share/legion/workspaces/`).

## Rule: Tracked vs State Directory

| Content type | Location | Example |
|---|---|---|
| **Daemon-generated** (indexes, caches, runtime state) | `~/.local/state/legion/legions/{projectId}/` via `config.paths.forLegion(legionId).legionStateDir` | `index.json`, `workers.json` |
| **Worker-authored** (handoffs, plans, learnings) | `.legion/` in the workspace branch (tracked, intentional) | `architect.json`, `plan.json` |

### Code Smell

Any `if (config.legionDir)` branch that writes to `path.join(config.legionDir, ...)` for
daemon-generated files. The `legionDir` config field is the repo root for **reading** (scanning
source files). Writing generated artifacts there pollutes the tracked tree.

The `paths` module (`packages/daemon/src/daemon/paths.ts`) already provides the correct
locations for everything the daemon generates. Use it.

## Scanner Exclusion Lists

The dependency graph scanner (`packages/daemon/src/index/graph.ts`) uses a hardcoded `SKIP_DIRS`
set to prune the directory walk. When adding support for new ecosystems, check this list:

```typescript
const SKIP_DIRS = new Set([
  "node_modules", ".git", ".jj",
  ".venv", "venv", "__pycache__", ".legion",
  "dist", "build",
]);
```

### When to add vs when to change approach

- **One-off ecosystem addition** (e.g., Go's `vendor/`, Rust's `target/`) → add to `SKIP_DIRS`.
- **Repeated false positives across projects** → escalate to `.gitignore`-aware scanning or
  configurable exclusions. The hardcoded list will get brittle.

### Include obvious variants

When adding an exclusion, include common variants in the same commit (e.g., `.venv` + `venv`).
Don't make the reviewer ask for them.

## Stale Artifact Note

Changing the write location doesn't clean up pre-existing files. After this fix, users with
an old `.legion/daemon/index.json` in their workspace will still see it as untracked until
they manually `rm -rf .legion/daemon/` in the affected repo. No automated migration was added.
