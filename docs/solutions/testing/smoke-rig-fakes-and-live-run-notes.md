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
- A checkpoint that needs no Dispatch call must not resolve `DISPATCH_TOKEN`: resolution lives
  inside `dispatch_request` (shared `dispatch-config.sh`, LEGION-40), so checkpoint 13 runs with
  neither variable set and the README says so next to the `checkpoints.sh` invocation.
- `down.test.sh`'s fakes gain `if [[ "$1" == "-L" ]]; then shift 2; fi` as their first line and
  the harness asserts the kill landed as `-L legion-omp kill-session -t legion-omp`.

## Live run (what the tester needed beyond the README)

Three of these were pre-existing rig defects and were fixed by LEGION-10 (sjawhar/legion#957);
they are kept here as history with the fix named, so nobody re-applies the workaround.

- **`SMOKE_WEBHOOK_MODE=none` cannot run checkpoints 1–4** despite what the README said then: no
  Dispatch issue event reaches the rig's isolated NATS and resync's `healStatusDrift` skips keys
  the daemon never ingested, so the root issue is never tracked. Use `envoy` mode (production
  NATS → rig NATS bridge). *Fixed in #957:* `checkpoints.sh` now blocks 1–4 and 12 with
  `SKIPPED-BLOCKED` (exit 3) under `none` and `forward`, and the README says `envoy` is the only
  mode for them — see `docs/solutions/legion/smoke-rig-modes-gate-checkpoints-and-the-pin-has-one-home.md`.
- **`LEGION_OMP_PATH` had to point at the rpcfix OMP** production ran with; the rig's pinned mise
  OMP died at its first RPC prompt (`send did not invoke the agent`). *Fixed by LEGION-32 (#983)
  moving the one pin in `packages/daemon/src/daemon/omp-pin.ts` to a release with the fix, and by
  #957 making `up.sh` verify that pin through `mise where` in preflight.* `LEGION_OMP_PATH` is
  now only an explicit override for a non-release build; do not export it for a normal run.
- **`DISPATCH_TOKEN` is not a secretsd key on this box**; the source is
  `~/.config/opencode/envoy.json` `.dispatch.token`. *Fixed by LEGION-40:* `up.sh` and
  `checkpoints.sh` read `.dispatch.serverUrl` / `.dispatch.token` from that file when
  `DISPATCH_URL` / `DISPATCH_TOKEN` are unset (`scripts/smoke/dispatch-config.sh`), and the
  README's command blocks no longer name a `secrets DISPATCH_TOKEN` key. Never print the value.
- **`up.sh ensure_root_issue` 409'd (`POSSIBLE_DUPLICATE`)** against prior smoke roots. *Fixed in
  #957:* the request carries `force: true`; a disposable near-duplicate per run is the rig's
  intent. No by-hand creation is needed.
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
