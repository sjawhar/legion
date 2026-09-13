---
title: "Daemon-generated artifacts must write to XDG state dir, not tracked workspaces"
category: daemon
tags:
  - file-placement
  - xdg-state
  - legionDir
date: 2026-04-06
status: active
module: daemon
related_issues:
  - "sjawhar-legion-172"
  - "LEGION-74"
symptoms:
  - "a daemon-generated file shows up as untracked in jj status"
  - "a generated artifact lands under .legion/ in a tracked workspace"
---

# Daemon-Generated Artifacts Must Write to XDG State Dir

## Context

The daemon once carried a codebase index (`packages/daemon/src/index/`: a dependency-graph
scanner, a change-hotspot reader over `jj log`, and a `CodebaseIndexManager` that persisted the
result). It wrote its `index.json` to `{legionDir}/.legion/daemon/` — inside the user's tracked
repo — so the file appeared as untracked in `jj status` / `git status`, in a location no worker
workspace could reference (workers live under `~/.local/share/legion/workspaces/`). The module
was removed in LEGION-74: nothing in the daemon called it. The placement rule it violated stands
for everything the daemon generates.

## Rule: Tracked vs State Directory

| Content type | Location | Example |
|---|---|---|
| **Daemon-generated** (caches, runtime state) | `~/.local/state/legion/legions/{projectId}/` via `config.paths.forLegion(legionId).legionStateDir` | `workers.json` |
| **Worker-authored** (handoffs, plans, learnings) | `.legion/` in the workspace branch (tracked, intentional) | `architect.json`, `plan.json` |

### Code Smell

Any `if (config.legionDir)` branch that writes to `path.join(config.legionDir, ...)` for
daemon-generated files. The `legionDir` config field is the repo root for **reading** (scanning
source files). Writing generated artifacts there pollutes the tracked tree.

The `paths` module (`packages/daemon/src/daemon/paths.ts`) already provides the correct
locations for everything the daemon generates. Use it.

## Scanning a Tracked Tree

A daemon component that walks a repository must prune vendored and generated directories
(`node_modules`, `.git`, `.jj`, `.venv`/`venv`, `__pycache__`, `.legion`, `dist`, `build`, …) or it
produces megabytes of noise from dependencies. A hardcoded exclusion list is fine for a first
ecosystem or two; repeated false positives across projects mean the approach should become
`.gitignore`-aware or configurable rather than the list growing forever. When adding an exclusion,
include its obvious variants in the same change (`.venv` and `venv`).
