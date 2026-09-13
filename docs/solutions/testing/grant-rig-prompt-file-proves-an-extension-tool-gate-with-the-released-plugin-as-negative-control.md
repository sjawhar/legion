---
title: "The grant rig's --prompt-file drives a real production-profile phase worker through a tool-gate change; the released plugin under the same prompt is the negative control, and the guard reaches live panes only when that release is installed into the profile before a daemon restart"
category: testing
tags:
  - e2e
  - grant-rig
  - production-profile
  - negative-control
  - tool-call-hook
  - pi-envoy
  - plugin-release
  - deployment-gap
date: 2026-09-13
status: active
module: packages/pi-envoy/scripts/grant-rig
related_issues:
  - "LEGION-45"
  - "sjawhar/legion#1020"
  - "LEGION-12"
  - "LEGION-54"
---

# The grant rig's --prompt-file drives a real production-profile phase worker through a tool-gate change; the released plugin under the same prompt is the negative control, and the guard reaches live panes only when that release is installed into the profile before a daemon restart

## Context

LEGION-45 (#1020) added a `tool_call` gate to the Legion extension. Its unit tables are the
regression lock; they are not proof that a real phase worker's bash call is refused, because the
gate's behaviour depends on what OMP actually loads into the pane — the plugin tree of the
`legion` profile, `secretsd`'s bash replacement included (LEGION-54 was a grant that the unit
suite delivered and the real pane dropped). The deployment's standing rule (Sami, 2026-09-13)
requires the implementer's proof on a production-like surface before merge. The grant rig
(`packages/pi-envoy/scripts/grant-rig`, built for LEGION-12) already was that surface for grant
delivery; its prompt was hard-coded, so it could only prove grants.

## What changed

`bun scripts/grant-rig/run.ts drive|tui … --prompt-file <path>` replaces the built-in A–G
grant steps with the file's text. Everything else stays real: `RIG_PLUGINS=production` copies
the live profile's whole plugin tree with only the Legion plugin swapped
(`RIG_LEGION_BUILD=branch` for the checkout's `bun run build`, or a published version), the
daemon's launch prefix (`secrets … -- omp --mode rpc`), the OMP pin read from
`~/.config/legion/sjawhar-legion/legion.yaml`, a stand-in daemon, and a real jj workspace at
`$RIG/ws` whose operation log is the evidence. The A–G verdicts the analyzer prints then
describe whatever bash calls the prompt happened to cause and are not that run's evidence.

## The procedure that proved LEGION-45 (three rounds, same shape)

1. Write the prompt with the `write` tool — it names the refused commands, so a heredoc is
   itself refused once the guard is in the pane. Number the items, one tool call each, "never
   retry or rephrase a refused item, move on", and bracket the refused items with a marker
   command and `jj op log` before and after.
2. Branch build: `RIG_PLUGINS=production RIG_LEGION_BUILD=branch sh setup.sh $SRC $RIG`, then
   `run.ts drive --rig $RIG --port $PORT --omp $OMP --prompt-file $RIG/prompt.txt --label <head>`.
   Evidence A: in `$RIG/runs/<label>-*/events.jsonl` the `tool_execution_end` frames for the
   refused items carry the refusal text and `isError`, the allowed items ran. Evidence B:
   `jj -R $RIG/ws op log` shows the marker as the newest mutating operation and no rollback
   operation.
3. Negative control: the same prompt against the plugin the panes run today
   (`RIG_LEGION_BUILD=<installed version>`; read it from
   `~/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json`). The
   refused items *run* and the op log gains the rollback operation. This is what proves the
   guard — not the harness, not the prompt, not the model's mood — is what changed behaviour.
4. Quote both op logs, the `events.jsonl` paths, and the head SHA in the PR body's `E2E` line;
   after a corrective round whose `packages/pi-envoy` source is unchanged, the bare gates are
   enough, otherwise run it again.

Round 3's E2E line also recorded that the branch's `bun run build` output was byte-identical to
the checkout's `dist/legion.js` — the rig runs the built plugin, so a stale `dist/` would test
the wrong code.

## The deployment gap this surface makes visible

The production plugin moved from 1.22.2 to 1.23.0 while LEGION-45 was in flight, and 1.23.0's
`dist/legion.js` has no guard. Nothing about merging #1020 changes a running pane: the daemon
spawns sessions without `--extension`, so they load whatever `@sjawhar/pi-legion-envoy` is
installed in the `legion` profile, and the daemon's boot gate only checks that plugin's
`legion.daemonApiVersion` (this change does not bump it). The guard reaches live panes only when
the pi-envoy release built from the merged commit is installed into the profile — an operator
action on this box — before a daemon restart; until then the skill text is ahead of the
enforcement, exactly as before the issue. The negative control above is the same fact from the
other side: today's panes are the control group. Post-merge verification for a tool-gate change
therefore means: confirm the release version, confirm it is installed in the profile, then drive
one refused item in a real pane — not "the PR merged".
