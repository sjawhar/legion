---
title: "A bash test against a real bare git origin: two clones race one line, a pre-receive hook forces rejection, ambient git config is fenced off, and the red run is quoted before the fix"
category: testing
tags:
  - bash-harness
  - git
  - bare-origin
  - pre-receive
  - hermetic
  - test-driven-development
  - release-workflow
date: 2026-09-13
status: active
module: .github/scripts/release-push.test.sh
problem_type: testing
component: .github/scripts/release-push.test.sh
severity: medium
applies_when:
  - A script drives real `git push` / `fetch` / `rebase` against a remote and its retry or conflict logic needs a deterministic test
  - Two writers must race on the same line of the same file
  - A push must be rejected N times on purpose
  - A bash test shells out to git and must not inherit the developer's `~/.gitconfig`
  - A test is written before the fix and its failing output is part of the evidence
related_issues:
  - "LEGION-75"
  - "sjawhar/legion#1035"
---

# A Bash Test Against a Real Bare Git Origin

`.github/scripts/release-push.test.sh` locks the behaviour of `release-push.sh push-main`
(sjawhar/legion#1035): a version-bump push that rebases onto `main` between attempts and resolves
one conflict shape. The script sees only exit codes from `git push`, `git fetch`, `git rebase`,
`git show`, so a test that runs those same binaries against a `file://` origin exercises the same
code path as the Actions step; only the transport differs. It needs git ≥ 2.32 and `jq`, runs in
about one second on the runner, and is a step of the `Tests` workflow right after checkout (before
Bun setup, since it needs neither). Seven cases, thirty-three checks. The patterns:

## 1. Bare origin plus two clones at different starting points

```bash
setup() {
  local dir=$1
  mkdir -p "$dir/seed/packages/pkg"
  git init -q -b main "$dir/seed"          # git ≥ 2.28 for -b
  … write the seed manifest at 1.0.0 and a README, commit …
  git clone -q --bare "$dir/seed" "$dir/origin.git"
  git clone -q "$dir/origin.git" "$dir/a"
  git clone -q "$dir/origin.git" "$dir/b"
}
```

Clone `a` plays the run whose bump commit already landed on `main`; clone `b` plays the next run
of the same package, whose checkout predates that bump. `a` bumps and pushes; `b` bumps from the
seed and runs the script under test. `b`'s push is rejected, its fetch sees `a`'s commit, and its
rebase meets `a`'s change on the same JSON line — the real two-CI-jobs-at-different-times topology,
not a conflict authored by hand in one working tree. Every case gets its own `setup` under one
`mktemp -d` that a `trap 'rm -rf "$work"' EXIT` removes; `ls /tmp | grep -c '^tmp\.'` before and
after two runs proves nothing is left behind.

Each case is a different shape of what `a` did before `b` arrived: the same manifest line (Case
1); a different file (Case 2); the manifest and a different file (Case 2b); a different package's
manifest (Case 4); deleting the package (Case 6, modify/delete). The assertions read state from
the bare origin, never from a clone: `git -C origin.git show main:<path> | jq -r .version`,
`git -C origin.git log --format=%s main | grep -c '^chore: release'`.

## 2. A pre-receive hook that always rejects forces exhaustion

```bash
printf '#!/bin/sh\necho "rejected by test hook" >&2\nexit 1\n' > "$dir/origin.git/hooks/pre-receive"
chmod +x "$dir/origin.git/hooks/pre-receive"
```

Case 3 needs five rejected pushes without anything to rebase onto. The hook is the origin refusing
under its own rules, exactly as branch protection would; git itself is never stubbed. The count is
asserted from the script's captured output — `grep -c 'rejected by test hook'` equals 5 — which
also proves the loop reached attempt five instead of dying earlier. Pointing the clone's `origin`
at a path that does not exist (`git remote set-url origin "$dir/nowhere.git"`, Case 5) is the
unreachable-remote shape: the push fails, then the fetch fails, and the fetch failure is the one
under test.

## 3. Fence off the developer's git config

```bash
set -euo pipefail
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
```

Without this the seed repos and the script under test inherit `~/.gitconfig` and `/etc/gitconfig`:
`commit.gpgsign=true` fails the seed commit before Case 1 (`fatal: failed to write commit object`),
`core.hooksPath` runs someone's hooks, `rerere.enabled` can silently auto-resolve the very conflict
a case exists to exercise, and `rebase.*` changes the backend. Identity is then set per clone
(`git -C "$clone" config user.name …`), which is also what the script under test's `tag`
subcommand does in production. CI runners are clean either way; the developer's box is where this
bites, and "run from anywhere" in the header is only true with the fence.

## 4. Capture output and exit code without tripping `set -e`

```bash
push_main() {
  local clone=$1 v=$2 summary=${3:-}
  set +e
  out=$(cd "$clone" && GITHUB_STEP_SUMMARY="$summary" "$release_push" push-main packages/pkg "$v" 2>&1)
  rc=$?
  set -e
}
```

`out` and `rc` are then asserted separately: `[ "$rc" != 0 ]`, `grep -q '^::error::…'` on the
output, `cat "$summary"` for the job-summary file (the script writes `$GITHUB_STEP_SUMMARY` when
the variable is set, so the test points it at a temp file and asserts the exact sentence), and
`[ ! -d "$clone/.git/rebase-merge" ] && [ ! -d "$clone/.git/rebase-apply" ]` for "not left
mid-rebase". A failure-path case asserts the working-tree file the script rewrites is intact
(`jq -r .version` on it), because the first version of the script truncated it to 0 bytes on the
way to failing.

One thing the test did **not** catch and the by-hand reproduction did: a stray `fatal:` line
printed *before* the `::error::` line (from `git cat-file -e` on the modify/delete shape). The
assertions matched the `::error::` line and the exit code; nothing asserted "no `fatal:` before
it". When the contract is "the first thing an operator reads is the annotation", assert on the
whole output's first error-ish line, or at least read the by-hand run's full stderr once.

## 5. Write the test first and quote the red run

The plan predicted the failing line for the test against the pre-fix script — `Results: 10 passed,
12 failed`, with Case 1 failing all five checks because the old `git pull --rebase` died under
`set -e` and left the clone mid-rebase — and the run matched it exactly. After the fix: `Results:
22 passed, 0 failed`. In the review round the two new cases were run against the round-1 script
first: `Results: 27 passed, 6 failed`, the six failures being precisely the reviewer's two
reproductions (three checks each). Then `33 passed, 0 failed`. Those red numbers are the evidence
that each case defends its contract rather than passing for an unrelated reason
([bash-harness-cases-that-pass-for-the-wrong-reason](bash-harness-cases-that-pass-for-the-wrong-reason.md));
they belong in the PR body's `E2E` line next to the green ones and the CI run that produced the
same green line on the runner.

## Related

- [release-bump-push-resolves-one-conflict-shape-and-routes-every-failure-through-one-sink](../github/release-bump-push-resolves-one-conflict-shape-and-routes-every-failure-through-one-sink.md) —
  the script this test locks, and the rules its two review rounds taught.
- [race-regression-tests-that-fail-before-the-fix](race-regression-tests-that-fail-before-the-fix.md) —
  the same red-first discipline for TypeScript races.
- [fake-cli-on-path-outputs-from-files-and-a-call-log](fake-cli-on-path-outputs-from-files-and-a-call-log.md) —
  when the binary *should* be stubbed; here git is the thing under test and is not.
