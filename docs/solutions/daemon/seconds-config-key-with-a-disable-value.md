---
title: "A seconds config key with a disable value: bound it at 2147483 for the 32-bit timer, accept only the canonical literal 0, validate every source through one check"
category: daemon
tags:
  - config.ts
  - setTimeout
  - 32-bit
  - validation
  - env-parsing
  - silent-fallback
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "LEGION-30"
  - "sjawhar/legion#973"
  - "LEGION-46"
  - "sjawhar/legion#1016"
---

# A seconds config key with a disable value: bound it at 2147483 for the 32-bit timer, accept only the canonical literal `0`, validate every source through one check

## Context

`worker_idle_retire_seconds` / `LEGION_WORKER_IDLE_RETIRE_SECONDS` (LEGION-30) is the daemon's
first lifecycle number where `0` is a meaningful setting ("never retire an idle worker"). The
existing `readPositiveInteger`/`parseEnvPositiveInteger` twins reject `0`, so it needed its own
parsers — and review round 1 found two defects in the first version of them, both of the
"accepted value silently inverts the feature" class. Both fixes are now the pattern for any future
key of this shape.

## Defect 1 — a seconds value above 2147483 becomes a 1 ms timer

`retireMs = seconds * 1000` is handed (via `boundedWait` → `createCancellableSleep`) to
`setTimeout`, whose delay is a signed 32-bit integer. Above `2_147_483_647` ms Bun emits
`TimeoutOverflowWarning: … does not fit into a 32-bit signed integer. Timeout duration was set to 1`
and fires after ~1 ms. Reproduced on Bun 1.3.14: `setTimeout(fn, 31536000 * 1000)` fired in 6 ms.

For a key with disable semantics this is the natural operator mistake: "set a year to mean never."
The result is every finished worker retired the instant it goes idle — the feature inverted, and
nothing in the daemon's own logs says why (`retiring idle worker …: idle for 31536000s` reads as a
lie).

Fix: the bound `2_147_483` — the largest whole number of seconds whose millisecond delay fits —
enforced at every source; the error names the key, the bound, and `0` as the real disable value:

```
worker_idle_retire_seconds must be at most 2147483; use 0 to disable idle retirement
```

PR #973 gave this key its own constant. LEGION-46 (PR #1016) generalized the bound to every
duration setting: the constant is now `MAX_TIMER_SECONDS` in `config.ts`, the refusal comes from
the shared `checkAtMost(number, field, max, hint?)`, and `checkIdleRetireSeconds` passes the
`use 0 to disable idle retirement` hint — the one thing about this key's bound that is still
its own. The audit method, the product rule, and the unit hazard are in
[`bound-every-duration-setting-at-the-32-bit-timer-limit.md`](bound-every-duration-setting-at-the-32-bit-timer-limit.md).

## Defect 2 — `Number()` manufactures a zero from a typo

`Number("  ")` is `0`; `Number("-0")` is `-0` and `-0 < 0` is `false`; `Number("05")` and
`Number("+5")` are `5`. A parser built on `Number()` alone lets a whitespace-only or `-0`
environment variable *silently disable* the timer where the positive twin would refuse to start —
exactly the silent fallback the repository forbids. The YAML loader has the same shape on the file
path: `worker_idle_retire_seconds: -0` parses to a real negative zero.

Fix: "disabled" is only the canonical literal `0`.
- Environment: `undefined`/`""` is "unset" (mirroring `parseEnvPositiveInteger` exactly, so the two
  env parsers disagree only on `0`); anything else must match `/^(0|[1-9]\d*)$/` before `Number()`.
- Both paths: `Object.is(number, -0)` is rejected alongside `number < 0`.
- Error text is the same key-naming message as the negative/non-integer case.

Tests pin eight spellings from env (`" "`, `"-0"`, `"05"`, `"+5"`, `" 5"`, `"5 "`, `"0x10"`,
`"1e3"`), `""` still resolving to the default, and YAML `-0`.

## The shape: one validator, three convergence points

`resolveValue(cli, file, env, default)` means a **cli override bypasses both the file and env
parsers**. The first version bounded nothing and validated only non-negativity post-resolve; the
fix put the whole rule in one function and called it from all three places:

```ts
function checkIdleRetireSeconds(number: number, field: string): number   // the rule
function readIdleRetireSeconds(value: unknown, field: string)            // file  → check
function parseEnvIdleRetireSeconds(value: string | undefined, field)     // env   → canonical regex → check
checkIdleRetireSeconds(resolved.value, "workerIdleRetireSeconds")        // post-resolve (covers cli)
```

Deliberately *outside* the `readPositiveInteger` file-key loop and the `lifecycleNumbers` post-
resolve loop, each with a comment saying why (those loops enforce `> 0`). Adding the key to either
loop would have been the path of least resistance and would have rejected `0`.

## Checklist for the next "0 means off" or bounded seconds key

- Does the value reach `setTimeout` (directly or via `boundedWait`)? Bound it with
  `MAX_TIMER_SECONDS` through `checkAtMost`, with a `hint` that names the disable value.
- Is `0` meaningful? Then `Number()` is not enough: canonical-spelling regex on env, `Object.is(-0)`
  on both paths, `""` stays "unset" like the sibling parsers.
- One `check*` function; call it from the file parser, the env parser, and once post-resolve so
  a cli override cannot skip it.
- Tests: boundary accepted from both sources; boundary+1 rejected from file, env, *and* cli
  override with the exact message; the non-canonical spellings; `""` → default; YAML `-0`.
- Verify the runtime behaviour once (`bun -e 'setTimeout(..., 2147484*1000)'`) rather than trusting
  the constant — the clamp is a runtime fact, not a TypeScript one.
