---
title: "A bash harness that sources a copy of the script under test: pin the copy's root and run it from /tmp, put the fail-closed form on the success path too, prove 'never calls X' with a call log, and guard an absence check over a glob"
category: testing
tags:
  - bash-harness
  - set-e
  - fake-binaries
  - call-log
  - glob
  - cwd
  - smoke-rig
  - mutation-testing
date: 2026-09-13
status: active
module: scripts/smoke
applies_when:
  - A harness `source`s a stripped copy of the script it tests (`sed '$d' script > copy`)
  - A harness passes from the repository root and you have not run it from another directory
  - A success-path case calls the script's `main` through `if !`, `||`, or `&&`
  - A case claims some tool is never invoked, or that no file carries some line
related_issues:
  - "LEGION-71"
  - "sjawhar/legion#1029"
  - "sjawhar/legion#957"
---

# A bash harness that sources a copy of the script under test

`scripts/smoke/up.test.sh` tests `up.sh` by writing a copy without its last line (`main "$@"`)
and `source`-ing it. That shape has four failure modes that all look like a green run. Three
were found by the reviewer of sjawhar/legion#957 and fixed in #1029 (LEGION-71); the fourth
surfaced while fixing them. The method that finds every one of them is the mutation check in
`bash-harness-cases-that-pass-for-the-wrong-reason.md`: break the thing the case exists to
catch and watch the harness stay green.

## 1. The copy resolves its own root from the copy's location: pin it, then run from `/tmp`

`up.sh` starts with `repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"`. Sourced
from a `mktemp` copy, `BASH_SOURCE[0]` is `/tmp/tmp.XXXX`, so `repo_root` is `/` and every
repository path becomes `//packages/...`. The harness had passed 16/16 for weeks anyway — from
the repository root, because Bun resolves `//packages/daemon/src/daemon/omp-pin.ts` against the
cwd. From `/tmp` the same file printed `error: Module not found "//packages/daemon/src/daemon/omp-pin.ts"`
and 0 PASS. `checkpoints.test.sh` and `down.test.sh` never had the problem: they run their
scripts in place (`bash checkpoints.sh <n>`), not from a copy.

Pin the variable in the copy and prove the pin landed, in the same `sed` that strips `main`:

```bash
sed -e '$d' -e "s|^repo_root=.*|repo_root=\"${project_root}\"|" "$up_script" >"${source_dir}/up.sh"
grep -Fxq "repo_root=\"${project_root}\"" "${source_dir}/up.sh" || {
  printf 'fixture error: repo_root was not pinned in the sourced copy\n' >&2
  exit 1
}
```

The fixture grep matters: a later edit to `up.sh` that renames the variable would otherwise
turn the pin into a silent no-op and the harness back into a root-only harness. Rule: a
harness that sources a copy owns every path variable the copy derives from its own location,
and the harness is not green until it has passed from a directory that is not the repository
root (`cd /tmp && bash "$ws/scripts/smoke/up.test.sh"`). That is also the proof form the
tester quotes.

The related sibling problem — the copied script `source`s a file next to itself by
`BASH_SOURCE`, so the sibling must be copied beside the copy — is in
`smoke-rig-fakes-and-live-run-notes.md` (LEGION-40). The same fact breaks a
`bash -c 'source <(sed "\$d" up.sh); some_function'` one-liner used as a proof command:
under process substitution `BASH_SOURCE[0]` is `/dev/fd/63`, and the script fails with
`/dev/fd/dispatch-config.sh: No such file or directory`. Write proof commands the way the
harness does — a stripped copy in a `mktemp -d` with the sibling copied beside it — and never
rely on a one-liner that a sibling `source` will break later.

## 2. The fail-closed form belongs on the success-path calls, not only on the negative probe

`bash-harness-cases-that-pass-for-the-wrong-reason.md` §2 records the bash fact: `set -e` is
suppressed inside an `if` condition, so a probe that expects `main` to *fail* must run it as
`status="$(set +e; (set -e; main) >out 2>&1; echo $?)"`. #957 applied that to the negative
probe and left the three success-path calls as `if ! main >"$out" 2>&1; then fail; fi`. Those
looked harmless — they expect success — but the suppression cuts both ways: with `-e` off inside
the condition, a failure *anywhere inside* `main` (a `cd` to a missing directory, a stub that
exits 1, the `//packages` path above) does not stop it; `main` runs to the end, prints
`RIG READY`, exits 0, and the case passes. The control that shows it: make the fake `go` exit 1.
With the `if !` form the harness stayed 16/16 green; with the fail-closed form it stops at the
first success-path call, `expected up.sh main() to succeed on the installed pin (exit 1)`.

