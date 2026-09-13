---
title: "Provisioning touches the issue bookmark only when it creates the workspace: resolve it to one commit first, add by commit id, create it only when nothing resolved"
category: legion
tags:
  - jj
  - workspace-provisioning
  - issue-bookmark
  - fetch-deletes-bookmark
  - silent-fallback
  - resolve-before-register
  - spec-rejected-list
date: 2026-09-13
status: active
module: packages/workspace
related_issues:
  - "sjawhar/legion#1023"
  - "sjawhar/legion#980"
---

# Provisioning touches the issue bookmark only when it creates the workspace: resolve it to one commit first, add by commit id, create it only when nothing resolved

`provisionIssueWorkspace` (`packages/workspace/src/workspace.ts`) runs on every spawn and every
resume of every role. Until #1023 it ended with `jj bookmark set legion/<KEY> --allow-backwards`
(no revision) in the workspace. That line is why LEGION-28's merged branch reappeared on
LEGION-56's commits, and why the live clone's `jj op log` showed a `point bookmark legion/<KEY>`
per resume — several per five minutes across issues. This document records the mechanism, the
contract that replaced it (spec v8), the failure shape both review-round edge cases shared, and
the alternatives the spec rejected so nobody re-proposes them.

## The "move" was a deletion followed by a re-creation

jj deletes a local bookmark when the remote branch it tracks disappears **and the two still point
at the same commit** — exactly the state of a merged pull request whose branch GitHub deleted.
Provisioning fetches first (`jj git fetch -R <clone>` prints
`bookmark: legion/<KEY>@origin [deleted] untracked`), so after a merge the local bookmark is gone
before the next line runs. The unconditional `bookmark set` that followed then *created* the
bookmark again at whatever `@` the workspace held. On LEGION-28 that was LEGION-56's follow-up
work in the same workspace (operations `be172183`, `e586a7f4`, 2026-09-13). Nothing pushed it only
because the implementer's push procedure names one bookmark; a bare `jj git push` would have
re-created the deleted remote branch at an unrelated head.

The rule that falls out: **any unconditional `jj bookmark set` that runs after a fetch re-creates
a merged issue's branch on unrelated commits.** A missing bookmark on an existing workspace stays
missing; the implementer's push step (`jj bookmark set legion/<KEY>` + `jj git push --bookmark`)
creates it when there is something to push.

## The contract (spec v8, `createWorkspace`)

Only `createWorkspace` touches the bookmark, and it runs only when the workspace directory does
not exist. In order:

1. **Resolve first, before any other command:**
   `jj log -r 'bookmarks(exact:legion/<KEY>)' --no-graph -T 'commit_id ++ "\n"' --ignore-working-copy -R <clone>`
   — one commit id per line. A nonzero exit or more than one line throws, naming the bookmark and
   the ids (`Bookmark legion/<KEY> is conflicted (<id>, <id>); workspace <dir> was not created.
   Resolve it with \`jj bookmark set legion/<KEY> -r <commit> -R <clone>\`.`), with **no prune, no
   add, nothing registered** — the failure repeats on every resume until a human resolves the
   bookmark. See the companion table in
   `jj-bookmark-facts-verified-on-0-44-0-and-0-45-1.md` for why this revset and not `present()` or
   `jj bookmark list`.
2. Exactly one commit is the add revision — `jj workspace add … --revision <commit id>`, **the id,
   never the name**. None means `main`.
3. `git worktree prune`, then the add. On `already registered|exists` (jj still registers the
   workspace but its directory is gone): flag-free `jj workspace forget <name> -R <clone>`, prune,
   the same add again at the same revision. A brand-new workspace and a forgotten registration
   start from the same resolution — the two paths no longer differ in where they start.
4. `jj bookmark set legion/<KEY> -r @` in the new workspace **only when nothing resolved**; one
   `console.error` line (`[legion] bookmark legion/<KEY> is gone (…); re-added the forgotten
   workspace <dir> at main, creating the bookmark on its fresh working copy`) **only when nothing
   resolved and the registration had to be forgotten** — a brand-new issue has no bookmark to miss
   and logs nothing.

