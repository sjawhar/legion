---
title: "Bound every duration setting at the 32-bit timer limit: trace each setTimeout delay back to its config key, bound products as one timer, and validate in the unit the value has at that point"
category: daemon
tags:
  - config.ts
  - setTimeout
  - 32-bit
  - MAX_TIMER_SECONDS
  - validation
  - unit-conversion
  - audit-method
  - env-parsing
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/config.ts
related_issues:
  - "LEGION-46"
  - "sjawhar/legion#1016"
  - "LEGION-30"
  - "sjawhar/legion#973"
symptoms:
  - "TimeoutOverflowWarning: 2147484000 does not fit into a 32-bit signed integer. Timeout duration was set to 1."
  - "a boot watchdog, resync interval, or stop timeout set to a huge value fires after 1 ms instead of never"
  - "a config key bounded on its own still overflows the timer that multiplies it by a second key"
---

# Bound every duration setting at the 32-bit timer limit: trace each `setTimeout` delay back to its config key, bound products as one timer, and validate in the unit the value has at that point

## Context

`setTimeout`'s delay is a signed 32-bit integer. Above `2_147_483_647` ms Bun does not throw: it
logs `TimeoutOverflowWarning … Timeout duration was set to 1` and fires after ~1 ms (confirmed on
Bun 1.3.14: `setTimeout(fn, 2147484 * 1000)` fired in 5 ms; `2147483 * 1000` stayed pending). A
daemon duration setting that is only required to be a positive integer therefore turns, at one
mistyped value, into an instant-fire timer — the boot watchdog retires every booting worker, the
resync spins, the slow-command budget kills every boot probe and workspace clone so the launch
hold never releases.

