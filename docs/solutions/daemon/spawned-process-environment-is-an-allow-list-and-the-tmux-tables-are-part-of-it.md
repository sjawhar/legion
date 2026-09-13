---
title: "A spawned process's environment is an allow-list, the tmux server's two tables are part of the boundary, and only two named daemon children see the daemon's own environment"
category: daemon
tags:
  - environment
  - allow-list
  - tmux
  - secrets
  - process-boundary
  - update-environment
  - show-environment
  - PANE_ENV_ALLOW_LIST
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-74"
  - "sjawhar/legion#1007"
  - "LEGION-77"
  - "sjawhar/legion#1017"
symptoms:
  - "a pane, worker, or controller can read a secret the daemon was launched with (`tr '\\0' '\\n' < /proc/<pid>/environ | grep -c PRIVATE_KEY` is not 0)"
  - "a daemon restart with a narrower environment still hands new panes the old variables"
  - "panes opened after an operator attaches to the private tmux server carry the operator's SSH_AUTH_SOCK"
---

# A Spawned Process's Environment Is an Allow-List, and the tmux Server's Tables Are Part of It

## What went wrong

Until LEGION-74 the daemon built each pane's environment as *its own environment minus a short
list of secret names*. The list never named the two GitHub App private keys the launcher
supplies (`GH_AGENT_APP_PRIVATE_KEY_B64`, `GH_REVIEW_APP_PRIVATE_KEY_B64`), so every controller,
architect, and worker pane on the production box could read them from its own environment and
mint installation tokens as either App. A strip-list fails open: every variable nobody thought of
is inherited, and the list is only as complete as the last person's memory of what the launcher
exports. Production proved it — panes spawned before the mitigation showed `grep -c PRIVATE_KEY`
→ `2`.

## The boundary has three layers, not one

The pane's own `-e` pairs are the visible layer. Two more sit beneath them and both outlive the
daemon:

1. **The environment the daemon hands its children.** `resolveDaemonEnvironment`
   (`environment.ts`) now builds `paneEnv` from `PANE_ENV_ALLOW_LIST` — identity, locale, XDG
   and mise directories, the OMP profile, proxy and CA policy, `PATH` — lays the complete
   `mise env --json` over it, and drops any credential-shaped name (`isSecretLikeName`,
   case-insensitive: `*_SECRET`, `*_TOKEN`, `*_GRANT`, `*_KEY`, `*_PASSWORD`, `*_PASSWD`, `*_PAT`,
   `*_CREDENTIALS`, their `*_FILE` twins, anything containing `PRIVATE_KEY`) as a second line of
   defence. That object is the environment of *every* command the runner executes: jj, git, gh,
   tmux, both start-up probes.
2. **The tmux server's global table.** The private server (`tmux -L legion-<project>`) is forked
   by the daemon's first tmux command and keeps that fork-time environment as its global table
   for its whole life — across daemon restarts. A daemon restarted with a narrower environment
   would otherwise keep handing new panes whatever an earlier daemon left in the running server.
3. **The tmux session's table.** tmux's default `update-environment` copies an attaching
   client's `SSH_AUTH_SOCK`, `SSH_CONNECTION`, `SSH_ASKPASS`, `DISPLAY`, … into the session, and
   every pane opened afterwards inherits them. An operator's `tmux -L legion-<project> attach`
   is enough; the production session carried the operator's SSH agent socket.

Layer 1 is fixed by construction. Layers 2 and 3 are fixed at every boot, once the probes pass
and before the launch hold releases (`TmuxRuntime.scrubServerEnvironment`): read each table,
empty the session's `update-environment` *before* reading the session table (so an attach landing
in between cannot slip a copy in), unset every name that is not a key of `paneEnv` (`PWD` and
`SHLVL`, which tmux writes itself, exempt), re-list and verify. `openWindow` creates the session
and empties the option in one tmux invocation so no attach ever fills it. The mechanism, and why
its first two designs were wrong, is `scrub-tmux-environment-tables-by-parsing-show-environment-s.md`.

What a plain restart does **not** do: revoke what was already handed out. The running server
process and every pre-upgrade pane keep their fork-time environment, readable through
`/proc/<pid>/environ` by any same-uid process, until `kill-server` or retirement ends them — the
scrub edits tmux's tables, not a process. Only rotating the material revokes it.

## Which side of the boundary does a new child belong on?

