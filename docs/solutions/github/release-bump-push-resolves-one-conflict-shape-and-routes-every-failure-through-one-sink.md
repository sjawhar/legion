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

## 6. A `pull_request` run tests the PR's cached potential merge commit, and only a head push refreshes it

A `pull_request`-triggered run does not check out the PR head. `actions/checkout` fetches
`github.sha` **by sha**, and for `pull_request` events that is the PR's *potential merge commit*
— GitHub's test merge of the head into the base, cached on the PR (`merge_commit_sha` in REST,
`potentialMergeCommit` in GraphQL). The run's log says so:
`HEAD is now at 5bc2afa Merge ac002051… into 0611be6b…`. When `main` was red at the moment that
merge was computed, the PR's checks are red for a failure the branch does not contain, and two
remedies that look right do nothing:

- `gh run rerun <run-id> --failed` re-runs the **same run**, whose event payload still names the
  same merge sha; the rerun fetches `5bc2afa` again and fails identically.
- A new `pull_request` event that does not move the head (`edited`: a title or body change) fires
  a **new run**, but its payload copies the PR's cached `merge_commit_sha` — GitHub had not
  recomputed it, so the new run checked out the same `5bc2afa`.

GitHub recomputes the potential merge commit lazily. On sjawhar/legion#1035 it stayed pinned to a
base three `main` pushes old for more than ten minutes of REST and GraphQL polling (`mergeable:
true`, `MERGEABLE` — cached, with no recompute pending). The one event that certainly refreshes
it is a push to the PR head (`synchronize`): GitHub recomputes the merge for the new head, and the
run that fires tests it against the `main` of that moment.

Today's evidence. Retro head `ac002051` (approved `7097b62b` plus `docs/solutions/` only) got
`Tests` run 34769469580, red on `typecheck` alone:
`real-prompt-delivery-e2e.test.ts(293,5): Property 'baseEnv' is missing … WorkerCatchupDeps` — a
file the branch never touches. Its merge base `0611be6b` (`chore: release cli v1.9.4`) had
`catchup.ts` requiring `baseEnv` and the test file lacking it; `main` was red there and #1045
(`8af58deb`) fixed it eight minutes later, green in run 34769578687. The rerun (attempt 2) and the
`edited`-triggered run 34769828557 both checked out `5bc2afa` and failed on the same line. What
finally produced a run against current `main` was this section's own commit — the one docs-only
push the retro rule allows above an approved head, which carries a real learning and refreshes
the merge commit as a side effect.

So, when a PR is red on a required check in a file the branch does not touch: read the run's
`HEAD is now at <merge> Merge <head> into <base>` line, check whether `main` was red at `<base>`
(`gh run list --branch main` around that time), and if so do not rerun and do not poke the body —
push the next real commit, or, on a branch that must not move, ask for one.

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
