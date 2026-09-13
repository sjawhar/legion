---
title: "A release script that pushes after the publish already happened: resolve exactly one conflict shape, verify the state the shape implies, and route every failure through one sink"
category: github
tags:
  - github-actions
  - release-workflow
  - git-rebase
  - set-e
  - version-bump
  - code-review
date: 2026-09-13
status: active
module: .github/scripts/release-push.sh
problem_type: ci-correctness
component: .github/scripts/release-push.sh push-main
severity: medium
applies_when:
  - A CI step pushes a commit to a shared branch after artifacts are already published (npm, a tag, a GitHub release)
  - Two runs of the same job race on the same line of the same file and the loser must rebase
  - A script promises "every failure says X" and runs under `set -euo pipefail`
  - A script resolves a merge conflict automatically for one known shape
related_issues:
  - "LEGION-75"
  - "sjawhar/legion#1035"
---

# A Release Script That Pushes After the Publish Already Happened

`.github/scripts/release-push.sh push-main` is the last step of the Release Packages workflow: the
npm publish, the tag and the GitHub release exist, and the version-bump commit still has to reach
`main`. Two releases of the *same* package racing (the second run's checkout predates the first
run's bump commit, which is `[skip ci]` and never triggers a run) both rewrite the one `version`
line of the same `package.json`; the rebase conflicts on the first attempt and `set -e` killed the
loop before attempts two to five. The run was red, the package was shipped, and `main`'s manifest
lagged the tag (sjawhar/legion#1035; run 34741843259). Two review rounds taught the rules below.
The script and its test are the worked example.

## 1. One conflict shape is resolvable; the gate must check the shape AND the state it implies

The resolvable race is "the only conflicted path is the released package's manifest". The
first-round gate was exactly that:

```bash
conflicted=$(git diff --name-only --diff-filter=U)
if [ "$conflicted" != "$manifest" ]; then … abort and fail …; fi
```

`--diff-filter=U` lists unmerged paths one per line, so comparing the whole string to one path
is the exact-set rule: the manifest *plus* any other path fails the comparison (test Case 2b).
But the review found a second shape that produces the same one-line list: **modify/delete** —
main deleted the package directory while this run released it. The gate passed, and the resolver's
`git show "HEAD:$manifest"` died (`fatal: path … exists on disk, but not in 'HEAD'`), leaving the
rebase in progress. The proxy signal (the conflicted-path list) matched; the invariant it was
supposed to imply (main still has the file) did not hold. The gate now checks both:

```bash
if [ "$conflicted" != "$manifest" ] || ! git cat-file -e "HEAD:$manifest" 2>/dev/null; then
```

Whenever a script pattern-matches "the known-good case" through a proxy — a path list, an exit
code, a file count — ask which semantically different failure produces the same proxy value, and
check the state directly. The `2>/dev/null` on `cat-file` matters too: on the modify/delete shape
it prints its own `fatal:` line, and the test did not catch that (the test asserted the `::error::`
line and the exit code, not the absence of a `fatal:` before it); reading the by-hand
reproduction's full stderr did.

## 2. The rewrite base at a rebase stop is `HEAD:`, and the rewrite lands through a temp file

During a `git rebase` stop, `HEAD` is the upstream side (main, plus any commit already replayed)
and `REBASE_HEAD` is the commit being applied. Rewriting from `HEAD:$manifest` therefore keeps
every other change main made to the file and changes only the field being resolved:

```bash
git show "HEAD:$manifest" | jq --arg v "$version" '.version = $v' > "$manifest.tmp" \
  && mv "$manifest.tmp" "$manifest"
```

The first round wrote `> "$manifest"` directly. Bash opens the redirection before `git show` runs,
so any failure in the pipeline leaves a **0-byte manifest** in the working tree — that is how the
modify/delete case looked when reproduced. Temp file plus `mv` is the fix; the failure branch
`rm -f "$manifest.tmp"`s. The test's Case 1 now has main's bump also set `.description` and asserts
it on `origin/main` after resolution, so a regression to `REBASE_HEAD:` (which would silently drop
main's other edits) fails a check instead of passing because both sides started from one base.

Three facts about driving the rebase from a script, each proved on git 2.43 with the merge
backend and each load-bearing:

- `GIT_EDITOR=true git rebase --continue` — `--continue` opens an editor for the replayed commit's
  message; without a tty (Actions) it fails `There was a problem with the editor`. Not cargo cult.
- A resolution that leaves the commit empty (main already carries the identical bump, reachable
  because the workflow skips `npm publish` for an existing version) is dropped by `--continue`
  itself. No `--skip` branch is needed; the loop's next `git push` says `Everything up-to-date`.
- Best-effort cleanup is `git rebase --abort 2>/dev/null || true`. When the rebase failed before it
  started (a dirty tracked file), a bare `--abort` prints `fatal: No rebase in progress?` as the
  first thing an operator reads, ahead of the real `::error::` line.

## 3. "Every failure says the publish succeeded" means every exit path, and `set -e` hides exits

The script promises, in its header, that any failure says in the log **and** the job summary that
the artifacts, tag and release already exist and only this push is outstanding — so an operator
never has to decode a red run to learn whether the release happened. One function owns that
sentence:

```bash
fail_outstanding() {
  local reason=$1
  local message="$reason The release itself succeeded: … only this version-bump push to main is outstanding. …"
  echo "::error::$message"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '**Version bump for %s v%s did not land on main.** %s\n' "$package_dir" "$version" "$message" >> "$GITHUB_STEP_SUMMARY"
  fi
  exit 1
}
```

The first round routed the two failures it had thought about (five rejected pushes; a conflict
outside the manifest) and left every other command bare. Under `set -euo pipefail` a bare command
that fails **is an exit path**: `git fetch origin main` against an unreachable origin exited 128
with git's `fatal:` and nothing else, and so would `jq`, `git add` or `rebase --continue` inside
the resolver. The review found both. The audit that finds them: every command not already inside
an `if`, `&&`, `||` or a guarded `{ …; }` list is a place the promise is broken. The fix shape:

```bash
git fetch origin main || fail_outstanding "Fetching main from origin failed on attempt $attempt."
…
if ! { git show "HEAD:$manifest" | jq … > "$manifest.tmp" \
    && mv "$manifest.tmp" "$manifest" \
    && git add "$manifest" \
    && GIT_EDITOR=true git rebase --continue; }; then
  rm -f "$manifest.tmp"
  git rebase --abort 2>/dev/null || true
  fail_outstanding "Resolving the $manifest version-line conflict onto main ($upstream) failed; the rebase was aborted."
fi
```

The script writes `$GITHUB_STEP_SUMMARY` itself when the variable is set (GitHub exports it to
every `run:` step), so the workflow call sites stay one-liners and the exact summary text is
asserted by the test with the variable pointed at a temp file.

## 4. The resolver's safety rests on a contract the producer keeps, not one it checks

"Take main's copy, reset only `.version`" is safe because the bump commit that `release-push.sh
tag` creates touches only the manifest, and only its version line (the workflow writes the
manifest with the same `jq '.version = $v'`; pi_envoy's `.omp.extensions` rewrite is restored
before `tag`). Nothing at the point of use asserts that. If `tag` ever commits a second file — a
changelog — in the same commit, the resolver would still take main's copy of the manifest and
push, silently discarding nothing from the manifest but leaving the changelog to git's ordinary
conflict handling (it would conflict as a second path and abort, which is the safe outcome). The
mirror modify/delete shape — the *bump* commit deleting the manifest while main modified it — is
unguarded for the same reason: `tag` stages a manifest the version step just wrote, so the bump
commit is always a modification. Both live in the header comment as facts, not in code. When two
stages of one script share a contract about the shape of what one produces and the other consumes
destructively, write the contract down at the producing end; the consuming end's correctness is
invisible to a reader who sees only it.

Two more couplings a future editor of this script should know: `push-main`'s `rebase --continue`
creates a commit and therefore needs `user.name`/`user.email`, which only the `tag` case sets —
fine while the workflow always runs `tag` then `push-main` in one job, and caught by the guarded
list (reported as "resolving … failed", not as the real cause) if `push-main` is ever run alone;
and `fail_outstanding` reads `$manifest`, `$version`, `$package_dir` from the caller's scope, so
it cannot be reused from another subcommand without threading those. `git push origin main &&
exit 0` at the top of the loop relies on the `set -e` exemption for the left side of an `&&`
list; wrapping it in an `if` "for clarity" keeps the behaviour, but a refactor that splits it does
not.

## 5. Explicit arguments over reading state back

`push-main <package-dir> <version>` takes both explicitly, mirroring `tag <package-dir> …` and the
`${{ steps.version.outputs.version }}` every other step in the job already uses. Reading the
version back out of `HEAD`'s manifest would also work but hides the contract; explicit arguments
make the three call sites self-documenting and let the test pass the version directly.

## Related

- [bash-test-against-a-real-bare-git-origin](../testing/bash-test-against-a-real-bare-git-origin.md) —
  the test that locks all of the above: two clones racing one line, a rejecting `pre-receive` hook,
  hermetic git config, and the red-first numbers.
- [conflicting-pr-gets-no-pull-request-ci](conflicting-pr-gets-no-pull-request-ci.md) and
  [pull-request-trigger-paths-follow-the-pr-head](pull-request-trigger-paths-follow-the-pr-head.md) —
  other GitHub Actions behaviours that look like a script bug and are not.
- [worker-pane-shell-gotchas](../legion/worker-pane-shell-gotchas.md) — the `Tests` workflow
  re-runs on every PR body edit (`pull_request: types: [… edited …]`), so a round's body edit is
  one edit after its last push; filed as LEGION-90.