Exactly two daemon children run under the daemon's own `process.env`, and both exist to read a
secret the launcher supplies to the daemon *for the daemon*: the `sh -c` of
`github_apps.<role>.private_key_command`, and `private_key_secret`'s two `secrets get` children
(`runSecretsGet`, minus exactly `SECRETSD_SESSION_TOKEN_FILE` so secretsd scopes the caller by the
launcher pane's tty, not an agent session). They are named in `AGENTS.md` and nowhere else gets
the daemon's environment.

The decision for anything new:

- **Default: `paneEnv`.** Anything a pane can run, anything the runner spawns, anything whose
  environment a pane-side process could reach (`/proc/<pid>/environ` is same-uid readable), and
  the tmux server itself. A `gh` child the daemon spawns for a GitHub App role starts from
  `paneEnv` too (`buildRoleEnv`), minus ambient `GH_TOKEN`/`GH_HOST`/`GH_CONFIG_DIR`, plus the
  minted token and identity.
- **Exception: the daemon's own `process.env`**, only for a child that must read something the
  daemon deliberately keeps from panes, that runs before any pane exists or never in a pane's
  reach, and that is named with its reader in `AGENTS.md`. If you cannot write the sentence
  "this child reads `<variable>` on the daemon's behalf", it is not this case.
- A pane that turns out to need a variable not on the list is **a bug in the list**: add the name
  with a comment naming its reader in `PANE_ENV_ALLOW_LIST`. Never a prefix or wildcard, never a
  strip-list fallback. Because the start-up probes run under the same `paneEnv`, a variable OMP
  itself needs fails the boot loudly instead of the first pane (`config-env-keys-that-panes-also-carry.md`
  records the two config keys the daemon reads *and* writes to panes).

Two things the boundary is easy to blur:

- **Daemon configuration and pane environment are different reads of different objects.**
  `LEGION_OMP_PATH` and the other `LEGION_*_PATH` overrides are read from the daemon's own
  environment, while the `mise where` lookup that turns `omp_invocation` into a binary runs under
  `paneEnv` — `resolveOmpInvocation` takes both. Collapse them one way and config leaks into
  panes; the other way and the daemon's own tool resolution breaks (the test is literally
  "honours `LEGION_OMP_PATH` although panes never inherit it").
- **Audit by enumerating spawn sites, not by reading the docs.** The reviewer found a dead
  `jj log` child (the codebase-index module) by grepping every `spawn`/`spawnSync`/`run(` in the
  daemon and classifying each; that is also how the two `process.env` children were confirmed
  to be the only two. Round 3 blocked on a single doc clause that said "the one child" when
  there were two — the exception list must be exhaustive and named.

## Proving it

Names, never values, everywhere. The environment of a process is read as names:

```sh
pid=$(tmux -L legion-<slug> display-message -p '#{pid}')                 # the server
pid=$(tmux -L legion-<slug> display-message -p -t %<n> '#{pane_pid}')      # a pane
tr '\0' '\n' < /proc/$pid/environ | grep -c -e PRIVATE_KEY -e FOO_SECRET   # 0 (grep exits 1)
tr '\0' '\n' < /proc/$pid/environ | cut -d= -f1 | sort                     # names only
```

The smoke rig's checkpoint 13 runs that over the server and every recorded pane, plus any name in
`SMOKE_CANARY_ENV`: plant `FOO_SECRET=canary` in the daemon's environment at `up.sh` and prove it
reaches nothing. Two stronger shapes: fork the private server by hand *before* the daemon boots
with the stale variables in its environment (the production shape — an older daemon's server
still carrying the keys) and read the boot log's `removed N variable(s) …` line; attach once from
a shell carrying a fake `SSH_AUTH_SOCK`, restart, and check the session table is markers only.
Note that a server forked by hand fails checkpoint 13 on its own pid by construction (its
fork-time environ is the planted one); run 13 against a daemon-forked server for the green line.
Two harness traps from writing that checkpoint: `$PPID` inside a command substitution names a
subshell that has already exited, so the harness inspects long-lived `env -u … sleep` processes
it started itself; and the harness must `-u` every inspected name from its *own* shell first,
because a worker pane's shell legitimately carries `LEGION_BOOT_TOKEN_FILE` and friends.

## Related

- `scrub-tmux-environment-tables-by-parsing-show-environment-s.md` — the scrub's mechanism and design history.
- `human-tier-secretsd-keys-from-the-daemon.md` — `private_key_secret` and why its `secrets get` child keeps the daemon's environment.
- `config-env-keys-that-panes-also-carry.md` — `LEGION_DAEMON_URL`/`LEGION_STATE_DIR`, config inputs that are also pane outputs.
- `pane-lifetime-files-the-daemon-names-but-does-not-write.md` — how secrets reach a pane as `*_FILE` pointers instead of `-e` values.
