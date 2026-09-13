---
title: "bunx from the repo root runs the wrong tsc and biome: an unpinned TypeScript that rejects the tsconfig, and biome@0.3.3 — an unrelated npm package whose check exits 0"
category: build-errors
tags:
  - bunx
  - biome
  - tsc
  - typescript
  - false-green
  - workspace
  - verification
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-46"
  - "sjawhar/legion#1016"
symptoms:
  - "tsconfig.json: error TS5102: Option 'baseUrl' has been removed"
  - "error TS5090: Non-relative paths are not allowed. Did you forget a leading './'?"
  - "bunx biome check <files> prints only a punycode DeprecationWarning and exits 0 while CI's biome fails on the same files"
  - "Resolving dependencies / Resolved, downloaded and extracted / Saved lockfile before a lint or typecheck"
---

# `bunx` from the repo root runs the wrong `tsc` and `biome`: an unpinned TypeScript that rejects the tsconfig, and `biome@0.3.3` — an unrelated npm package whose `check` exits 0

## What happened

Verifying LEGION-46 from the repository root with the two commands the root `AGENTS.md` lists
(`bunx tsc --noEmit`, `bunx biome check …`):

- `bunx tsc --noEmit -p packages/daemon` printed `Resolving dependencies … Saved lockfile`, then
  `TS5102: Option 'baseUrl' has been removed` and `TS5090: Non-relative paths are not allowed`.
  Neither is a defect in the code: `bunx` resolved `tsc` to the newest `typescript` on npm
  (7.0.2 that day), whose tsconfig rules differ from the package's pinned `typescript@^5.3.0`
  (5.9.3 installed). The real typecheck was clean.
- `bunx biome check <three files>` printed a `punycode` deprecation warning and exited 0. The
  package's own `@biomejs/biome` 2.4.11 then found two formatting errors in the same files. The
  root `bunx biome` had run **`biome@0.3.3`** — an unrelated legacy npm package named `biome`
  (`Usage: biome [options] [command]`, commander-style) — whose `check` subcommand does nothing.
  A green exit that verified nothing.

Root cause: `bunx <bin>` uses the package installed at the current directory when one provides
that bin, and otherwise fetches from npm by name. The repository root's `package.json` has no
`typescript` or `@biomejs/biome` dependency (they are `packages/daemon` devDependencies, and Bun's
workspace install left no `node_modules/.bin` at the root), so from the root both names resolve
to the registry: `tsc` through `bunx`'s well-known-bin alias to latest `typescript`, `biome` to
the squatting `biome` package. `Resolving dependencies … Saved lockfile` in the output is the
tell — that lockfile is `bunx`'s own cache, not the repository's `bun.lock`, so `jj status`
shows nothing.

## The rule

Run lint and typecheck **from `packages/daemon`**, which is what CI does
(`working-directory: packages/daemon` in `.github/workflows/pr-and-main.yaml`) and what the
package scripts encode (`bun run lint`, `bun run typecheck`). Equivalent from anywhere:

```sh
cd -- "$LEGION_WORKSPACE/packages/daemon" && node_modules/.bin/tsc --noEmit
cd -- "$LEGION_WORKSPACE" && packages/daemon/node_modules/.bin/biome check <files>
```

Before trusting either tool's verdict, confirm the version is the pinned one
(`node_modules/.bin/tsc --version` → 5.9.x; `node_modules/.bin/biome --version` → 2.4.x). Any
`Resolving dependencies` line before a lint or typecheck means the tool was just downloaded,
which means it is not the one CI runs.

## Why it matters for a handoff

A worker's verification claim ("tsc clean, biome clean") is what the tester and reviewer trust
before CI. The `tsc` failure mode is loud and merely confusing; the `biome` one is a silent
false-green, exactly the class of evidence the review loop cannot catch by reading a handoff.
When `bunx` and the package binary disagree, the package binary is right and the `bunx` result is
discarded — and the handoff should say which binary produced the claim.
