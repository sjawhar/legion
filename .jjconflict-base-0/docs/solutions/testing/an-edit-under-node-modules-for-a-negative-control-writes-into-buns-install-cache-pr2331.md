---
title: "An edit under node_modules for a negative control writes into Bun's install cache"
category: testing
tags:
  - bun
  - negative-control
  - node-modules
  - hardlink
date: 2026-10-28
status: active
module: packages/envoy-client
related_issues:
  - "sjawhar/legion#2331"
---

# An edit under node_modules for a negative control writes into Bun's install cache

- **Bun links installed files to its cache.** It installs a package's files under
  `node_modules/.bun/<pkg>@<ver>/` as hardlinks to its install cache. For a patched package the
  cache entry is `<pkg>@<ver>@@@1_patch_hash=<hash>`. Any tool that writes an installed file in
  place also writes that cache entry. Then `bun install` hands the edited file back, because the
  cache entry is the edited one.
- **Copy the file before the control.** To revert a dependency for a negative control, copy the
  file aside first, or remove it and write a fresh copy (`rm` then `cp`), never an in-place edit.
- **Restore by deleting the installed copy and the cache entry.** After the control, delete the
  installed package directory and its cache entry, then run `bun install --frozen-lockfile`. Grep
  the installed file for the patched line and run the lock green again before you trust the
  restore.
- **Check the link count.** `stat -c %h <file>` above 1 means the file is linked to the cache.

## Evidence

- At c2db0543 on Bun 1.4.4, row C's control reverted `protocol.js` in
  `node_modules/.bun/nats@2.29.3/` with an in-place edit.
- The first `bun install` afterwards left the reverted line in place. The file's link count was 2,
  and its other link was in `nats@2.29.3@@@1_patch_hash=257c91f8b5726726`.
- Deleting that entry and reinstalling brought the patch back, and rows A, B and C passed.
- The implementer's handoff (`implement.json`) recorded this under `discoveredComplexity`.
