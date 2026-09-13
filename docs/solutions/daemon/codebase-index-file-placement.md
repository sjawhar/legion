---
title: "Daemon-generated artifacts must write to XDG state dir, not tracked workspaces"
category: daemon
tags:
  - file-placement
  - xdg-state
  - state_dir
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
result). It wrote its `index.json` to `<legionDir>/.legion/daemon/`, where `legionDir` was that
module's own config field for the repo root (removed with it) — inside the user's tracked repo, so
the file appeared as untracked in `jj status` / `git status`, in a location no worker workspace
could reference (workers live under `paths.workspacesDir`, `~/.local/share/legion/workspaces/`
by default). The module
was removed in LEGION-74: nothing in the daemon called it. The placement rule it violated stands
for everything the daemon generates.

## Rule: Tracked vs State Directory

| Content type | Location | Example |
|---|---|---|
| **Daemon-generated** (durable state, locks, secret files, per-pane sockets, launchers) | `config.stateDir` — `state_dir` in `legion.yaml` / `LEGION_STATE_DIR`, default `~/.legion/<project>` | `state.json`, `daemon.lock`, `secrets/`, `workers/<name>.sock`, `worker-bin/gh`, `bin/legion`, `deployment-instructions.md` |
| **Daemon-generated** (learning feedback, logs) | `resolveLegionPaths(env, home).forLegion(legionId).legionStateDir` — `$XDG_STATE_HOME/legion/legions/<projectId>/` (`paths.ts`) | `learning-feedback.jsonl` |
| **Worker-authored** (handoffs, plans, learnings) | `.legion/` in the workspace branch (tracked, intentional; deleted before merge) and `docs/solutions/` | `architect.json`, `plan.json` |

### Code Smell

Any daemon code that derives a *write* path from a repository checkout — a workspace under
`paths.workspacesDir`, a clone under `paths.reposDir`, or the daemon's own cwd. Those are inputs
the daemon reads and provisions for workers; writing a generated artifact into one pollutes a
tracked tree that a worker will `jj status`. Everything the daemon generates has a home in
`config.stateDir` or in `paths.ts`; use one of those.

## Scanning a Tracked Tree

A daemon component that walks a repository must prune vendored and generated directories
(`node_modules`, `.git`, `.jj`, `.venv`/`venv`, `__pycache__`, `.legion`, `dist`, `build`, …) or it
produces megabytes of noise from dependencies. A hardcoded exclusion list is fine for a first
ecosystem or two; repeated false positives across projects mean the approach should become
`.gitignore`-aware or configurable rather than the list growing forever. When adding an exclusion,
include its obvious variants in the same change (`.venv` and `venv`).