An existing workspace gets `update-stale`, the fetch, the credential config writes, and nothing
with `bookmark` in it. The reactivation test's complete expected argv list is that check.

## The silent-fallback shape both review edge cases had

Both edge cases the round-1 reviewer found (COMMENT review, "spec-level, not blocking") had one
shape: **the first provision fails loudly, but it has already left something behind, and the next
resume — seeing the directory exists — skips creation and adopts it silently.**

- Spec v4–v6 said "any `jj bookmark list` stdout means the bookmark exists; add at the name."
  A conflicted bookmark (and a deleted local bookmark whose remote row survives) lists non-empty,
  but `jj workspace add --revision legion/<KEY>` for a name jj cannot resolve prints
  `Created workspace in …` and **registers the workspace, parented on the root commit
  `000000000000`, before** printing `Error: Name … is conflicted` / `Revision … doesn't exist`
  (both binaries). The next resume hands a worker an empty-tree workspace.
- Spec v4–v6 started a brand-new workspace at `main` even when its bookmark survived (a hand-run
  `jj workspace forget`, an operator wiping `workspaces/`). The add succeeded and the
  `bookmark set -r @` that followed refused (`Refusing to move bookmark backwards or sideways`);
  the next resume adopted the `main`-based workspace while the issue's work sat at the bookmark.

The fix for both is the same and is the general rule: **decide everything before the first
command with a side effect, and when the decision cannot be made, stop with nothing registered.**
A loud failure that leaves a partial state is a silent fallback one resume later. The real-jj
tests pin it by asserting, on two consecutive attempts, that the directory does not exist and
`jj workspace list` has no entry.

## Rejected — do not re-propose

From the spec's Rejected list (v8):

- Keeping the move but pinning `-r` to the remote branch head: a merged issue has no remote
  branch, and an in-progress issue's head is the worker's business.
- Creating a missing bookmark at the working copy on an existing workspace (the first draft's
  acceptance 2): after a merge the fetch deletes it, so this is the failure itself.
- Dropping provisioning's bookmark creation entirely: workable, but the recovery of a lost
  directory re-adds *at* the bookmark, so creating it with the workspace keeps that path
  meaningful.
- Treating any `jj bookmark list` output as "exists" and adding at the name (v4–v6): see above.
- Starting a brand-new workspace at `main` when its bookmark survived (v4–v6): see above.
- `present(legion/<KEY>)` as the resolution revset (v7's first candidate): exits 1 on a conflicted
  bookmark instead of listing its targets, so the spec's "more than one commit" branch would be
  unreachable in reality and defended only by the fake runner.
- `jj bookmark create` instead of `set` on the fresh path: `create` refuses a pre-existing local
  bookmark on a workspace jj does not know; `set` moves it forward or refuses sideways. The spec
  names `set`; with v8's resolve-first rule the set only ever runs when no bookmark exists.

## Checking it in production

The daemon's clone is `$LEGION_STATE_DIR/repos/github.com/sjawhar/legion`. A
`point bookmark legion/<KEY> to commit …` operation belongs in exactly two places: directly above
`create initial working-copy commit in workspace <key>` (an issue's first spawn, or a forgotten
registration re-created with no bookmark), or as a worker's own `jj bookmark set` shortly before
`push bookmark legion/<KEY> to git remote origin`. One per resume with no push after it is the
old behaviour. The resolution is a `--ignore-working-copy` read and records no operation at all.
`jj -R <clone> op log --no-graph --limit 200 -T 'time.start().format("%FT%TZ") ++ " " ++ description ++ "\n"'`
is the check; the out-of-suite driver shape the tester used (provision, `jj new` in the
workspace, provision again → `point bookmark` count 1 with the fix, 2 with the old file) is the
negative control.
