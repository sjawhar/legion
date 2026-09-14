---
title: "A wrapper that calls a tool passes the tool's stderr through and classifies the failure on that text; a remedy is named only when the tool itself named the condition the remedy fixes"
category: best-practices
tags:
  - bash
  - diagnostics
  - stderr
  - error-classification
  - mise
  - smoke-rig
  - fake-binaries
date: 2026-09-13
status: active
module: retired smoke rig
applies_when:
  - A script or daemon calls an external tool (`mise`, `gh`, `secrets`, `jj`, `docker`) and reports its failure to an operator
  - You are about to write `cmd 2>/dev/null || fail "<one remedy>"`
  - A remedy string (`run: <install command>`) is printed for every non-zero exit of the tool
related_issues:
  - "LEGION-71"
  - "sjawhar/legion#1029"
  - "sjawhar/legion#957"
---

# A wrapper that calls a tool passes the tool's stderr through and classifies the failure on that text; a remedy is named only when the tool itself named the condition the remedy fixes

## The shape that sends operators after the wrong fix

The retired rig's startup script verified the pinned Oh My Pi build in preflight with:

```bash
install_dir="$(mise where "$omp_pin" 2>/dev/null)" ||
  fail "OMP pin ${omp_pin} is not installed (mise where failed); run: mise install ${omp_pin}"
```

Two things are wrong with it, and they are the two things every wrapper of this shape gets
wrong. `2>/dev/null` throws away the only diagnosis that exists — mise's own message — and the
`||` branch asserts a cause (`not installed`) and a remedy (`mise install`) that the wrapper never
checked. `mise where` fails for other reasons: a broken `MISE_CONFIG_FILE` prints a TOML parse
error and `Invalid TOML in config file: <path>`; an unknown backend prints `<name> is not a valid
plugin name`. For each of those the operator was told the pin is not installed and to run
`mise install <pin>`, which either fails on the same configuration error with the same
suppressed message, or succeeds and changes nothing. The reviewer of sjawhar/legion#957 named
this in the PR's Fast-follow line; #1029 (LEGION-71) fixed it.

## The shape that works

Capture the tool's stderr, show it to the operator whatever the exit, then classify a failure on
the captured text and name a remedy only for the condition the tool named:

```bash
local install_dir mise_stderr mise_said
local mise_status=0
mise_stderr="$(mktemp)"
install_dir="$(mise where "$omp_pin" 2>"$mise_stderr")" || mise_status=$?
cat "$mise_stderr" >&2
mise_said="$(<"$mise_stderr")"
rm -f "$mise_stderr"
if ((mise_status != 0)); then
  [[ "$mise_said" != *'not installed'* ]] ||
    fail "OMP pin ${omp_pin} is not installed (mise where failed); run: mise install ${omp_pin}"
  fail "mise where ${omp_pin} failed; mise's own message is above"
fi
```

Four properties, each of which a test pins:

- **Passthrough on every exit.** A success-path warning (`mise WARN GitHub rate limit exceeded`)
  reaches the operator too; the wrapper never decides which of the tool's lines are worth seeing.
  Test: with a fake `mise` that prints `mise ERROR <pin> not installed`, that exact line must be
  in the wrapper's output (it was not, under `2>/dev/null`).
- **Classification on the tool's text, not on the exit code.** The exit code says "failed"; only
  the text says why. The classifier is a substring the tool actually prints (`not installed`,
  matching mise 2026.8.10 and the harness fake); bash pattern matching, no `grep` dependency.
- **The remedy only for the condition it fixes.** `mise install <pin>` appears in exactly one
  branch. Everything else stops on `mise where <pin> failed; mise's own message is above` and
  names nothing to run — the operator reads the message that is already on the screen. Test: a
  fake that fails with `mise ERROR Invalid TOML in config file: /etc/mise/config.toml` must
  produce output containing that line and the generic failure and *not* containing
  `mise install`. This case was written first and failed on the old shape (remedy printed
  unconditionally), then passed on the new one.
- **Fail-safe direction on drift.** If mise ever rewords `not installed`, the wrapper degrades to
  the generic message plus mise's own text — still correct, only without the shortcut. A
  classifier that instead defaulted to the remedy would degrade to the original bug.

The temp file is the honest way to keep stdout (the install directory, captured by the
substitution) and stderr (the diagnosis) apart in bash; `2>&1` inside the substitution would
mix them into the value. `mktemp` needs no cleanup trap here because the file is removed on the
straight-line path before any exit; a wrapper with several exits between create and remove
would want `trap`.

## Where the same rule already lives, and where to apply it next

The daemon's boot probes classify a child's failure the same way — on what the child printed, not
on its exit code (`../daemon/boot-probe-kill-is-transient-not-a-verdict.md`: a printed negative
marker is definitive, a budget kill without one is transient). `legion gh` and the `secrets`
wrapper are the next places to check: any `|| fail "<fixed remedy>"` after a `2>/dev/null` is
this bug. Two checks before writing such a line: (a) can the tool fail for a reason your remedy
does not fix? If yes, the remedy needs a classifier. (b) Would you want to read the tool's stderr
if you were the operator? If yes, do not suppress it — at most, suppress it on success and only
when the tool is known to be noisy there.

## Related

- `../testing/harness-that-sources-a-copy-pins-its-root-fails-closed-on-success-and-proves-absence-with-a-call-log.md`
  — the retired harness cases that used fake mise failure knobs to pin the four properties above.
- `../testing/fake-cli-on-path-outputs-from-files-and-a-call-log.md` — building a fake CLI whose
  stderr and exit are fixture-driven, for wrappers tested from Bun.
