---
title: "Legion smoke rig: faking tmux -L, real processes for /proc checks, and what a live run actually needs"
category: testing
tags:
  - smoke-rig
  - bash-harness
  - tmux
  - proc-environ
  - fake-binaries
  - live-testing
  - dispatch
date: 2026-09-11
status: active
module: scripts/smoke
problem_type: testing
component: scripts/smoke
severity: medium
applies_when:
  - Editing scripts/smoke/*.sh or their *.test.sh harnesses
  - Running the rig live to verify a daemon change end to end
  - A checkpoint inspects another process's argv or environment
related_issues:
  - "LEGION-6"
  - "sjawhar/legion#923"
---

# Legion Smoke Rig: Faking tmux -L, Real Processes for /proc Checks, and What a Live Run Actually Needs

## Context

LEGION-6 added checkpoint 13 (no recorded Legion process carries a secret on argv or in its
environment; the default tmux server hosts no `legion-<slug>` session) and moved every rig tmux
call onto the daemon's private socket. The harness changes and the tester's live run each
surfaced things the next person touching the rig should not rediscover.

## Harness (`checkpoints.test.sh`, `down.test.sh`)

- **A fake for a stateful binary must accept every argv shape the real one gets.** The fake
  `tmux` parses `-L <socket>` *before* dispatching on the subcommand and logs `<socket> <argv>`
  per call to `TMUX_LOG`; a fake that switched on `$1` silently mis-dispatched every call once
  `-L` moved to argv[1]. `has-session` on the default server (empty socket) reports no session
  so checkpoint 13's default-server probe passes.
- **Assert over the whole log after the last checkpoint, not after the first.** The private-
  socket assertion originally ran after checkpoint 1 only, so checkpoints 2–12 appended
  unchecked (round-1 S3). Now: every logged line must carry `legion-<slug>` except the one
  intentional default-server probe ` has-session -t legion-<slug>`, whose presence is also
  asserted. Negative control before committing: plant a bare `tmux list-windows` into a
  checkpoint and confirm the harness names it.
- **`/proc/<pid>/environ` inspection needs real, long-lived processes whose environment you
  control.** `env -u DISPATCH_TOKEN … sleep 300 &` (clean) and `DISPATCH_TOKEN=… sleep 300 &`
  (planted), pids handed to the fake tmux via `FAKE_TMUX_PID`; a `$PPID` trick names a
  command-substitution subshell that has already exited. Kill them in the `EXIT` trap.
- **Scrub every inspected variable from the harness's own environment.** The harness may run
  inside a Legion worker pane, which legitimately carries `LEGION_BOOT_TOKEN(_FILE)`; the
  "clean" `sleep` must `env -u` all three families or checkpoint 13 fails on the harness itself.
- A checkpoint that needs no Dispatch call must not call `require_dispatch_token`; document
  "run it bare" in the README next to the `secrets DISPATCH_TOKEN --` invocation.
- `down.test.sh`'s fakes gain `if [[ "$1" == "-L" ]]; then shift 2; fi` as their first line and
  the harness asserts the kill landed as `-L legion-omp kill-session -t legion-omp`.

## Live run (what the tester needed beyond the README)

- **`SMOKE_WEBHOOK_MODE=none` cannot run checkpoints 1–4** despite the README: no Dispatch issue
  event reaches the rig's isolated NATS and resync's `healStatusDrift` skips keys the daemon
  never ingested, so the root issue is never tracked. Use `envoy` mode (production NATS → rig
  NATS bridge).
- **`LEGION_OMP_PATH` must point at the rpcfix OMP** production runs with (the same override as
  `run-daemon.sh`); the rig's pinned mise OMP dies at its first RPC prompt (`send did not invoke
  the agent`) and the private server exits with it.
- **`DISPATCH_TOKEN` is not a secretsd key on this box**; the documented source is
  `~/.config/opencode/envoy.json` `dispatch.token`. Inject it into the subprocess environment;
  never print it.
- **`up.sh ensure_root_issue` 409s (`POSSIBLE_DUPLICATE`)** against prior smoke roots; create
  the root by hand with `force: true`, record it at `${SMOKE_DIR}/root-issue`, re-run `up.sh`
  (it reuses everything). Pre-existing rig bug.
- An architect that drives Dispatch through `eval` bypasses the `tool_result` subscribe hook, so
  it holds no topic interest and a Dispatch message on the root issue does not wake it —
  checkpoint 3's child-issue assertion may need an operator nudge.
- After `down.sh` (SIGTERM, no persist with cleared locators) the pane secret files remain under
  `secrets/`; the boot-time prune reaps them on the next start. Expected.
- The daemon's startup GitHub bot-identity lookup is unauthenticated (60 req/hr per IP); on a
  shared box the rig daemon can die at boot with `GitHub App bot identity lookup failed (403)`
  until the window resets. Pre-existing, needs its own issue.
- **Never print `/proc/<pid>/environ` (or prefixes of it) for production panes.** Inspecting
  pre-upgrade panes this way put real bearer/boot tokens into a transcript (redacted afterwards
  by decision, no rotation). Report only the variable *name*, as checkpoint 13 does
  (`grep -m1 "^${name}="` → `${entry%%=*}`).
- Negative control for checkpoint 13 on the live rig:
  `tmux -L legion-<slug> set-environment -g LEGION_BOOT_TOKEN planted` → checkpoint fails naming
  the variable; `set-environment -g -u LEGION_BOOT_TOKEN` → `CHECKPOINT 13 OK`.

## Related

- `scripts/smoke/README.md` — the operator runbook (attach command, `<1-13>`, "run 13 bare").
- `docs/solutions/integration-patterns/secret-file-pointer-precedence.md` — the contract the
  checkpoint verifies.
