> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

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
  - shellcheck
  - xdg-config-home
date: 2026-09-11
status: active
module: retired smoke rig
problem_type: testing
component: retired smoke harness
severity: medium
applies_when:
  - Reading the historical rig evidence or designing current daemon test fixtures
  - A fixture inspects another process's argv or environment
  - A harness case must prove that a controlled environment variable wins or a default applies
related_issues:
  - "LEGION-6"
  - "sjawhar/legion#923"
  - "LEGION-40"
  - "sjawhar/legion#1011"
---

# Legion Smoke Rig: Faking tmux -L, Real Processes for /proc Checks, and What a Live Run Actually Needs

## Context

LEGION-6 added checkpoint 13 (no recorded Legion process carries a secret on argv or in its
environment; the default tmux server hosts no `legion-<slug>` session) and moved every rig tmux
call onto the daemon's private socket. The harness changes and the tester's live run each
surfaced things the next person touching the rig should not rediscover.

## Harness (`up.test.sh`, `checkpoints.test.sh`, `down.test.sh`)

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
- **`up.test.sh` sources a stripped `mktemp` copy of `up.sh`, so a sibling `up.sh` sources by
  `BASH_SOURCE` must be copied beside that copy.** The harness runs `sed '$d' up.sh` (dropping
  `main "$@"`) into a scratch file and `source`s that; inside it `BASH_SOURCE[0]` is the scratch
  path, so `source "$(dirname "${BASH_SOURCE[0]}")/dispatch-config.sh"` looks in the scratch
  directory. LEGION-40 made the scratch a `mktemp -d` and `cp`s `dispatch-config.sh` into it before
  sourcing; the red run before that wiring was `resolve_dispatch_config: command not found`.
  `checkpoints.test.sh` never needed this because it runs `bash checkpoints.sh <n>` in place. (The
  same copy made `repo_root` resolve to `/` there, which only worked from the repository root
  because `bun` resolves `//packages/…/omp-pin.ts` against the cwd; LEGION-71 pins `repo_root` to
  the harness's `project_root` in the same `sed`, so the harness runs from any directory — see
  `harness-that-sources-a-copy-pins-its-root-fails-closed-on-success-and-proves-absence-with-a-call-log.md`.)
- **A harness that proves a default must clear the setting it is testing.** The retired default
  cases inherited a former webhook-mode setting from the caller, so the operator-shaped invocation
  selected a different mode than the bare case. The fix controlled that setting in the child shell.
  Rule: for every configuration variable a runbook describes, include a fixture whose expected
  outcome depends on the variable being absent and control it in that fixture.
- **"An exported variable wins" is proven in a child bash, not with `export` inside `$(…)`.**
  `[[ "$(export DISPATCH_URL=…; resolve_dispatch_config && printf …)" == … ]]` is what the plan
  wrote; shellcheck 0.11.0 flags it SC2030 at the subshell and SC2031 at every later
  `export DISPATCH_URL=` in the same file (the harness's own `main` setup), so the "no new
  findings" gate cannot pass. Directives do not help: `disable=SC2030,SC2031` at the subshell
  leaves the later SC2031s, and a subshell-bodied function `f() ( export …; … )` is flagged the
  same way. The clean shape is a function that runs the resolver in a fresh `bash` with the pair as
  a prefix assignment on that child — `DISPATCH_URL="$1" DISPATCH_TOKEN="$2" bash -c 'fail() {…};
  source "$1"; resolve_dispatch_config && printf …' _ "$shared_file"` — which is also exactly how an
  operator's `export` reaches the script. A `local`-scoped wrapper is the other lint-clean shape but
  does not exercise the environment path.

## Historical live-run findings (procedure suspended)

Three of these were pre-existing rig defects and were fixed by LEGION-10 (sjawhar/legion#957).
They remain historical evidence; the rig launch procedure must not be recreated.

- **A mode without Dispatch events could not run checkpoints 1–4.** The daemon never ingested the
  root issue, so resync skipped it. The configuration gate later blocked those checkpoints before
  a network request. The durable principle is that an unreachable prerequisite is reported as
  blocked, not as a test failure.
- **`LEGION_OMP_PATH` had to point at the rpcfix OMP** production ran with; the rig's pinned mise
  OMP died at its first RPC prompt (`send did not invoke the agent`). *Fixed by LEGION-32 (#983)
  moving the one pin in `packages/daemon/src/daemon/omp-pin.ts` to a release with the fix, and by
  #957 making `up.sh` verify that pin through `mise where` in preflight.* `LEGION_OMP_PATH` is
  now only an explicit override for a non-release build; do not export it for a normal run.
- **Dispatch credentials came from the local Envoy configuration, not a secretsd key.** The retired
  configuration helper resolved both URL and token when environment variables were absent; a test
  that left pane-provided values in place passed through an unintended fallback. A controlled
  fixture must make the intended source explicit and never print the token.
- **The former root-issue creation could receive a 409 near-duplicate response.** The case showed
  why a test's identity must be disposable and its creation contract explicit.
- An architect that drives Dispatch through `eval` bypasses the `tool_result` subscribe hook, so
  it holds no topic interest and a Dispatch message on the root issue does not wake it —
  checkpoint 3's child-issue assertion may need an operator nudge.
- After `down.sh` (SIGTERM, no persist with cleared locators) the pane secret files remain under
  `secrets/`; the boot-time prune reaps them on the next start. Expected.
- **Relocating the configuration home also relocates secretsd's configuration.** A historical
  failure variant changed only the outer environment and therefore failed before the intended
  Envoy configuration read. The durable lesson is to identify the layer producing a negative
  result; an earlier configuration failure is not evidence about a downstream path.
- The daemon's startup GitHub bot-identity lookup is unauthenticated (60 req/hr per IP); on a
  shared box the rig daemon can die at boot with `GitHub App bot identity lookup failed (403)`
  until the window resets. Pre-existing, needs its own issue.
- **Never print `/proc/<pid>/environ` (or prefixes of it) for production panes.** Inspecting
  pre-upgrade panes this way put real bearer/boot tokens into a transcript (redacted afterwards
  by decision, no rotation). Report only the variable *name*, as checkpoint 13 does
  (`grep -m1 "^${name}="` → `${entry%%=*}`).
- **A negative environment control must name the inspected variable, not print its value.** The
  former live check planted a boot-token variable and asserted the checkpoint identified the name.
  Current equivalent coverage belongs in the daemon test harness and fixture-owned processes.

## Related

- The removed rig's runbook is suspended; its test-design lessons remain here.
- `docs/solutions/integration-patterns/secret-file-pointer-precedence.md` — the contract the
  checkpoint verifies.
- `docs/solutions/testing/bash-harness-cases-that-pass-for-the-wrong-reason.md` — the mutation
  discipline for these harnesses; the `repo_root`-resolves-to-`/` fact above is what its §2 probe
  died on.
- `docs/solutions/daemon/config-env-keys-that-panes-also-carry.md` — the pane scrub a live run
  needs, and why `DISPATCH_URL`/`DISPATCH_TOKEN` are scrubbed rather than re-supplied.
- `docs/solutions/legion/worker-pane-shell-gotchas.md` §14 — the bash tool's `jq` is jaq; verify a
  `jq` expression with the `jq` the script runs.
