---
title: "Rebasing an issue branch across a refactor of the code it changed: re-anchor the invariant, not the lines"
category: legion
tags:
  - legion
  - jj
  - rebase
  - conflict-resolution
  - runtime-boundary
  - processes
  - github
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/processes.ts
related_issues:
  - "LEGION-17"
  - "sjawhar/legion#956"
  - "LEGION-21"
  - "sjawhar/legion#962"
symptoms:
  - "GitHub reports a pull request CONFLICTING/DIRTY and schedules no pull_request workflow for the new head"
  - "jj rebase reports the same file conflicted in every commit of the chain, from the first code commit to the last handoff"
  - "a conflict hunk deletes the method the branch edited and the branch's change has nowhere to land"
  - "tsc passes on the resolved source but fails in a test fixture that was never in conflict"
---

# Rebasing an issue branch across a refactor of the code it changed

PR #956 (LEGION-17) was rebased onto `main` twice in one round. The second rebase crossed 61
commits, including LEGION-21's runtime boundary (#962), which had rewritten the exact region of
`processes.ts` this branch had changed. Nothing about jj was hard; what was hard was deciding
what "keep this PR's behaviour" means when the lines it changed no longer exist. The rules below
are what that took.

## 1. Know the invariant before you open the first conflict

This branch's contract in `processes.ts` was a sentence, not a diff: *one builder,
`systemPromptArguments`, produces the ordered `--append-system-prompt` fragments for every launch
site, so the sites cannot drift*. Before the rebase there were two sites, `launchShimmedProcess`
and `spawnController`. After #962, `launchShimmedProcess`, `shimmedShellCommand`, and
`prepareSocket` were gone (the shim command is now built in `runtime-tmux.ts`), `shellPath` had
moved to `./runtime`, and the OMP command for roots and workers was built in a new
`issueInnerCommand`. jj's hunk for the deleted block offered only "keep my version of a method
that no longer has a caller" or "drop my change".

Neither is right. Resolve by re-anchoring: find every place on the new `main` that builds the
launch command (`grep append-system-prompt` — two hits, `issueInnerCommand` and `spawnController`),
route each through the builder, delete the branch's now-duplicate `shellPath`, and reword the doc
comments to name the new sites. Then prove the invariant held with the same grep: exactly two
`systemPromptArguments(` call sites, zero hand-built `--append-system-prompt` strings.

## 2. Expect the conflict everywhere, resolve it once, bottom-up

jj marks every descendant of a conflicted commit as conflicted, so a 3-commit change with 8
handoff commits above it shows 11 red rows. Only the first commit that touched each file needs a
decision: `jj new <that commit>`, edit the file, `jj squash`. jj then auto-rebases the descendants
and the same conflict disappears from all of them. Check with `jj log -r 'conflicts() & (main::@)'`
after each squash; it should shrink, never grow. Two files here — `AGENTS.md` tables where both
sides added a row after the same anchor — needed only "keep both rows", and the docs table order
was chosen to match `main`'s.

## 3. Type-check the fixtures the conflict did not touch

The E2E test had no textual conflict, but `bunx tsc --noEmit` failed in it: `DaemonConfig` had
gained five fields, `ProcessManagerDeps.panePath` had become `processPath` plus a required
`runtime`, and `Locator` had become a `runtime`-tagged union. The fixture was adapted by copying
the shape its sibling (`real-shutdown-e2e.test.ts`) already used on `main` — a real `TmuxRuntime`
over the test's own `run`, `locatorsForIssue`, and `toMatchObject({ runtime: "tmux", tmuxSession })`
— rather than inventing a second way to build a `ProcessManager` in tests. Run tsc and the affected
test files before pushing, not just the conflicted ones.

## 4. Merge state comes from GitHub, not from jj

GitHub does not run `pull_request` workflows for a head it cannot merge. The first push after the
rebase still showed `CONFLICTING`/`DIRTY` because `main` had advanced three more commits (a fix and
two releases) between the fetch and the push, and one of them edited the row next to this branch's
row in `packages/pi-envoy/AGENTS.md`. `git merge-tree --write-tree --name-only <main> <head>` on the
jj-backed git repository reproduces GitHub's answer locally in a second and names the file; use it
before asking GitHub. Fetch, rebase once more, resolve, push, then poll `gh pr view --json
mergeable,mergeStateStatus` until it leaves `UNKNOWN`. Release commits touch `package.json` files
only and never conflict; a rebase over them is safe to do without re-running the gate, but the gate
is cheap enough (40 s here) that it was re-run anyway.

## 5. jj housekeeping specific to a rebased worker branch

- Before LEGION-58 the daemon-provisioned `.omp/config.yml` lived in the empty working-copy
  commit. Two rebases of the chain made that change divergent (two empty siblings of the head).
  Neither is under the bookmark and neither holds tracked content; `jj edit <one of them>` puts the
  working copy back on the tip. The worker skill forbids `jj abandon`, so leave the other in place
  and note it in the handoff.
- After every `jj split` or `jj squash` that rewrites the head, re-run
  `jj bookmark set legion/<KEY> -r @- --allow-backwards` before pushing; the bookmark otherwise
  points at the undescribed working-copy commit, which `jj git push` refuses.
- A reviewer's locally committed handoff sits above origin in the shared workspace (the review App
  cannot push). It rides along on the implementer's next push; confirm it is an ancestor of your
  commit with `jj log` before building on it, and mention it in the push summary.

## 6. `jj diff --from main --to <branch>` is not the pull request's diff once `main` moves again

During this issue's retro, a fresh-eyes scout read `jj diff --from main --to legion/LEGION-17`, saw
`omp-pin.ts` and `package.json` "regress" and three unrelated `docs/solutions` files "deleted", and
concluded that merging would revert a fix `main` already carried. It would not. `main` had gained two
commits (an OMP-pin bump and its release) after the branch's last rebase, and that command shows the
*symmetric* difference between two heads — every change on either side since they diverged — not the
branch's own contribution. The pull request's diff is against the merge base:
`jj diff --from <the commit the branch was rebased onto> --to <branch>` (22 files here, none of the
five `main` had changed), or GitHub's own file list (`gh pr view --json files`), which is what the
reviewer and the merge queue see. Check `jj log -r '::main ~ ::<branch>'` first: if it is non-empty,
read the diff from the branch's base, not from `main`. A branch that is behind `main` with no
overlapping files is `MERGEABLE` and needs no rebase for the merge queue to take it.

## Related

- [handoff-file-conflicts-during-rebases](handoff-file-conflicts-during-rebases.md) — the
  edit-and-squash, bottom-up mechanics, for the case where the conflicting file is a `.legion/`
  handoff and one side is simply right.
- [conflicting-pr-gets-no-pull-request-ci](../github/conflicting-pr-gets-no-pull-request-ci.md) —
  why a `CONFLICTING` head has no CI to wait for.
- [proving-a-pure-refactor-behind-an-interface](../architecture-patterns/proving-a-pure-refactor-behind-an-interface.md)
  — the other side of this rebase: how #962 proved the runtime boundary was behaviour-preserving.