```bash
main_status="$(set +e; (set -e; unset LEGION_OMP_PATH; main) >"$main_output_file" 2>&1; echo $?)"
[[ "$main_status" == 0 ]] || {
  printf 'expected up.sh main() to succeed on the installed pin (exit %s); output:\n%s\n' \
    "$main_status" "$(<"$main_output_file")" >&2
  exit 1
}
```

Rule: every call of a `set -e` function from a harness — success path or failure path — goes
through the subshell-and-echo-status form. The `if !` form only ever proves that the function's
*last* command succeeded.

## 3. "Never invokes X" is a call log on the fake, never a PATH that happens to lack X

The case "a valid `LEGION_OMP_PATH` override never consults mise" was written as
`PATH="${fake_bin}/no-mise-here:/usr/bin:/bin" resolve_omp_path` and asserting the override
came back. That passes because `/usr/bin` and `/bin` lack `mise` on this machine — an
environment coincidence, not a property of the code; on a box with a system mise it proves
nothing, and it says nothing about *what* was called. The fake already on PATH is the right
instrument. Its first statement, before any subcommand check, appends `"$*"` to a log:

```bash
cat >"${fake_bin}/mise" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\${SMOKE_MISE_CALL_LOG:-/dev/null}"
[[ "\$1" == "where" ]] || { printf 'fake mise: unexpected subcommand %s\n' "\$1" >&2; exit 2; }
…
EOF
: >"$mise_call_log"
[[ "$(LEGION_OMP_PATH="$fake_omp" SMOKE_MISE_CALL_LOG="$mise_call_log" resolve_omp_path 2>&1)" == "$fake_omp" ]] || fail_case
[[ ! -s "$mise_call_log" ]] || {
  printf 'a valid LEGION_OMP_PATH must not invoke mise; calls:\n%s\n' "$(<"$mise_call_log")" >&2
  exit 1
}
```

Logging `$*` (not just the `where` argument) means the positive case can assert the exact call
too — `"$(<"$mise_call_log")" == "where ${omp_pin}"` — and the failure message names what was
called. Mutation: add `mise where "$omp_pin" >/dev/null 2>&1 || true` before the override
`return` in the script; the case fails naming `where <pin>`. The same principle in a TypeScript
test is `fake-cli-on-path-outputs-from-files-and-a-call-log.md` §2–3.

## 4. An absence check over a glob needs an existence guard for what the glob should match

"No `start_process` argv carries `LEGION_OMP_PATH`" was
`if grep -l '^LEGION_OMP_PATH=' "${SMOKE_DIR}"/start_process.*.argv 2>/dev/null; then fail; fi`.
If `main` had recorded no argv files at all, the glob stays literal, `grep` fails on a missing
file, its complaint goes to `/dev/null`, and the absence is confirmed — vacuously. Assert first
that the files the case is about exist, then run the grep without the silencer:

```bash
for name in listener daemon envoy-bridge; do
  [[ -f "${SMOKE_DIR}/start_process.${name}.argv" ]] || {
    printf 'fixture error: main() recorded no start_process argv for %s\n' "$name" >&2
    exit 1
  }
done
if grep -l '^LEGION_OMP_PATH=' "${SMOKE_DIR}"/start_process.*.argv; then …; fi
```

Mutation: delete the stub's argv write; the fixture error fires where a silent pass used to.
Rule: a negative assertion ("nothing matches") is only as strong as the positive assertion that
the population it searches is non-empty and is the population you meant.

## Related

- `bash-harness-cases-that-pass-for-the-wrong-reason.md` — the mutation method, one guard per
  case, the negative probe's `set -e` form, and the "PASS line names only what ran" rule.
- `smoke-rig-fakes-and-live-run-notes.md` — the sibling-`source` copy (LEGION-40) and the rig's
  other fakes.
- `fake-cli-on-path-outputs-from-files-and-a-call-log.md` — call logs and "never spawns" markers
  for fakes driven from Bun tests.
- `../best-practices/pass-the-tools-stderr-through-and-classify-before-naming-a-remedy.md` — the
  `up.sh` change these cases pin.
