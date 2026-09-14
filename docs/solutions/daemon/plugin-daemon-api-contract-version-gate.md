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
  - pane-contract
  - LEGION_GRANT_FILE
date: 2026-09-12
status: active
module: packages/daemon, packages/contracts, packages/pi-envoy
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
  - "LEGION-20"
  - "sjawhar/legion#975"
  - "LEGION-52"
  - "sjawhar/legion#1018"
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

- `packages/contracts/src/legion-daemon-api.ts` exports `LEGION_DAEMON_API_VERSION` (an integer:
  `2` when the design gate's `register_gate` request and gate record changed shape, LEGION-20;
  `3` for `ENVOY_TOKEN_FILE` on every pane, LEGION-25; `4` for durable `spawn_worker` requests,
  LEGION-102; `5` when the controller handshake changed — `controllerLocator.ompSessionFile`,
  `/controller/ready`'s session file, `/grants`' controller form, and the merge flag on
  `/gh-token` only — `/git-credential` rejects it, LEGION-16), a plain integer beside the schemas
  it versions.
- `packages/pi-envoy/package.json` carries `"legion": { "daemonApiVersion": <the same integer> }`;
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

## The bump is a step the author must take; the gate only catches its absence at deploy (LEGION-20)

LEGION-20 (#975) changed `register_gate`'s request from `{askId}` to `{artifactId, version}` and
the gate record in the state response — a `LegionDaemonApi` shape change on both sides. Neither
the plan nor the first implementation bumped `LEGION_DAEMON_API_VERSION` or the plugin manifest's
`legion.daemonApiVersion`; the architect caught it in review and the bump landed as a follow-up
commit ("daemon API contract 2 — register_gate and the gate record changed shape"). Nothing
automatic could have caught it: `daemon-api-version.test.ts` pins the two numbers *equal*, not
*changed*, and the gate itself only fires at boot against an installed plugin. Two habits now:

- **Any diff that touches `packages/contracts/src/legion-daemon-api.ts` bumps the constant and
  the manifest in the same commit as the shape change**, and the pull request body says so —
  the contract number, not a `!` marker, is how this repository signals the incompatibility
  (Sami's rule). Put "grep the diff for `LegionDaemonApi`" on the planner's checklist and the
  reviewer's.
- **A serial number claimed on a long-lived branch is re-checked against main at every rebase.**
  Two branches that each change a shape will both bump 1 → 2; see
  `docs/solutions/legion/schema-bump-branch-rechecks-mains-version-at-every-rebase.md`,
  where the same thing happened to the state version on this branch. It then happened to this
  number: LEGION-16 (#961) had also taken 2 for the controller handshake, and after rebasing over
  #975 its tester found main's plugin release at contract 2 passing a daemon whose shapes differed.
  LEGION-16 renumbered to 3 and main's 1.23.x/1.24.0 plugin releases are refused by name — and
  then, after LEGION-25 and LEGION-102 landed at 3 and 4, renumbered again to 5.

## The number covers the pane contract too (LEGION-52)

LEGION-52 (#1018) widened what the contract number means without changing the gate: it also
covers the **pane contract** — every environment variable the daemon sets on a pane that the
plugin reads or writes. Sixteen of them at the time of writing; the authoritative list is the doc
comment on `LEGION_DAEMON_API_VERSION` in `packages/contracts/src/legion-daemon-api.ts`, and the
same list is repeated verbatim in `packages/daemon/src/daemon/AGENTS.md`, `packages/pi-envoy/AGENTS.md`,
and the pi-envoy `CHANGELOG.md` (`boot-probes.ts` points at the constant instead of repeating it).
The rule that follows: a change to one of those variables bumps the constant and the manifest in
the same commit, exactly like an HTTP shape change. Three things the round taught:

- **A list that says "every" is verified with two greps, one per side, and the count is
  checked 1:1.** The first draft was "verified against `processes.ts`" and still missed
  `LEGION_CONTROL_SUBJECT` (root panes only, `processes.ts` root env), `ENVOY_NATS_URL`, and
  `ENVOY_URL` (every pane; read not by the plugin's own files but by `@legion/envoy-client`'s
  `defaults.ts`). The reviewer found them by reading the three pane environments in
  `processes.ts` (root, worker, controller) plus `credentialProcessEnvironment` and the runtime's
  `<NAME>_FILE` pointer, then every `process.env.*`/`env.*` read in `extensions/legion.ts`,
  `src/legion/classify.ts` (`requiredSecret` maps `X` to `X_FILE`), and `envoy-client`. Daemon-set
  but plugin-unread variables (`LEGION_PROJECT`, `LEGION_ROOT_WORKSPACE`, `PATH`, the emptied
  `GH_*`) are deliberately not on the list. Do both greps; a list recalled from a prior commit is
  not a verification.
- **Four verbatim copies stay equal only under a script.** The corrective round checked the
  four texts with a small Python loop extracting the backticked names between `LEGION_GRANT_FILE`
  and `LEGION_CONTROLLER` and comparing them to a canonical array; anything less and one copy
  drifts. The next author who adds a variable either updates all four in one commit and re-runs
  that check, or converts the three prose copies to point at the constant the way
  `boot-probes.ts` already does.
- **What a real skew prints, per direction — and the daemon gates only one direction.** A plugin
  from before the credential file on a daemon that names it never writes the file; every worker
  then fails at its first `legion gh`/`jj git push`, after the work is done, with `grantFrom`'s
  (`packages/daemon/src/cli/index.ts`) `LEGION_GRANT_FILE names <path>, which could not be read:
  ENOENT …: the pi-envoy extension in this pane did not write it — the installed plugin predates
  LEGION-54; install the released plugin in the profile and relaunch the pane`. That is the
  direction the contract gate catches at boot. The reverse — a plugin newer than the daemon, on a
  pane launched without `LEGION_GRANT_FILE` — is not something a daemon can gate (it cannot check
  a plugin newer than itself); the plugin's `tool_call` hook blocks every bash command itself with
  `LEGION_GRANT_FILE is not set on this pane: the daemon that launched it predates this plugin;
  restart the daemon on the matching release`. `LEGION_GRANT is missing` is neither: the 2026-09-13
  incident that produced it was daemon and plugin at the **same** contract with 1.17.1's `env`
  delivery dropped by the secretsd bash shim (`../legion/grant-delivery-plugin-omp-contract.md`) —
  the first draft of the daemon `AGENTS.md` cited it as the skew the gate now catches, and a
  contract number would have been equal on both sides. Before quoting an error string as a
  contract's failure mode, read it out of the source (and the test that pins it) rather than out
  of the incident you remember.

## When another branch takes the number first: a contract integer is a skew detector, not a changelog

LEGION-52 first landed as a code change: bump 1 → 2 on both sides so that a daemon on `main`
would refuse every pre-credential-file release. Fifty-two minutes after its first handoff,
LEGION-20 (#975) merged its own 1 → 2 for the `stateGate`/`GatesRegister` shapes, and release
1.23.0 — the first to declare 2 — also carried the credential file. The tester caught it on the
first round by booting the branch daemon against a copy of the installed 1.23.0 tree (it passed
the gate, which the PR body said it would refuse). On rebase the branch's constant and manifest
edits collapsed to nothing and the pull request became docs-only.

The decision that followed is the transferable part, because it is the *opposite* of the state
schema rule in `../legion/schema-bump-branch-rechecks-mains-version-at-every-rebase.md`. A
migration version keys a step; two branches cannot share one, so the second renumbers. A contract
integer only has to *differ from the number every incompatible release declares*; two branches
that each need "a bump since the last release" are both satisfied by one bump. Bumping to 3 would
have refused 1.23.0 — a release that already has the behaviour — for no change in behaviour and
forced the operator to reinstall. So: when `main` has moved the counter under you, check whether
the release that first declares the new number already carries what your branch needs; if it
does, do not renumber, drop your bump, and land the rule and the history instead. Check at every
phase boundary (`jj file show -r main@origin packages/contracts/src/legion-daemon-api.ts | grep
LEGION_DAEMON_API_VERSION`, and `npm view @sjawhar/pi-legion-envoy@<v> legion` for the release),
not only when GitHub says CONFLICTING — this collision produced a clean rebase.

A rebase that turns a fix PR into a docs PR also changes the review discipline: the "Thermo"
line is skipped (docs-only), the E2E proof becomes "the branch daemon boots against a copy of the
installed release and passes the gate", and every historical statement in the diff is checked
against `main` rather than against the branch's own earlier commits — the first history text this
PR wrote said contract 2 *was* the credential file, which was true on the branch and false on
`main`.

## Related

- `packages/daemon/src/daemon/AGENTS.md`, "OMP invocation and daemon tools": the gate, the bump
  rule, and the load probe, as the current contract.
- `docs/solutions/daemon/handoff-schema-migration-patterns.md`: versioning of persisted state,
  the other place a shape change needs a number.
- `../legion/release-bound-claims-are-verified-against-tags-and-tarballs.md`: how the "first
  release that writes the credential file" bound in this contract's history was checked (and
  first got wrong).
- `../legion/grant-delivery-plugin-omp-contract.md`: the 2026-09-13 incident that is *not* a
  contract skew.
