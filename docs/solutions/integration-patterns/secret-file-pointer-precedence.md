---
title: "Pane secrets travel as *_FILE pointers: precedence, no fallback, and where each consumer reports failure"
category: integration-patterns
tags:
  - secrets
  - environment-variables
  - tmux
  - file-pointer
  - precedence
  - envoy-client
  - pi-envoy
  - daemon
date: 2026-09-11
status: active
module: daemon
problem_type: security
component: secret delivery
severity: high
applies_when:
  - A daemon hands a credential to a child process it launches through tmux (or any spawner whose argv is world-readable)
  - A consumer resolves a secret from an environment variable and a config file today and needs a file-pointer variant
  - A forked server (tmux, a supervisor) inherits the launching process's environment and passes it to every child
related_issues:
  - "LEGION-6"
  - "sjawhar/legion#923"
---

# Pane Secrets Travel as *_FILE Pointers: Precedence, No Fallback, and Where Each Consumer Reports Failure

## Context

A tmux client's argv is world-readable in `/proc/<pid>/cmdline` for as long as it runs, so
`tmux new-window -e DISPATCH_TOKEN=<bearer> …` published every Dispatch bearer, boot token and
controller secret to any local user (finding F1 of LEGION-6; a tester later observed exactly this
on the pre-upgrade production daemon). `sjawhar/legion#923` replaced the values with pointers.
The contract below is shared by three packages and every future consumer must follow it.

## The contract

- The daemon writes each secret to `<state_dir>/secrets/<name>`: directory `0700`, file `0600`,
  contents exactly the token (no newline), modes re-applied explicitly on every write (`mkdir`'s
  mode is umask-masked and ignored for an existing directory; `writeFile`'s mode applies only on
  create). Pane files are named by the role token (`legion-<project>-<key>-<role>`,
  `legion-<project>-controller`) so liveness derives from `state.roles` / tree locators with no
  new state field; the shared bearer is `dispatch-token`, written once at startup.
- A pane receives only `DISPATCH_TOKEN_FILE`, `LEGION_BOOT_TOKEN_FILE`,
  `LEGION_CONTROLLER_SECRET_FILE` — never `DISPATCH_TOKEN`, `LEGION_BOOT_TOKEN`,
  `LEGION_CONTROLLER_SECRET`.
- Consumer precedence: `X_FILE` set → trimmed file contents; unreadable or blank → an error
  **naming the variable and the path** (`X_FILE names <path>, which could not be read: <cause>` /
  `…, which is empty`), **never** a fallback to `X` or to a config file. `X_FILE` unset → `X`
  exactly as before. Both set → `X_FILE` wins silently. A set pointer is a claim the daemon made;
  falling back would hide a daemon bug behind a stale operator export.

## Where each consumer reports the failure — and why they differ

| Consumer | Helper | On a bad `X_FILE` |
| :--- | :--- | :--- |
| `@legion/envoy-client` `resolveDispatchConfig` | `readSecretFile` (`src/secret-file.ts`) | returns `{enabled: false, error}` — a throw from `envoyExtension(pi)`'s top level would take down every Envoy tool, not just Dispatch; `envoy.ts` surfaces `error` as a session-start warning |
| pi-envoy `requiredSecret` / `requiredControllerCapability` (`src/legion/classify.ts`) | imports `readSecretFile` | throws — the boot handshake fails and the worker exits, which is the documented behaviour for a bad boot token |
| daemon CLI `resolveControllerSecret` (`src/cli/index.ts`) | local reimplementation | throws `CliError` |

The CLI reimplements the read because the daemon package does not depend on `envoy-client`;
that leaves three implementations of one rule. A precedence change must touch all three (the
review flagged this as an intentional seam, not a bug).

## Environment inheritance is the other leak

The private tmux server is forked by the daemon's own first `tmux -L legion-<project>` command
and inherits the daemon's environment; a pane's `-e` pairs can add or override a variable but
never remove one the pane would otherwise inherit from the server. So the daemon started from
inside a Legion pane (the smoke rig, a worker's own shell) carries that pane's
`LEGION_BOOT_TOKEN_FILE` — and would hand it to every pane that does not override it. Strip the
whole family (`PANE_SECRET_ENV_KEYS` in `environment.ts`: `DISPATCH_TOKEN`,
`DISPATCH_TOKEN_FILE`, `DISPATCH_URL`, `DISPATCH_MCP_URL`, `LEGION_BOOT_TOKEN`,
`LEGION_BOOT_TOKEN_FILE`, `LEGION_CONTROLLER_SECRET`, `LEGION_CONTROLLER_SECRET_FILE`) from
every child the daemon spawns — the tmux runner, `mise env`, `private_key_command`, GitHub App
`gh` children — and test the strip against both the daemon's `env` and the `mise env --json`
output, since either can carry a leak.

## Private-socket consequences

Every daemon tmux argv is `tmux -L legion-<project> <subcommand> …` (`-L` is a global option and
precedes the subcommand; tests read the subcommand at index 3). On that socket, two `kill-pane`
stderr shapes both mean "the pane is gone": `no server running` (socket file left by an exited
server) and `error connecting to … (No such file or directory)` (socket never created — the
shape a first boot after the upgrade runbook or a reboot produces). Treating only the first as
gone strands every dead worker's locator until some launch happens to fork the server.
Upgrading a live box: stop the daemon, `tmux kill-session -t legion-<project>` on the default
server once, start the new daemon; roots resurrect onto the private server. Attach with
`tmux -L legion-<project> attach -t legion-<project>`.

## Related

- `packages/daemon/src/daemon/AGENTS.md` — the daemon-side contract in full.
- `docs/solutions/daemon/refcounted-hold-guards-a-derived-prune.md` — how the files are kept
  alive while a pane launches and pruned afterwards.
