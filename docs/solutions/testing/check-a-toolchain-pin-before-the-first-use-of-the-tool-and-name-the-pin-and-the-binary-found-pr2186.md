---
title: "Check a toolchain pin before the first use of the tool, and name the pin and the binary found"
category: testing
tags:
  - bun
  - toolchain-pin
  - live-proof
  - prerequisites
date: 2026-10-29
status: active
module: scripts/e2e
related_issues:
  - "sjawhar/legion#2186"
---

# Check a toolchain pin before the first use of the tool, and name the pin and the binary found

- When a run builds something whose output depends on the exact tool build (a bundler pinned by
  a version file), compare the tool's version with the pin before the run's first invocation of
  that tool, reading the pin from the same file the build reads, never a second copy.
- Refuse with the pin, the file that holds it, and the path of the binary actually found, so the
  operator fixes their `PATH` instead of reading a failure at the build line.

## Evidence

`scripts/e2e/claude-backend-tmux.sh` (the `bun_pin` check before `read_app_logins`). After the
plugin build moved into the proof, its first run on a devbox whose version manager defaulted to
another Bun failed in the rig at `build.ts --out`, which refuses any Bun but the pinned one. The
check (commit bb11c27a) refuses at once with the pin's own message.
