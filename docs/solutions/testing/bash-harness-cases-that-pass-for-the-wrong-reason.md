---
title: "Bash harness cases that pass for the wrong reason: isolate one guard per case, assert the success-only side effect is absent, and give a group-kill fixture a second process"
category: testing
tags:
  - bash-harness
  - mutation-testing
  - set-e
  - process-group
  - fake-binaries
  - smoke-rig
  - code-review
date: 2026-09-13
status: active
module: scripts/smoke
problem_type: testing
component: scripts/smoke/*.test.sh
severity: medium
applies_when:
  - Writing a bash harness case for a function with several guards (`[[ a && b && c ]] || fail`)
  - Proving that a failed command substitution stops a `set -e` script
  - A fixture stands in for a process a teardown must kill as a group
  - A PASS line names more cases than the code above it runs
related_issues:
  - "LEGION-10"
  - "sjawhar/legion#957"
---

# Bash Harness Cases That Pass for the Wrong Reason

Three new cases in `scripts/smoke/*.test.sh` were green, read correctly, and proved nothing.
The reviewer of sjawhar/legion#957 found each the same way as in
`docs/solutions/testing/mutation-proof-probe-tests.md`: mutate the script into the plausible
bug the case exists to catch and watch the harness stay green. In bash this happens more
easily than in a typed test runner, because a failing guard, a failing `-f`, a failing `cd`
and a masked substitution all look like "non-zero exit" to an `if` or `||`.

## 1. One guard per case: every other guard must already be satisfied

`resolve_omp_path` in `up.sh` rejects `LEGION_OMP_PATH` unless
`[[ "$p" == /* && -f "$p" && -x "$p" ]]`. The first harness fed it `relative/omp` (does not
exist) and `${fake_bin}/missing-omp` (does not exist). Both failed on `-f`. Deleting the
leading-slash test left all fourteen cases green; deleting both `-x` tests did too. The
harness could not see either regression, and both are real: a relative or non-executable
path would have reached the daemon and failed after NATS and the listener were already up.

The fix is one case per guard, each arranged so the other guards pass:

```bash
# leading slash: an existing, executable file reached by a relative path
(cd "$fake_bin" && LEGION_OMP_PATH="omp" resolve_omp_path)      # must fail
# execute bit: an existing absolute file with no -x
chmod -x "$fake_omp_noexec"; LEGION_OMP_PATH="$fake_omp_noexec" resolve_omp_path
```

Rule: for `[[ g1 && g2 && g3 ]] || fail`, write one negative case per `gN` whose fixture
satisfies every other `gM`. Then delete each `gN` in turn and confirm exactly its case fails.
`sed -i` on the script, run the harness, `jj restore` the script, run it again green.

## 2. A `set -e` probe needs the success-only side effect to be absent

`main()` does `omp_path="$(resolve_omp_path)"`, which under `set -e` stops the script when
the function fails. The harness probe ran `main` with a bad path and asserted: non-zero exit,
the error text present, and an empty process-start log. The reviewer changed the line to
`local omp_path="$(resolve_omp_path)"` — the classic bug where `local`'s own zero status hides
the substitution's failure from `set -e` — and the probe still passed. `main()` continued,
printed `GREEN OMP build: ` with an empty path, and then died at `cd "${repo_root}/packages/envoy"`
because the harness sources a `mktemp` copy of the script and `repo_root` resolves to `/`.
Every assertion held for an unrelated reason.

The missing assertion is positive knowledge about the success path: `GREEN OMP build:` is
printed only after `resolve_omp_path` returned 0, so it must be absent from the probe's output:

```bash
[[ "$(<"$assertion_file")" != *'GREEN OMP build:'* ]] || fail_case
```

Two bash facts to keep in mind when writing the probe itself:

- Bash disables `set -e` inside an `if` condition, so `if (main); then …` lets a failed
  substitution continue. Run the probe as a plain subshell that re-enables `-e` and report its
  status through a substitution: `status="$(set +e; (set -e; main) >out 2>&1; echo $?)"`.
- `local x="$(cmd)"` is the same masking bug in production code. Keep `local x` and
  `x="$(cmd)"` on separate lines wherever the failure must stop the script.

Rule: a "this stops before doing X" probe asserts that X's own unmistakable output did not
happen, not only that something failed afterwards.

## 3. A group-kill fixture needs a second process

`down.sh` kills the recorded `gh webhook forward` process group with `kill -- "-$pgid"`. The
first stand-in was `setsid bash -c '…; exec sleep 300'`: one process, which is its own group.
A `down.sh` mutated to `kill "$pgid"` (pid only) passed the case, because with one member
there is no survivor to observe. In production the group is `gh` plus its `gh-webhook`
extension child; a pid-only kill leaves the child relaying events to a listener that no
longer exists — the regression the group kill exists to prevent.

The stand-in is now a two-process group, a shell waiting on its child:

```bash
setsid bash -c 'printf "%s\n" "$$" >"$1"; sleep 300 & wait' _ "$forward_pid_file" &
forward_job_pid="$!"
```

With that, the pid-only mutant fails: `kill -0 -- "-$pgid"` still succeeds and the failure
message lists the surviving `sleep`. Two safety rules that came with it:

- The stand-in reports its own pid from inside the new session and the harness refuses to
  continue unless `/proc/<pid>/stat` field 5 (pgid) equals that pid. Reading `/proc/$!/stat`
  right after `setsid … &` races the `setsid()` call; an early read returns the harness's own
  group, which `down.sh` would then kill — the harness, and possibly the operator's shell.
- Record `$!` immediately and kill it in the `EXIT` trap. A fixture failure before the
  leader check otherwise leaves a `sleep 300` running for five minutes.

Rule: to prove "the whole group died", the group must contain something other than its
leader, and the assertion must probe the group (`kill -0 -- -pgid`), never the leader's pid.

## 4. A PASS line may only name what ran

A block ran checkpoint 5 under `none` mode and then printed
`PASS: none mode blocks GitHub-fed checkpoints 5-7 and 9-11 …`. Removing `10` from the
`5 | 6 | 7 | 9 | 10 | 11` arm in `checkpoints.sh` went unnoticed; adding `8` to it did too.
Loop over the set the label claims, and add the boundary case (checkpoint 8 under `none`
must report its own branch-protection reason and no ingress reason at all). An overclaiming
label is a coverage gap even when no mutation is tried.

## Running the mutations in a bash rig

Same discipline as the daemon's TypeScript tests, in shell:

```bash
cp scripts/smoke/up.sh /tmp/o-up.sh
sed -i 's/\[\[ "\$LEGION_OMP_PATH" == \/\* \&\& -f/[[ -f/' scripts/smoke/up.sh
bash scripts/smoke/up.test.sh        # expect: exactly the leading-slash case fails
cp /tmp/o-up.sh scripts/smoke/up.sh && cmp /tmp/o-up.sh scripts/smoke/up.sh
```

Check that the `sed` changed something (`cmp` against the copy) — a mutation that did not
apply is a false "caught". Record each mutation's first failing assertion, not just "fails".
On #957 the eight mutations (three guards, the `local` mask, the pid-only kill, drop-a-checkpoint,
add-checkpoint-8, restore-the-mode-guess) each named their own case.

## Related

- `docs/solutions/testing/mutation-proof-probe-tests.md`: the same method on the daemon's
  TypeScript tests; the prefix-chain and contract-not-cadence corollaries.
- `docs/solutions/testing/smoke-rig-fakes-and-live-run-notes.md`: the rig's fake `tmux`, real
  processes for `/proc` checks, and the whole-log assertion rule.
