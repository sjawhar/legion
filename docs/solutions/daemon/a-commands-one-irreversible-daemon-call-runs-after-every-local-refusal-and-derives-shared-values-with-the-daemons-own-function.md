---
title: "A command's one irreversible daemon call runs after every local refusal — including the values it derives — and a value the daemon computes is computed on the client by the daemon's own function"
category: daemon
tags:
  - legion-controller-start
  - controller-secret
  - operator-token
  - ordering
  - derived-values
  - shared-sanitizer
  - legionProjectToken
  - regression-tests
  - review-rounds
date: 2026-09-15
status: active
module: packages/daemon/src/cli/controller-start.ts, packages/daemon/src/daemon/config.ts
related_issues:
  - "LEGION-25"
  - "sjawhar/legion#1110"
symptoms:
  - "a CLI that fails on a local misconfiguration has already made the network call whose side effect cannot be undone"
  - "the running controller's credential stops working although nothing about the controller changed — an operator's failed command revoked it"
  - "a value copied from the daemon's own legion.yaml is refused by the client, or accepted and turned into a different token than the daemon's"
  - "a review thread reopens with `Still open:` after a fix that moved the named checks: one more derived value still ran late"
applies_when:
  - Writing a command that calls a daemon route with an irreversible side effect (minting a secret that revokes the incumbent's, consuming a one-shot token, PATCHing a status)
  - A client-side artifact must agree with a value the daemon computes internally (a role token, a sanitized project, a derived path)
  - Reviewing such a command: enumerating what must run before the call
---

# A command's one irreversible daemon call runs after every local refusal, and derives shared values with the daemon's own function

## What happened

`legion controller start` (LEGION-25 Part B, #1110) is the operator's one command against an
in-cluster daemon: read `controller.yaml`, `POST /legion/v1/controller/secret` with the operator
token as a bearer, write the secret 0600, launch Oh My Pi in the foreground. That POST is not a
read: `mintControllerCapability` rotates `controllerCapabilityHash` and revokes the incumbent
controller's grants — the merge queue that is currently running stops being able to act the
moment the response is written. So the command must be *certain it will launch* before it asks.

Round 1 fetched first and then resolved the role-prompt directory and read the `instructions`
file. A missing `LEGION_ROLE_PROMPTS_DIR` or a bad `instructions:` path — local, free to check —
failed the command after the incumbent's secret was gone (review 5204795546, thread
r4011301117). Round 2 moved the two named checks before the fetch (`resolveRolePromptsDir`, a
read-only `readDeploymentInstructions` split out of `materializeDeploymentInstructions`) with
two regression tests asserting zero fetches. The reviewer answered `Still open:`: one more
prerequisite ran late — `controllerToken(config.project)`, the first shape check on `project`
(`/^[a-z0-9]+$/`), sat after the fetch, and `loadControllerStartConfig` only required `project`
non-empty. With `project: sjawhar/legion` — the value the daemon's own `legion.yaml` carries,
and the value an operator copies — every pre-fetch check passed, the secret was minted, and the
command died on the raw token (review 5204985576). Round 3 closed it (386f3562).

## The two rules

**1. Enumerate what the command derives, not only what it reads.** "Every local check before
the network call" is easy to satisfy for I/O — file modes, pointer reads, directory stats — and
easy to miss for values the command *computes* from local input: a sanitized name, a token built
from it, a path joined from config. A partial fix that moves the reads leaves the derivations
where they were. Before the one irreversible call, list every value that is a pure function of
config, environment, and disk, and make each one either computed or refused first. In
`cmdControllerStart` the order is now: `loadControllerStartConfig` (strict keys, token-file mode
`& 0o077`, `project` sanitized) → `controllerToken(config.project)` → the `envoy_token_file` /
`dispatch_token_file` pointer reads → `resolveRolePromptsDir` → `readDeploymentInstructions` →
**`fetchControllerSecret`** → `writeSecretFile` → shim and launcher installs → the environment →
`spawn`. Everything after the fetch is a write the fetch was the precondition for; nothing after
it can refuse on local grounds.

**2. A value the daemon computes is computed on the client by the daemon's function.** The fix
was not a CLI-side validator that accepts `sjawhar/legion`; it was extracting the daemon's own
rule — lowercase, drop every non-alphanumeric, refuse an empty result — out of
`resolveDaemonConfig` as `legionProjectToken(value, field)` (`config.ts`) and calling it from
`loadControllerStartConfig`, so `sjawhar/legion` yields `sjawharlegion` on both sides: the same
controller token, the same `secrets/legion-sjawharlegion-controller` file, the same
`LEGION_PROJECT`, the same refusal text (`project must include at least one alphanumeric
character`). A second implementation that "looks equivalent" is a drift the next edit to either
side breaks silently; one exported function makes the agreement a fact of the code. Look for the
daemon's computation first, however small, and export it — the daemon path stays byte-equivalent
(the reviewer checked exactly that: `resolveDaemonConfig` calls the helper with its old message).

## The test shape that holds the order

Each pre-fetch refusal has a test that asserts three things, not the error text alone: the
`CliError` message, **`fetches` is empty** (the fake `fetch` records every call), and **no state
directory exists** (nothing was written). `controller-start.test.ts`: the 0640 token file, the
empty role-prompts directory, the missing `instructions` file, and `project: "/-_/"` each end
with zero requests; the happy path asserts exactly one request and its exact bearer and body
(`{}`). Counting requests is what turns "the check runs first" from a reading of the source into
a property a refactor cannot break unnoticed.

## Why the reviewer found it and the tests did not

The round-1 tests covered the refusals that existed; the ordering defect is a property *between*
tests — a refusal and a network call in the wrong order — and only a reader asking "what has
already happened when this throws?" sees it. When a route is irreversible, that question is the
review checklist for every command that calls it, and the answer is written down as the
request-count assertion above.

## Related

- [launch-hold-serve-state-spawn-nothing-until-proven](launch-hold-serve-state-spawn-nothing-until-proven.md)
  — the daemon-side shape of the same rule: nothing irreversible until every precondition is
  proven.
- [config-resolution-patterns](config-resolution-patterns.md) — where the daemon's own
  `project` rule lives and how config helpers are shared.
