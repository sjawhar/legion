> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "A teardown script keys every destructive action on the rig's own state directory, and tests ownership exactly"
category: legion
tags:
  - smoke-rig
  - teardown
  - shared-machine
  - ownership-test
  - docker
  - tmux
  - fail-closed
  - test-fixtures
date: 2026-09-13
status: active
module: retired smoke rig
related_issues:
  - "sjawhar/legion#1010"
---

# A teardown script keys every destructive action on the rig's own state directory, and tests ownership exactly

## Context

The retired smoke rig's `up.sh` / `down.sh` let several testers run rigs side by side on
one machine — separate ports, separate scratch directories — but two things it destroyed were
still named globally: the NATS container `legion-smoke-nats` (one fixed name for every rig) and
the tmux server `legion-<slug>` (derived from the `SMOKE_PROJECT` environment variable at
teardown time). Two other testers' live rigs were destroyed on 2026-09-13 while LEGION-41 was
under review:

- **09:34Z** — a review subagent ran the real `down.sh` with `SMOKE_PROJECT=sjawhar/16` from a
  throwaway `SMOKE_DIR`. `stop_tmux_session` derived `legion-sjawhar16` from the environment and
  killed the LEGION-16 tester's controller pane. The `@legion_owner` marker did not help: that
  rig's own daemon set it, so it says "a Legion rig owns this", never "*this* teardown owns
  this". (PR #1010 review 5190342693, "Pre-existing"; spec acceptance 6.)
- **11:44:50Z** — a tester probe ran `down.sh` from a directory whose `legion.yaml` existed but
  whose container record did not; the legacy fallback removed `legion-smoke-nats` by name, which
  at that moment was LEGION-44's live bus. Their listener died on the lost JetStream KV buckets.
  (PR #1010 body, "Incident, round 5".)

## The rule

Every action that can destroy something reads **only** what the rig wrote into its own
`SMOKE_DIR` when it started. Never an environment variable, never a fixed name.

| action | keyed on |
| --- | --- |
| kill the tmux server | the `project:` line of `${SMOKE_DIR}/legion.yaml`, which `up.sh` wrote |
| remove the rig's container | `${SMOKE_DIR}/nats-container`, which `up.sh` wrote after the container ran |
| remove the shared legacy-name container (pre-record rigs only) | `legion.yaml` present and record absent **and** `docker port legion-smoke-nats 4222/tcp` equals the port in that `legion.yaml`'s `nats_urls` |

Corollaries that each cost a review round on LEGION-41:

1. **A directory that never started a rig is inert.** No `legion.yaml` naming a project → no
   tmux stop, no container removal, one printed line saying so. The PID-file stops still run;
   they find nothing.
2. **An environment variable that disagrees is a warning, not an input.** `SMOKE_PROJECT` set
   and different from `legion.yaml` → warn, use `legion.yaml`.
3. **Every parse of the identity fails closed.** Missing, unreadable, directory-shaped, or
   malformed `legion.yaml` / record → nothing removed, a warning naming the file. Never guess.
4. **Ownership is an exact comparison, per line, against the tool's real output.** The first
   port gate was `[[ "$published" == *":${port}"* ]]` — a decimal-prefix substring match over
   `docker port`'s two lines (`0.0.0.0:14222` / `[::]:14222`). A `legion.yaml` port `1422`
   matched a live `14222` and would have removed it (thread r3999609771). The fix extracts
   `:([0-9]+)$` from every line and requires equality on all of them, at least one line, and
   refuses on empty output. The same function is used verbatim by `up.sh`'s reuse test, so "the
   same ownership test" is literally true.
5. **Fixtures never use the value the code compares against by default.** With every test port
   at `14222` (the rig's default `NATS_PORT`), a comparison against a constant or against
   `${NATS_PORT:-14222}` passes for the wrong reason. Move fixtures off the default (`14731`)
   and add the prefix (`1473` vs `14731`) and suffix (`14731` vs `4731`) sub-cases; the fake
   `docker port` prints both lines the real one prints.

## Known limitation (filed)

`docker port` reads `NetworkSettings.Ports`, which docker clears when a container stops. A
stopped rig container therefore cannot pass the ownership test: `up.sh`'s `docker start` branch
is unreachable after a host reboot (it fails with the misleading `is not mapped to configured
port`), and `down.sh`'s legacy branch leaves a stopped fixed-name container with the `none`
message. Trunk's substring test failed the same way on empty output; this PR did not introduce
it. The fix is to read `HostConfig.PortBindings` via `docker inspect --format` instead. The
LEGION-41 architect re-filed it to the controller as its own LEGION issue on 2026-09-13
(reviewer finding, PR #1010 review 5190800368).

## Residual

`docker rm -f` runs unguarded under `set -e` after the ownership test; a container removed by
someone else in between aborts `down.sh` before `RIG DOWN`. Harmless (nothing follows the
banner), noted by the reviewer, left as is.
