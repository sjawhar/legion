---
title: "A daemon/plugin contract version gate: why a wire-shape change needs a loud boot refusal, and why a package-version lower bound was the wrong gate"
category: daemon
tags:
  - pi-legion-envoy
  - daemon-api-contract
  - boot-probes
  - strict-schema
  - deploy-ordering
  - version-gate
date: 2026-09-12
status: active
module: packages/daemon, packages/contracts, packages/pi-envoy
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
---

# A Daemon/Plugin Contract Version Gate

## Context

LEGION-21 (PR sjawhar/legion#962) tagged every locator in the daemon's `GET /legion/v1/state`
response with a `runtime` discriminant. That is a wire-shape change, and the installed
`@sjawhar/pi-legion-envoy` plugin validates every daemon response against the strict
`LegionDaemonApi` schemas it bundles (`strictObject`: the first unknown field rejects the parse).
The controller/architect boot handshake calls `daemon.state()` first. Under the new daemon, the
old plugin's controller booted, failed that parse inside the plugin, never claimed its role, and
was retired and respawned by the daemon roughly every six minutes — observed on the smoke rig as
three controller sessions and `roles: {}`, with nothing in the daemon's own log to say why. The
daemon saw a healthy pane that never called `/controller/ready`; the plugin saw a schema error it
had no channel to report.

The fix is a contract version the two sides compare at boot, and this document records the
design and, more usefully, the two alternatives that were rejected and why.

## The gate

- `packages/contracts/src/legion-daemon-api.ts` exports `LEGION_DAEMON_API_VERSION = 1`, a
  plain integer beside the schemas it versions.
- `packages/pi-envoy/package.json` carries `"legion": { "daemonApiVersion": 1 }`;
  `packages/pi-envoy/src/legion/daemon-api-version.test.ts` pins the two equal, so a bump on
  one side without the other fails the plugin's own suite.
- `verifyLegionPluginContract` (`packages/daemon/src/daemon/boot-probes.ts`) reads the installed
  manifest at `getPluginsNodeModules()/@sjawhar/pi-legion-envoy/package.json` — the same
  `DirResolver` precedence OMP uses, so the daemon inspects the plugin its spawned sessions will
  actually load — and refuses startup unless `legion.daemonApiVersion` equals the constant. A
  missing manifest, an unreadable one, or one without the field is the same refusal, never a
  fallback. It runs between the `pi.agents` capability probe and the plugin-load probe
  (`index.ts`: agents → contract → load), before the daemon takes a GitHub token, loads state,
  opens NATS, or serves the API, so a refused boot leaves nothing half-started.
- The message names everything an operator needs:
  `pi-legion-envoy at <manifest path> (package <version>) speaks daemon API contract <n|none>;
  this daemon requires <N>. Install the @sjawhar/pi-legion-envoy release built from this
  daemon's commit into the active profile.`
- Bump rule (now in `packages/daemon/src/daemon/AGENTS.md`): any change to a `LegionDaemonApi`
  request or response shape bumps the constant and the manifest field together, and the
  deployment installs the plugin release built from the same commit into the active profile
  before the daemon restarts on the new code.

Negative control on the rig: a daemon from the branch started against a profile whose plugin
was the released 1.3.0 refused with exactly that message (`speaks daemon API contract none;
this daemon requires 1`); the same daemon against a profile holding a plugin built from the
branch passed and ran checkpoints 1–4 and 13.

## Why a package-version lower bound was rejected

`AGENTS.md` had documented a `legion.minPluginVersion` field that never existed in the code.
Making it real — "the daemon requires plugin ≥ X.Y.Z" — looks simpler than a second version
number, and it was rejected for three concrete reasons:

1. **The PR cannot know the number.** `release.yaml` auto-bumps `packages/pi-envoy/package.json`
   on merge to `main`. A PR that needs "the version containing this change" would have to
   predict the bump, and a wrong guess is either a gate that never opens or one that accepts the
   previous release.
2. **Interleaved releases satisfy the bound with the old contract.** Any unrelated pi-envoy fix
   that merges first produces a version above the bound whose bundled schemas are still the old
   shape. A version number orders releases; it says nothing about which contract they speak.
3. **A branch-built plugin fails the bound.** During implementation and testing the rig runs a
   plugin packed from the branch, which carries the *last released* version string. A lower
   bound set to the next release refuses exactly the build that has the fix.

A contract integer has none of these problems: it changes only when the shape changes, it is
bumped by hand in the same commit as the shape, and a branch build carries the new value.

Two other alternatives were also rejected. A tolerant (non-strict) parse in the plugin would have
hidden the skew instead of surfacing it, contradicts the repository's strict-contract rule, and
does nothing for the plugin already installed. A deploy note alone ("reinstall the plugin before
restarting the dogfood daemon") relies on a human reading a PR body; the failure mode without a
gate is a silent respawn loop, which is worse than any refusal.

## The deploy-ordering consequence

The gate turns a silent loop into a loud refusal, which means the shared dogfood daemon will
refuse to restart on this code until the plugin release built from the merged commit is
installed into its profile. That is correct behaviour, and it must be stated where the person
restarting the daemon will see it: the PR body's deployment section, not only in `.legion/`
handoffs (which are deleted before merge). The tester's negative control against the installed
1.11.1 confirmed the same refusal on the head that merged.

## Test discipline that came out of review

The gate is proven by three refusal cases in `index.test.ts` (missing field, mismatched
integer, unreadable manifest), each asserting that the load probe never ran and no state or NATS
was touched. An early version also pinned `manifestReadCount === 1` — proving the gate read the
manifest exactly once. The reviewer flagged that as a wiring assertion and it was deleted in the
hardening round: a caller observes the refusal, not the read count, and the pin would break on
any harmless refactor of who reads the manifest.

## Where it lives now

LEGION-23 (#966) moved the two OMP launch probes into `boot-probes.ts` so that `startDaemon`
and the hidden `legion probe-image` subcommand (the worker image's last build step) execute
identical code. The contract gate moved there with them on rebase, for the same reason: a worker
image that carries a plugin at the wrong contract should fail its build, not its first boot.

## Related

- `packages/daemon/src/daemon/AGENTS.md`, "OMP invocation and daemon tools": the gate, the bump
  rule, and the load probe, as the current contract.
- `docs/solutions/daemon/handoff-schema-migration-patterns.md`: versioning of persisted state,
  the other place a shape change needs a number.
