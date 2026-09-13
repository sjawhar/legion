---
title: "A pane-lifetime file the daemon names and prunes but never writes: track it by role token from the live locator, hold it with the launch, and write it atomically from the other side"
category: daemon
tags:
  - daemon
  - secrets
  - pane-environment
  - locator
  - prune
  - atomic-write
  - legion-grant
  - worker-bin
date: 2026-09-13
status: active
module: daemon
related_issues:
  - "LEGION-54"
  - "sjawhar/legion#992"
  - "LEGION-6"
  - "sjawhar/legion#923"
---

# A pane-lifetime file the daemon names and prunes but never writes

`<state_dir>/secrets/<role token>` (the boot token) is written by the daemon immediately before the
pane launches and pruned when the pane's locator clears
([secret-file-pointer-precedence](../integration-patterns/secret-file-pointer-precedence.md)).
LEGION-54 added a second file with a different author: `<role token>-grant`, named on the pane as
`LEGION_GRANT_FILE` at spawn, **written by the pi-envoy extension** before each bash command, read
by `legion credential`/`legion gh`/`legion handoff complete`, and pruned by the daemon exactly like
the boot token. The daemon never writes it and cannot know when it first appears. That changes
three things about how the lifecycle is kept; they are the reusable part.

## 1. Track names from the live locator, not from the files on disk

The boot prune used to seed the steady-state set from the files it found and kept
(`pruneSecretFiles` returned `kept`). A file that does not exist yet at boot — a grant file the
extension writes later in the pane's life — would never enter that set and would survive its
pane. `ProcessManager.pruneSecretFiles()` now seeds `processSecretFiles` from every name a live
locator references, whether or not the file exists (an absent name is a no-op `rm --force` later),
and the return value is gone. The test that pins this writes the grant file *after* the boot prune
and asserts the next locator-clearing persist removes it
(`processes.test.ts` "reaps a secret file inherited from a previous daemon process…").

Rule: a name's membership in the tracked set is a function of state (`state.roles`, `state.trees`,
`state.controllerLocator`, launches in flight), never of `readdir`.

## 2. One function names every file a token owns

`processSecretNames(token)` in `secrets.ts` returns `[token, grantSecretName(token)]`.
`trackProcessSecrets` iterates it before every `runtime.spawn`; `liveSecretFiles` `flatMap`s it
over every live token, including the refcounted launch holds (`holdProcessSecret`), so a persist
during a launch never reaps a file the new pane is about to read or write. A third per-token file
lands in that one function and inherits tracking, holding, and both prunes. The tests reconstruct
`${token}-grant` by hand in several places; the reviewer named that as the fast-follow — new tests
should call `grantSecretName`.

## 3. The writer on the other side writes atomically, beside the target, and refuses bad pointers

`writeGrantFile` (`packages/pi-envoy/src/legion/grant-file.ts`) writes `<file>.<pid>.<uuid>` with
mode 0600, re-`chmod`s it (`writeFile`'s mode is umask-masked), and `rename`s it over the target;
on any failure it removes the temp best-effort and throws naming the path, and the hook blocks the
command rather than let it run under whatever the file held before. Two hooks in flight for one
session never collide on the temp name and both produce live grants. A blank `LEGION_GRANT_FILE`
is treated as unset (blocked before minting); a relative one is refused before writing, because
the temp would otherwise land in OMP's cwd — the issue workspace.

The daemon's own `installWorkerGhShim` uses the same temp-plus-rename shape for
`<state_dir>/worker-bin/gh`, which it rewrites at every startup while panes from before a
`legion restart` may be mid-`gh`. Two implementations exist now (`grant-file.ts`, `worker-bin.ts`);
`writeSecretFile` in `secrets.ts` still overwrites in place. A third caller should extract the
helper rather than add a third copy.

## 4. A PATH entry given to panes for life must be stripped where the daemon builds its own environment

The same change put `<state_dir>/worker-bin` first on every pane's PATH for the pane's life. `mise
env --json` keeps the inherited PATH head, so a daemon started from inside a Legion pane — how
daemons on this box are started — would resolve its own `gh` to the shim (every GitHub read then
fails `LEGION_GRANT_FILE is missing`) and hand every child pane a second entry. The fix is one
strip, `pathWithoutWorkerBin`, applied in `resolveDaemonEnvironment` (so `processPath` is clean by
construction and `credentialProcessEnvironment`'s prefix is the only one), in `legion gh` before it
spawns `gh` (so the real `gh` runs, never the shim re-entering `legion gh` under a scrubbed env),
and in the rig. Nothing asserts the invariant in prose any more; the tests build the tainted PATH
and check the launched panes.

This is the PATH instance of the rule already recorded for config keys in
[config-env-keys-that-panes-also-carry](config-env-keys-that-panes-also-carry.md): before adding
anything a daemon exports to panes, decide whether an inherited copy in the daemon's own process
is a leak to strip, a per-daemon value to refuse, or a file pointer every rig must set — and write
the strip at the boundary, not a workaround in an operator note.

## 5. Deployment is ordered when the two sides ship separately

The plugin (npm) and the daemon (this repository's checkout) ship separately, and neither side
carried an API-shape change, so nothing gates the skew. Both directions fail by name at the first
credentialed command: a new plugin on a pane an old daemon launched blocks every bash call
(`LEGION_GRANT_FILE is not set on this pane: the daemon that launched it predates this plugin`); an
old plugin on a pane the new daemon launched fails `legion` with `LEGION_GRANT_FILE names <path>,
which could not be read: ENOENT: … the installed plugin predates LEGION-54; install the released
plugin in the profile and relaunch the pane`. The operator installs the plugin, then restarts the
daemon, immediately, spawning nothing between; panes started earlier carry no `LEGION_GRANT_FILE`
and keep working through `LEGION_GRANT`. When you add a coupling like this without an API version
bump, write both failure messages so they name the skew and the remedy — that text is the only
deployment gate there is.
