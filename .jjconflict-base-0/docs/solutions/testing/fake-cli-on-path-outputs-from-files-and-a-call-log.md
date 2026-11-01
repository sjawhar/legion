---
title: "A fake CLI first on PATH: outputs from files, never shell quotes; a call log that records argv, fd 0, and the env you claim to drop; and delegating one verb to the real binary for a live boot"
category: testing
tags:
  - fake-binaries
  - bun-test
  - spawnSync
  - PATH
  - stdin
  - secretsd
  - live-boot
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/__tests__/config.test.ts, packages/daemon/src/cli/__tests__/index.test.ts
related_issues:
  - "LEGION-77"
  - "sjawhar/legion#1017"
---

# A fake CLI first on PATH

## Context

`resolvePrivateKeySecret` (`config.ts`) spawns `secrets` by name from `process.env.PATH`, inherits
the daemon's stdin, and drops one environment variable. None of that is observable from the return
value, so the tests observe the **child**: a fake `secrets` first on PATH records what it was given.
The `withFakeSecrets` helper in `config.test.ts` is the pattern; three things about it were learned
the hard way.

## 1. Fixture text goes in files the script `cat`s — never inside shell quotes

The first draft embedded each output in the script: `printf '%s\n' '${fake.stderr}' >&2`. It broke
the moment a fixture carried a single quote — secretsd's real message `secrets: secret 'GH_NOPE' not
found` printed as `secret GH_NOPE not found` and the exact-message assertion failed on the fixture,
not the code. Write `status`, `value`, and `stderr` to their own files in the fixture's temp dir and
have the script `cat` them; no fixture text is ever shell-interpreted:

```sh
#!/bin/sh
printf '%s\t%s\t%s\n' "$*" "$(readlink /proc/self/fd/0)" "${SECRETSD_SESSION_TOKEN_FILE:-unset}" >> '<dir>/calls'
cat '<dir>/stderr' >&2
case "$*" in
  *--no-request) cat '<dir>/status'; exit <statusExit> ;;
  *--value)      cat '<dir>/value';  exit <valueExit> ;;
esac
exit 2
```

Only the fixture *directory path* is interpolated, and it is `mkdtemp` output under `os.tmpdir()`.

## 2. The call log is the assertion surface for "inherited" and "dropped"

Each invocation appends one tab-separated line: `argv`, `readlink /proc/self/fd/0`, and the variable
under test or `unset`. The test parses the file back into rows and asserts:

- **Order and count**: `["get K --no-request", "get K --value"]` for the success path; exactly one
  row for the agent-tier refusal (the value was never fetched).
- **Stdin inheritance by equality**: every row's fd 0 equals `fs.readlinkSync("/proc/self/fd/0")`
  taken in the test process. Under `bun test` that is a `socket:[…]`, not a tty — irrelevant; the
  contract is "the child got the parent's fd 0", so assert equality, never "is a tty".
- **The dropped variable**: set `process.env.SECRETSD_SESSION_TOKEN_FILE` to a sentinel before the
  call, assert every row reads `unset`, restore in `finally`.

Because the resolver reads `process.env` (like `executePrivateKeyCommand`, and its existing leak
test at `config.test.ts` "strips every pane-secret key …"), the helper mutates `process.env.PATH`
and restores it in `finally`. Bun runs a file's tests sequentially, so this is safe; a parallel
runner would need the resolver to take its environment as a parameter.

Bun's `spawnSync` returns `result.error` with `code === "ENOENT"` for a missing executable — it does
not throw — so the "not on PATH" case is `process.env.PATH = <empty mkdtemp dir>` and an ordinary
`toThrow` on the mapped message.

## 3. "Never spawns" is a marker file, not a spy

The `--check-config` guarantee (`resolveSecrets: false` validates the name and never runs `secrets`)
is tested exactly like the pre-existing `private_key_command` one: a fake `secrets` on PATH that
`touch`es a marker and exits 1, then `expect(fs.existsSync(marker)).toBe(false)` after
`cmdCheckConfig`. A spawn under `--check-config` would both fail the command and leave the marker.

## 4. Booting the real daemon against a fake: delegate the one verb that must be real

The spec asked for one live `legion start` against a fake `secrets`. Boot needs a *real* App key
before it prints its listening lines — `tokenManager.getToken("implement", owner)` mints a JWT during
`startDaemonLocked` — so a fake that returns a made-up PEM dies at GitHub, not at the seam under
test. The fake that worked fabricated **only the tier status** (read from a `tier` file so one script
serves the positive and negative controls) and `exec`'d the real binary for `get <NAME> --value`.
An agent-tier key's value is readable tokenless from any stdin, so the delegation needs no grant,
and the value only ever flows into the daemon's stdout pipe — never a log. Evidence to quote: the
two `[legion] requesting <NAME> from secretsd …` lines, both `listening on` lines, and the fake's
call log showing `/dev/pts/N` (the pane's tty, since the daemon was started from a tmux pane with
stdin left on it) and `unset` on all four rows; then flip `tier` to `agent` and quote the exit-1
refusal after a single `--no-request` row.

Scrub `LEGION_OMP_PATH` along with the rest of the inherited `LEGION_*` family when doing this from a
Legion pane — see `docs/solutions/daemon/config-env-keys-that-panes-also-carry.md`, "Working from a
pane".

## Related

- `docs/solutions/daemon/human-tier-secretsd-keys-from-the-daemon.md`: why stdin and the token
  variable matter (secretsd's caller identification and grant scope).
- `docs/solutions/testing/fresh-state-dir-boot-beside-a-busy-rig.md`: the scratch-daemon recipe the
  live boot in §4 followed (scratch NATS with the `ENVOY_NOTIFICATIONS` stream, fresh state dir).
- `docs/solutions/testing/bash-harness-cases-that-pass-for-the-wrong-reason.md`: the bash-side
  discipline for fake binaries and negative cases that must fail for the right reason.
