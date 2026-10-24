---
title: "A shimmed tmux under a scratch HOME fails the daemon's boot from a checkout under the operator's real HOME"
category: testing
tags:
  - tmux
  - mise
  - e2e
  - scratch-home
  - boot
date: 2026-10-24
status: active
module: scripts/e2e/lib/rig.sh, scripts/e2e/stage1-skeleton.sh
symptoms:
  - "legion start: build the tmux runtime: scrub the private tmux server's environment: tmux show-environment -s -g failed (exit 1): mise ERROR … config.toml are not trusted"
  - "the same run passes from a checkout outside HOME"
---

# A shimmed tmux under a scratch HOME fails the daemon's boot

## Problem

On a devbox, the first `tmux` on `PATH` is a dotfiles wrapper or a mise shim, not a binary. A
live proof runs the Legion daemon with a scratch HOME (Stage 1's `pane_home`, a pane's
`make_omp_home`). From a checkout under the operator's real HOME, the daemon's boot refused:

```
legion start: build the tmux runtime: scrub the private tmux server's environment:
tmux show-environment -s -g failed (exit 1): mise ERROR … config.toml are not trusted
```

The same checkout outside HOME passed, which hid the cause. Earlier comments in the repository
blamed "mise's global config untrusted" and did not name the trigger.

## Root cause

mise runs from the daemon's working directory and walks up its parent directories. Under the real
HOME, that walk finds the real HOME's `~/.config/mise/config.toml` as a parent directory's config.
mise does not trust that file under the scratch HOME, so the shim exits 1 before tmux runs. The
boot scrubs its private tmux server's environment with `tmux show-environment -s -g` and reads
that exit as a failure, not as "no server yet". Outside HOME, the walk finds no such file.

## Solution

When the first tmux on `PATH` is a script or mise itself, put the real binary that `mise which
tmux` names first on the daemon's `PATH`: `real_bin_path` in `scripts/e2e/lib/rig.sh`, used by
`scripts/e2e/stage1-skeleton.sh`. Resolve it in the daemon's working directory, and refuse the run
when mise names no tmux. Leave a real binary (CI's apt tmux) alone.

## Prevention

A proof that passes outside HOME and fails under it points at a config walk over parent
directories. Test a scratch-HOME rig from a checkout under the real HOME, and give the daemon real
binaries, never shims that read HOME.
