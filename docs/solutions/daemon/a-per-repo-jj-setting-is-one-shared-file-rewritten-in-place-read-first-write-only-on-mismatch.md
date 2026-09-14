---
title: "A per-repo jj setting is one shared file rewritten in place: read first, write only on mismatch, and know that the file is under the user's config directory, not in the clone"
category: daemon
tags:
  - jj
  - jj-config
  - per-repo-config
  - config-id
  - shared-clone
  - workspaces
  - non-atomic-write
  - provisioning
  - git.abandon-unreachable-commits
date: 2026-09-14
status: active
module: packages/workspace
related_issues:
  - "LEGION-84"
  - "sjawhar/legion#1080"
  - "LEGION-44"
  - "LEGION-24"
---

# A per-repo jj setting is one shared file rewritten in place: read first, write only on mismatch

LEGION-84 made provisioning set `git.abandon-unreachable-commits = false` on the daemon's shared
clone so a fetch that deletes a merged branch's bookmark no longer abandons the branch's commits
under a live issue workspace. The setting itself is one line; what is reusable is where jj keeps
a `--repo` setting, who reads it, and why the write is guarded. Every fact below was verified on
jj 0.44.0 (CI) and 0.45.1 (the box) with `jj config get`, `jj config path --repo`, `jj op log`,
and `jj status --ignore-working-copy` before and after each command.

## Where `jj config set --repo` actually writes

Not inside the clone. `jj config path --repo -R <clone>` names
`$XDG_CONFIG_HOME/jj/repos/<config-id>/config.toml` — `~/.config/jj/repos/…` under the **user's**
config directory — where `<config-id>` is the content of `<clone>/.jj/repo/config-id`. jj says
why the first time it creates one: `Per-repo config is stored in the same directory as your user
config for security reasons.` Consequences:

- Every `jj workspace` of the clone reads the same file: a workspace's `.jj/repo` is a pointer at
  the clone's, so the config-id is the clone's. `jj config get <key>` prints the same value with
  `-R <clone>`, with `-R <workspace>`, and with cwd inside the workspace.
- A process under another `HOME`/`XDG_CONFIG_HOME` does **not** see it. The negative control is
  `env HOME=<literal fresh dir> XDG_CONFIG_HOME=<that dir>/.config jj config get <key> -R <clone>`,
  which prints jj's default plus `Warning: Per-repo config not found. Generating an empty one.`
  Legion panes see the daemon's setting only because `HOME` and `XDG_CONFIG_HOME` are on the pane
  allow-list and panes run as the daemon's user; a Kubernetes worker (LEGION-24) under its own home
  needs the same config directory or its own write.
- The file is *one* file for *every* feature that touches repo scope. LEGION-44 removes a
  repository-scoped `user.name`/`user.email` from it; LEGION-84 writes a `git.*` key into it. Two
  features, one file, one `jj config` command stream — which is also why their tests collided
  (see `docs/solutions/testing/pin-the-exact-command-list-never-exclude-a-subcommand.md`).

## Why the write is guarded

`jj config set --repo <key> <value>` rewrites that file **in place** — same inode, truncate then
write, not a rename. Every jj command on the box parses the file at start-up. A concurrent jj
process (another issue's provisioning fetch, a worker's own `jj status`) that opens the file inside
the write window reads it empty (every repo-scoped key at its default) or partial (a TOML parse
error for that one command). This is not theoretical: on 2026-09-13 at 19:30Z the production
clone's file was read holding exactly `[user]` with nothing under it; ninety seconds later it held
a full identity again.

So the rule for any repo-scoped setting provisioning owns:

```
read:  jj config get <key> -R <clone>                    # every provisioning
write: jj config set --repo <key> <value> -R <clone>     # only when the read's trimmed stdout != <value>
```

The read is free: on both binaries it records **no operation** (`jj op log` identical before and
after) and takes **no snapshot** (an unsnapshotted file in the clone's working copy stays
unsnapshotted, so no `--ignore-working-copy` is needed). `config set --repo` also records no
operation and snapshots nothing. With the guard, the write lands once per clone and the steady
state never writes; without it, every provisioning would reopen the race window for every other
jj process on the box, forever. Order the guard before the one command the setting governs — for
LEGION-84 that is clone → read → (write) → fetch — and let a failed read or write stop provisioning
before that command runs (`runChecked`; it counts as a launch failure like any other).

Two more details for the implementer:

- The value is parsed as TOML. `false` must be the bare boolean; a bare string with brackets
  (`legion-implementer[bot]`) is refused — quote such values (`'"…"'`).
- `jj config set --repo` merges into whatever the file already holds (`[user]` from an earlier
  writer survives beside `[git]`), so the read-first guard, not the write, is what keeps other
  features' keys from being rewritten needlessly.

## How to prove it

The real-binary test rig in `packages/workspace/src/workspace.test.ts` (`realJjRig`, both
`JJ_BINARIES`) is the regression lock; the accepted out-of-suite proof shape is a jj-only driver
with main's file as the negative control, see
`docs/solutions/testing/jj-only-driver-with-mains-file-as-negative-control-is-the-proof-shape-without-the-rig.md`.
The quotable evidence for "the fetch did nothing but the bookmark" is
`jj op log --op-diff --limit 1 -R <clone>`: with the setting, only `Changed local bookmarks` and
`Changed remote bookmarks`; without it, also `Changed commits` and `Changed working copy <ws>@`.