LEGION-30 (PR #973) bounded one key, `worker_idle_retire_seconds`, with its own constant. Its
reviewer asked for the same bound on every such key; LEGION-46 (PR #1016) is that generalization.
The spec named five `*_seconds` keys and `linger_hours`; the planner's audit found three things
the spec had not, and each is a reusable method rather than a one-off.

## The audit: from every timer sink back to its config key

A spec that says "bound every timer setting" is a claim to verify, not a list to copy. The method
that found the gaps:

1. **Enumerate the sinks, not the keys.** Grep the module for every `setTimeout`, `setInterval`,
   and the helpers that wrap them (`boundedWait`, `createCancellableSleep`/`cancelableSleep`,
   the command runner's kill timer in `state/fetch.ts`). Each call's delay argument is a lead.
2. **Trace each delay backward to the config field(s) that built it.** Doing this for the daemon
   found `slow_command_timeout_seconds` — `state/fetch.ts` hands
   `slowCommandTimeoutSeconds * 1000` to `setTimeout(() => kill("budget"), limitMs)` for both boot
   probes and every provisioning command. The spec's Summary had listed only the lifecycle
   timeouts; the key is a real timer and the worst offender (above the bound the daemon can never
   open a pane).
3. **A delay built from two keys is one timer.** `processes.ts`'s root and controller registration
   deadlines are `workerBootTimeoutSeconds * 1000 * workerBootRegistrationDeadlineIntervals` in
   a single `boundedWait`. Bounding each factor at 2147483 leaves the product open: the bound
   itself times the default 3 intervals overflows, and `1_000_000 × 3` — each factor well within
   bound — retires every booting root. Rule: after per-key bounds, grep for any expression that
   multiplies two config fields on the way to a timer and bound *that expression* in the unit the
   timer sees. The refusal names both keys:
   `worker_boot_timeout_seconds * worker_boot_registration_deadline_intervals must be at most 2147483`.
4. **Classify each duration-shaped key as a raw delay or a timestamp offset.** `linger_hours`
   never reaches `setTimeout`: `beginLinger` writes
   `lingerUntil = new Date(now + lingerHours * HOUR_MS).toISOString()` and a fixed
   `setInterval` sweep compares `Date.parse(lingerUntil) <= now`. `Date` arithmetic is a 64-bit
   float, so the 32-bit clamp does not apply to it. It is bounded anyway (`MAX_TIMER_HOURS = 596`)
   because the spec chose one rule for every duration key (Rejected: "bounding only the keys
   observed to matter") and because a safe-integer hours value can still push `now + hours *
   HOUR_MS` past the `Date` range (~2.4e9 hours), where `toISOString()` throws `RangeError` at
   linger time. Know the consequence: linger is capped at ~24.8 days. An operator who needs a
   tree to linger longer is asking for a different config shape (a days key), not a raised bound
   — and the code comments and `AGENTS.md` must describe `linger_hours` as a swept deadline, not
   a timer delay, or the next reader will "fix" the bound.

The same trace works for any config surface feeding a runtime sink with a hard limit (buffer
caps, retry counts into allocations): enumerate the sinks, walk back to the inputs, check for
arithmetic that combines inputs, and classify each input by whether it actually reaches the sink.

## The shape in `config.ts`

One constant, one refusal producer, and every source funnels through it:

```ts
const MAX_TIMER_SECONDS = 2_147_483;                         // seconds * 1000 <= 2_147_483_647
const MAX_TIMER_HOURS = Math.floor(MAX_TIMER_SECONDS / 3600); // 596, computed so it cannot drift

function checkAtMost(number, field, max, hint?)  // `<field> must be at most <max>[; <hint>]`
readPositiveInteger(value, field, max?)          // file  → positive integer → checkAtMost
parseEnvPositiveInteger(value, field, max?)      // env   → positive integer → checkAtMost
lifecycleNumbers: Record<string, { value; max? }> // post-resolve: the only guard for a cliOverride
```

A new bounded key must carry its `max` in **three places**, because `resolveValue(cli, file,
env, default)` lets a cli override bypass both parsers: the file loader's `lifecycleKeys` row,
the `parseEnvPositiveInteger` call in `resolveDaemonConfig`, and the post-resolve
`lifecycleNumbers` entry. The tests are shaped to catch exactly a forgotten one: one row per key
from the file, one per `LEGION_*` variable, and a cliOverride refusal — a shared helper passing
for one key proves nothing about the row you forgot. The pre-existing inline
`if (port > 65535) throw` checks became `readPositiveInteger(config.port, "port", 65535)` with
identical messages: once a `max` parameter exists, a second way to say "at most" is a defect.

Two typing notes. The lifecycle table with an optional third column cannot stay `as const`:
destructuring `[fileKey, configKey, max]` from a union of 2- and 3-tuples is a type error, so it
is `ReadonlyArray<readonly [string, string, number?]>`. And the constants are deliberately not
exported: the tests pin the literals `2147483`, `2147483000`, and `596`. A test that derived the
bound from the constant would pass when the constant is wrong; the literal in the test is the
guard, and changing the constant is supposed to turn those rows red.

`checkAtMost`'s `hint` names the remedy when the message alone would not: idle-retire says
`; use 0 to disable idle retirement`. The product refusal has two knobs and no hint; if that
message is touched again, name them.

## Unit hazard: validate in the unit the value has at that point

`resync_interval_seconds` is the one lifecycle setting whose resolved field is in another unit
(`resyncIntervalMs`), and its three sources convert differently: the file loader multiplies by
1000 eagerly, the env value is multiplied only in `resolveDaemonConfig`, and a cliOverride is
milliseconds by contract. Two wrong bounds were one edit away — bounding the post-resolve ms field
at `MAX_TIMER_SECONDS` (refuses a normal `3600`), or checking `resyncIntervalMs.value` before the
env `* 1000` (an env value 1000× over the bound passes). What landed: the normalization is hoisted
into `resyncIntervalMsValue` *above* the check, the post-resolve bound is
`MAX_TIMER_SECONDS * 1000` (`resyncIntervalMs must be at most 2147483000`), and the return reuses
the same value. The rule: convert once, before any bound is judged, and bound each field in the
unit it carries at that point (seconds at the parsers, milliseconds post-resolve). The boundary
test for this key deliberately covers all three sources at exactly the bound and bound+1.

Do not add a second `*Ms` field with the same three-way asymmetry. A new `*_seconds` key should
stay in seconds through `DaemonConfig` like its siblings and be multiplied at its own call site;
`resyncIntervalMs` is the legacy exception, not the pattern.

## Proving it on the real surface

`cmdStart` loads the config before `startDaemon`, so a refused file exits 1 with nothing opened —
no instance lock, NATS, or tmux — and `--check-config` runs the identical parser. The E2E was the
real `legion start` entry against a minimal temp yaml (no `github_apps`, so no secret command
runs) with `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`, and `LEGION_DAEMON_URL` unset (a Legion pane
exports the first two without `DISPATCH_TOKEN`, which alone makes `resolveDaemonConfig` refuse):
bound+1 → `error: <key> must be at most <bound>`, exit 1; bound → `Config OK`; the bound with the
default 3 intervals → the product refusal; with intervals 1 → `Config OK`. Acceptance for the
live deployment was the same command on `~/.config/legion/sjawhar-legion/legion.yaml`, which sets
no timer key. Verify the runtime clamp itself once with `bun -e` — it is a Bun fact, not a
TypeScript one.
