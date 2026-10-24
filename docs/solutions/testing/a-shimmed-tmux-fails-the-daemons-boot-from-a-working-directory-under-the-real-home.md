---
title: "A shimmed tmux fails the daemon's boot from a working directory under the operator's real HOME"
category: testing
tags:
  - tmux
  - mise
  - e2e
  - boot
date: 2026-10-24
status: active
module: packages/daemon/internal/runtime/tmux, scripts/e2e/lib/rig.sh, scripts/e2e/stage1-skeleton.sh
symptoms:
  - "legion start: build the tmux runtime: scrub the private tmux server's environment: tmux show-environment -s -g failed (exit 1): mise ERROR … config.toml are not trusted"
  - "the same run passes from a checkout outside HOME"
---

# A shimmed tmux fails the daemon's boot from a working directory under the real HOME

## Problem

On a devbox, the first `tmux` on `PATH` is a dotfiles wrapper or a mise shim, not a binary. Started
from a directory under the operator's real HOME, the daemon's boot refused:

```
legion start: build the tmux runtime: scrub the private tmux server's environment:
tmux show-environment -s -g failed (exit 1): mise ERROR … config.toml are not trusted
```

The same run from a checkout outside HOME passed, which hid the cause. Earlier comments in the
repository blamed "mise's global config untrusted" and named neither condition.

## Root cause

The daemon runs tmux with its pane environment, which moves `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`,
`XDG_DATA_HOME` and `XDG_STATE_HOME` under the state directory and drops
`MISE_TRUSTED_CONFIG_PATHS` (`packages/daemon/internal/runtime/tmux/environment.go`). mise then no
longer reads `~/.config/mise/config.toml` as its global config, and keeps no trust record for it.
mise runs from the daemon's working directory and walks up its parent directories. Under the real
HOME, that walk finds the file as a parent directory's config with no trust record, so the shim
exits 1 before tmux runs. The boot reads that exit as a failure, not as "no server yet". A live
proof's scratch HOME has the same effect, but the failure does not need one: a daemon with the
operator's real HOME, started under it, fails the same way. Outside HOME, the walk finds no such
file.

## Solution

Give the daemon a real tmux binary first on its `PATH`. The live proofs do this with `real_bin_path`
in `scripts/e2e/lib/rig.sh`, and Stage 1 (`scripts/e2e/stage1-skeleton.sh`) now does it whenever its
first tmux is a script or mise itself. An operator does the same (`docs/deployment.md`, "What a
pane's `gh`, `git` and `jj` are"); `mise which tmux` prints the install path.

## Prevention

A proof that passes outside HOME and fails under it points at a config walk over parent directories.
Run a daemon's proofs from a checkout under the real HOME, and give the daemon real binaries, never
shims that read HOME or XDG config.
