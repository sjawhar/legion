---
title: "A strict schema's new required field is censused across every parser, rigs and stand-ins included: the contract gate pairs the daemon with the plugin and nothing else"
category: legion
tags:
  - daemon-api-contract
  - strict-schema
  - contract-change-census
  - grant-rig
  - stand-in-daemon
  - zod
date: 2026-10-08
status: active
module: packages/contracts, packages/pi-legion, packages/daemon
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
---

# A strict schema's new required field is censused across every parser, rigs and stand-ins included

Extends docs/solutions/daemon/plugin-daemon-api-contract-version-gate.md.

- The contract gate (`DaemonAPIVersion` against `legion.daemonApiVersion`) pairs one producer with
  one consumer: the daemon and the plugin that boots against it. A test rig, a stand-in daemon or a
  fixture that hand-writes a body and parses it through the same strict schema has no version to
  bump and no gate to refuse it; a required member added to the schema breaks it at module load.
- Before the push that adds a required field to a `strictObject` in `packages/contracts`, census
  every parser: `grep -rn "<SchemaName>" packages scripts --include=*.ts --include=*.sh`, then give
  each hit a disposition — the real reader (paired by the gate), a fixture (regenerated), a
  hand-written literal (migrated), a `jq` read (additive). Write the census into the pull request
  body (`## Contract change census`, one row per boundary: the change, the search and its hits with
  their disposition, the rollout), so the reviewer re-runs the searches instead of trusting the
  list.
- Only `go build`, `tsc` and the plugin's own suite are green at a broken head: the stand-in is
  TypeScript outside the Go module, starts under no CI job, and dies before it binds a port. The
  census is the only check.

## Evidence

sjawhar/legion#1846 made `capabilities` a required member of `LegionStateResponse`
(`packages/contracts/src/legion-api.ts`) and bumped the daemon API contract (`DaemonAPIVersion` in
`packages/daemon/internal/api/version.go`) with the plugin in the same commit, as the gate's rule
says. Round 1's blocking review finding, outside the diff:
`packages/pi-legion/scripts/grant-rig/daemon-standin.ts` builds its `GET /legion/v1/state` document
with a literal and parses it at module load (`LegionStateResponse.parse({...})`), so the grant rig
(`scripts/grant-rig/README.md`) and the skill-scenario rig (`scripts/skill-scenarios/rig.sh`) threw
`ZodError: invalid_type at path capabilities` before printing `listening on`. Lint, typecheck and
every test were green at that head. The fix is one line, `capabilities: []`, the empty list the
daemon's `MarshalJSON` emits for an empty report; the proof started the stand-in as the rigs do and
read its state through the strict schema, with the pre-fix literal as the negative control.

The census the review asked for found six parsers of the schema: the definition, its fixture test,
`packages/pi-legion/src/daemon-client.ts` (the gated reader), the stand-in (the one migrated), and
`scripts/e2e/stage3-devbox-workflow.sh`'s `jq` over `legion state --json` (additive, unaffected).
