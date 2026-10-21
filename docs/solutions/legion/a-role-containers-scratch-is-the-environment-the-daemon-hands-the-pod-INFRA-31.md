---
title: "A role container's scratch is the environment the daemon hands the pod, and a hand-made one is forgotten by the next turn"
category: legion
tags:
  - ephemeral-storage
  - issue-pod
  - eviction
  - agent-sandbox
  - scratch
date: 2026-10-21
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# A role container's scratch is the environment the daemon hands the pod, and a hand-made one is forgotten by the next turn

Extends docs/solutions/legion/a-roles-scratch-on-the-container-filesystem-is-what-an-issue-pod-is-replaced-over-INFRA-43.md.

That document says to move a role's scratch to the issue volume before building anything. This is
the delta: which paths are charged, where the locations must come from, and what to do in a pod
whose image predates the fix.

- **Export the six variables before the first build, in every shell of the turn.** A role in a pod
  on an image older than daemon API contract 34 is told none of them, so a build fills the
  container filesystem and the kubelet evicts the whole pod — every role of the issue with it.
  (34 is the contract that carries this layout; 33 is another change's, so an image at 33 is one
  of the images that needs the export.) The directories the block below makes are yours, made by
  hand: the reclaim remedy in `docs/kubernetes.md` names only the paths the runtime renders, and
  the test that holds that remedy to the renderer cannot see these, so remove them yourself when
  the volume fills.

  ```sh
  # /legion/scratch/<role>/env.sh — source it in every command of the turn
  export TMPDIR=/legion/scratch/<role>/tmp XDG_CACHE_HOME=/legion/scratch/<role>/cache \
         XDG_DATA_HOME=/legion/scratch/<role>/data \
         GOCACHE=/legion/scratch/<role>/go/build GOMODCACHE=/legion/scratch/<role>/go/mod \
         BUN_INSTALL_CACHE_DIR=/legion/scratch/<role>/bun
  mkdir -p "$TMPDIR" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$GOCACHE" "$GOMODCACHE" "$BUN_INSTALL_CACHE_DIR"
  ```

- **`/tmp` is charged.** A role container mounts no `/tmp` volume — the in-memory one belongs to
  `workspace-fetch` alone — so `/tmp` is the container's root filesystem and costs exactly what
  `$HOME` costs. Moving a cache to `/tmp` moves nothing. What is free: `/legion` (the issue's
  volume), the sessions directory, `/home/legion/.config` and `/var/run/legion/*`.
- **Prefer the environment the daemon gives the pod over anything you set yourself.** A hand-made
  relocation is a symlink or an export that dies with the pod, so every replacement rediscovers
  the problem; the fix that persists is the runtime telling the container where to write
  (`roleEnvironment`, `manifest.go`). Where a role must do it by hand, treat the export as part of
  every command rather than a thing done once.
- **A derived path is a trap for the tools that bind unix sockets.** Moving `TMPDIR` off `/tmp`
  pushes `t.TempDir()`-based socket paths toward `sun_path`'s 107 bytes: a test that listens on
  `filepath.Join(t.TempDir(), "s")` spells its own name into the path and fails with
  `bind: invalid argument`. Root a test's socket at `/tmp` explicitly and leave everything else
  under `t.TempDir()`.
- **Nothing prunes the issue volume.** Reclaim is manual, and a plain `rm -rf` of a Go module
  cache cannot do it: the go command writes every extracted module directory `555`, so the delete
  exits 1 with a `Permission denied` line per file and — worse than failing — removes the
  re-downloadable `cache/download` while leaving the extracted source. `chmod -R u+w` first.

## Evidence

This issue's own pod, 2026-10-20 to 2026-10-21. Two replacements during `go build ./...` with the
Go caches on the container filesystem (08:16Z and 08:18:53Z); none in the eighteen hours after the
caches moved to `/legion/scratch/implementer`, through dozens of builds and full test runs.

The forgetting is the part worth keeping. At hour seventeen the tester of this very issue — the
role that had spent the day proving this fix — put 1.17 GB under `$HOME` against a 1 GiB limit, in
a detached shell that had not sourced the export. A sibling tree's implementer held 1.5 GiB of Go
module cache and 2.9 GiB of build cache under `$HOME`, 4.4 GiB against the same limit, and
relocated them with symlinks that did not survive the next pod. Neither was careless; both are why
the locations belong in the environment the daemon hands the pod.

Reclaim, measured on this issue's volume at 01:32Z on 2026-10-21: 17 GiB used of 20, 14 GiB of it
in per-role copies of the Go caches under an intermediate layout. Removing the copies this pod
owned took it to 11 GiB used in about a minute — after `chmod -R u+w`, because the first attempt
exited 1 and left the module cache behind. At one-module scale: 19 `Permission denied` lines, exit
1, 96 KB of 2.3 MB surviving; with the `chmod` first, exit 0 and nothing left.
