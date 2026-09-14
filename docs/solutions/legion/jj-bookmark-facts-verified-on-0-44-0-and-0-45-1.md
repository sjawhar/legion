---
title: "jj bookmark facts verified on 0.44.0 and 0.45.1: a fetch deletes a matching local bookmark, three ways to read a bookmark disagree on conflicted and deleted ones, a bookmark on an unsnapshotted @ moves with the next snapshot"
category: legion
tags:
  - jj
  - jj-0.44
  - jj-0.45
  - bookmarks
  - revsets
  - present
  - bookmarks-exact
  - conflicted-bookmark
  - fetch
  - snapshot
  - update-stale
  - workspace-add
  - workspace-forget
  - workspace-list
  - abandon
  - worktree-prune
  - workspace-removal
date: 2026-09-13
status: active
module: packages/workspace
related_issues:
  - "sjawhar/legion#1023"
  - "LEGION-104"
  - "sjawhar/legion#1087"
---

# jj bookmark facts verified on 0.44.0 and 0.45.1: a fetch deletes a matching local bookmark, three ways to read a bookmark disagree on conflicted and deleted ones, a bookmark on an unsnapshotted @ moves with the next snapshot

Everything below was reproduced on 2026-09-13 on both binaries this repository runs: this box's
`jj 0.45.1-sami.20260910` and stock `jj 0.44.0` (`mise x github:jj-vcs/jj@0.44.0 -- jj`, the CI
version). Every row was **identical on both** unless a cell says otherwise. Construction recipes
are at the end so a future worker can re-verify on a new pin instead of trusting this page.

## A fetch deletes a tracked local bookmark whose remote branch is gone

`jj git fetch` on a clone whose `legion/X` tracks `legion/X@origin`, after the remote branch was
deleted (what GitHub does at merge; `git --git-dir=<remote>/.git branch -D legion/X` in a test):

```
bookmark: legion/X@origin [deleted] untracked
```

and the local bookmark is gone — **only while the local bookmark still points where the remote
did.** If the local target has moved (a later snapshot, see below), jj keeps the local bookmark
and only drops the remote row. Consequence: anything that runs after a fetch and assumes the
bookmark is still there is wrong after every merge.

The same fetch also **abandons the branch's commits that no other bookmark reaches, even under a
live workspace's working copy** (`Abandoned 1 commits that are no longer reachable … Rebased 1
descendant commits`) and rebases that working copy onto the fork point; the workspace is stale
(`The working copy is stale (not updated since operation …)`) until `jj workspace update-stale`,
which then removes the abandoned commits' files. Pre-existing and independent of the bookmark rule;
decided in LEGION-84: before every fetch, provisioning reads `jj config get
git.abandon-unreachable-commits -R <clone>` and, only when that does not print `false`, runs `jj
config set --repo git.abandon-unreachable-commits false -R <clone>`; the fetch then deletes the
bookmark and nothing else (`jj op log --op-diff` shows only the local and remote bookmark sections).
Three more facts, both binaries: `config get` and `config set --repo` each record no operation and
do not snapshot the clone's working copy; `config set` rewrites the file in place (same inode), so
the read-first guard is what keeps the steady state from writing a file every jj command parses; and
a repo's settings live at `~/.config/jj/repos/<config-id>/config.toml` (`$XDG_CONFIG_HOME`, the id
from `<clone>/.jj/repo/config-id`), not inside the clone — under another `HOME` the same `jj config
get` prints the default `true` with `Per-repo config not found. Generating an empty one.`

## Three ways to read one bookmark, five states

Command forms, run from the clone with `--ignore-working-copy -R <clone>`:

- **list**: `jj bookmark list legion/X`
- **present**: `jj log -r 'present(legion/X)' --no-graph -T 'commit_id ++ "\n"'`
- **exact**: `jj log -r 'bookmarks(exact:legion/X)' --no-graph -T 'commit_id ++ "\n"'`

| state of `legion/X` | list | present | exact |
| --- | --- | --- | --- |
| missing | exit 0, **no stdout**, stderr `Warning: No matching bookmarks for names: legion/X` | exit 0, 0 lines | exit 0, 0 lines |
| only `legion/X1` exists | as missing (positional name is exact, not a prefix) | 0 lines | 0 lines (`exact:` is exact) |
| normal | exit 0, one row `legion/X: <change> <commit> …` | exit 0, **1 line**, the commit id | exit 0, **1 line**, the commit id |
| local deleted, `@origin` row survives | exit 0, **2 rows**: `legion/X (deleted)` + `  @origin: …`, stderr `Hint: Bookmarks marked as deleted can be *deleted permanently* …` | exit 0, 0 lines | exit 0, 0 lines |
| conflicted (two targets) | exit 0, **5–6 rows**: `legion/X (conflicted):`, `  - <base>`, `  + <A>`, `  + <B>`, stderr `Hint: Some bookmarks have conflicts …` | **exit 1**, 0 lines, stderr `Error: Name \`legion/X\` is conflicted` + `Hint: Use commit ID to select single revision from: <A>, <B>` + `Hint: Use \`bookmarks(legion/X)\` to select all revisions` | exit 0, **2 lines**, one commit id each |

What that means for a caller that needs "exactly one commit or stop":

- **`list` stdout is not "the bookmark exists."** Both the deleted-with-remote-row and the
  conflicted states print rows, and neither can be used as a `--revision`. Spec v4–v6 of #1023 made
  this mistake.
- **`present()` cannot tell conflicted from broken.** It collapses the conflicted case into exit 1
  with the answer only in a stderr hint, so a "more than one commit" branch keyed on it is dead
  code. Fine when you only need present/absent of a *healthy* name.
- **`bookmarks(exact:name)` lists every local target and exits 0**, so the caller's three
  branches — none / one / several — are all real paths. jj's own conflict hint names
  `bookmarks(<name>)` as the way to select all targets. This is what `createWorkspace` uses.

## `jj workspace add --revision <name>` registers before it errors

For a name jj cannot resolve to one commit — missing, or conflicted — both binaries print
`Created Git worktree for the new workspace.` / `Created workspace in "…"` **and then**
`Error: Revision \`legion/X\` doesn't exist` or `Error: Name \`legion/X\` is conflicted`, exit 1,
leaving the workspace **registered** (`jj workspace list` shows it) with its **directory created**
and its working copy parented on the root commit `000000000000`. Resolve to a commit id first and
add by id; never let `add` be the thing that discovers the name is bad.

## `jj workspace forget` takes only workspace names

`jj workspace forget [WORKSPACES]...` — no `--cleanup`, no `--force`, on 0.44.0 and 0.45.1 alike
(`error: unexpected argument '--cleanup' found`, exit 2). `git --git-dir=<clone>/.git worktree
prune` after it is what cleans the colocated worktree. The pre-#1023 code passed both flags and its
recovery branch had never once completed on a real binary; see the testing doc for why no test
saw it.

## A bookmark on an unsnapshotted `@` moves with the next snapshot

Provisioning creates the bookmark with `jj bookmark set legion/X -r @` on the fresh working copy,
then writes `.omp/config.yml` into it. The next jj command in that workspace snapshots the file,
rewriting `@`, and **the bookmark moves with the rewrite** (its target is `@`, and `@` was
rewritten in place). Two consequences:

1. A test of the fetch-deletion rule must make the pushed commit the bookmark's *final* target:
   snapshot (`jj status` in the workspace) or advance (`jj new`) **before** pushing, or the later
   snapshot moves the local bookmark off the pushed commit and the fetch keeps it (rule above).
   Real workers always leave `@` above the pushed head, so the snapshot step is also the
   production shape, not a test artefact.
2. `jj workspace update-stale` loads the repo **at the workspace's last-recorded operation** and
   snapshots there — a divergent operation — before `Concurrent modification detected, resolving
   automatically`. If the bookmark sat on the unsnapshotted `@` at that recorded operation and the
   main operation line has since moved or deleted it, the reconciliation **conflicts** the
   bookmark (`- <base> + <A> + <B>`; `jj bookmark list` shows `(conflicted)`), on both binaries.
   The reviewer's exact boundary: it needs all four of — the workspace's recorded operation behind
   the op-log head (always, on a shared clone); an unsnapshotted change at that operation; the
   bookmark on `@` itself; and a main-line move or delete of that bookmark in between. The
   daemon's own operations cannot supply the fourth: a bookmark provisioning just created tracks
   no remote branch, so no fetch can touch it, and once a worker has pushed, the push snapshotted
   `@` and the skill's procedure puts the bookmark on `@-`. The old unconditional `bookmark set
   --allow-backwards` masked this by force-resolving onto `@`; the new code leaves it conflicted,
   which is loud (the next provision refuses, see the table). **Deliberately not fixed** in #1023;
   do not open a follow-up without first showing a daemon spawn/resume path that reaches it.

## `JJ_USER` / `JJ_EMAIL` are honoured by both binaries; a CI runner has neither

`jj git push` refuses `Won't push commit … since it has no author and/or committer set`. A
developer box supplies identity from `~/.config/jj`; a GitHub runner has no jj config. Both
binaries take `JJ_USER` and `JJ_EMAIL` from the environment. Reproduce the runner locally with an
empty `JJ_CONFIG` file and `XDG_CONFIG_HOME` pointed at an empty directory.

## Removing a workspace: the abandon set, the forget, and the order (LEGION-104)

Verified 2026-09-14 on both binaries (`/tmp/legion-104-jjfacts.sh`, `/tmp/legion-104-crashshape.sh`
during planning and again during implementation), identical unless noted. These are the facts
`removeIssueWorkspace` (`packages/workspace/src/workspace.ts`) rests on; when the daemon runs it,
and the two guards that decide whether it runs at all, are in
`docs/solutions/daemon/a-close-fence-awaited-downstream-must-clear-before-the-close-hands-on-its-slot.md`.
None of them is visible to a fake command runner (it returns whatever exit code the test scripts
and leaves no side effect), which is why the three removal tests in `workspace.test.ts` and the
`/tmp` driver ran both binaries for real
(`docs/solutions/testing/jj-only-driver-with-mains-file-as-negative-control-is-the-proof-shape-without-the-rig.md`).

- `jj log -r '::a@ ~ ::(working_copies() ~ a@) ~ ::(bookmarks() | remote_bookmarks() | tags())' --no-graph -T 'commit_id ++ "\n"' --ignore-working-copy -R clone`
  lists exactly workspace `a`'s own commits, newest first: its working copy and every ancestor no
  bookmark, remote bookmark, tag, or other workspace's working copy reaches. `::main` is subtracted
  (so the root commit never appears), a commit under a surviving bookmark is not listed, a commit
  another workspace is stacked on is not listed. Exit 0 and no lines for an empty set.
- `jj abandon -r '<id> | <id>' --ignore-working-copy -R clone` of another workspace's working-copy
  commit (its directory already deleted) prints `Abandoned N commits` and gives that workspace a new
  empty working-copy commit, visible as `a@` in every other workspace's log. `-r` is an alias of the
  positional `[REVSETS]...`; a `|` union is one revset. `jj abandon -r 'none()'` prints
  `No revisions to abandon.` and exits 0.
- `jj workspace forget a --ignore-working-copy -R clone` then hides that empty commit;
  `jj workspace list -T 'name ++ "\n"'` (the template exists on both) no longer names `a`.
  Forgetting a name jj does not know: `Warning: No such workspace: a` / `Nothing changed.`, exit 0.
  `jj log -r 'a@'` for an unregistered name: `Error: Workspace \`a\` doesn't have a working-copy
  commit`, exit 1 — check registration before asking for `a@`.
- On 0.45.1 a colocated clone's `jj workspace add` creates a git worktree
  (`git --git-dir=clone/.git worktree list` names it, `prunable` once the directory is gone) and
  `git worktree prune` removes it after the forget; on **0.44.0** no git worktree is created and
  the prune is a no-op, exit 0.
- Order: delete the directory, then forget. Directory gone but still registered:
  `jj workspace add … --name a` says `Error: Workspace named 'a' already exists` (exit 1) —
  `createWorkspace`'s `already (registered|exists)` branch forgets, prunes, and adds again.
  Registration gone but directory present: `jj workspace update-stale` inside it says
  `Error: Nothing checked out in this workspace` (exit 1; 0.45.1 also
  `Removed Git worktree for …`) — the failure the wrong order would give every later provisioning.
- A fetch that deletes a merged branch's bookmark abandons the branch's commits unless the clone's
  `git.abandon-unreachable-commits` is `false`; provisioning writes that setting before every
  fetch (LEGION-84, `provisionIssueWorkspace`; both binaries accept `jj config set --repo`, and the
  file is `~/.config/jj/repos/<config-id>/config.toml`), so a test that provisions through it
  needs no write of its own for the removal, not the fetch, to be what makes those commits leave
  another workspace's log. A bare scratch clone driven by hand (the recipe below) sets it itself.

## Construction recipes (for re-verifying on a new pin)

```sh
export JJ_USER="Legion test" JJ_EMAIL="legion-test@example.invalid"
jj git init --colocate remote; jj git init --colocate clone
jj -R clone bookmark set main; jj -R clone git remote add origin "$PWD/remote"
jj -R clone workspace add w1 --name w1 --revision main
(cd w1 && jj bookmark set legion/X -r @)                       # normal
(cd clone && jj git push --remote origin --bookmark legion/X --allow-empty-description \
          && jj bookmark delete legion/X)                      # deleted local, remote row survives
(cd w1 && jj bookmark set legion/X -r @)                       # restore
jj -R clone workspace add w2 --name w2 --revision main
(cd w2 && jj new -m "w2 work")
OP=$(jj -R clone op log --no-graph -T 'id.short() ++ "\n"' --limit 1)
(cd w1 && jj new -m "w1 work" && jj bookmark set legion/X -r @)
(cd w2 && jj --at-op "$OP" bookmark set legion/X -r @ --allow-backwards)   # two moves from one op;
jj -R clone bookmark list legion/X                              # the next command reconciles → (conflicted)
jj -R clone config set --repo git.abandon-unreachable-commits false   # what provisioning writes before every fetch (LEGION-84)
jj -R clone workspace add a --name a --revision main; jj -R clone workspace add b --name b --revision main
(cd a && jj bookmark set legion/A -r @ && echo hi > f && jj new -m "a work 2")
jj log -r '::a@ ~ ::(working_copies() ~ a@) ~ ::(bookmarks() | remote_bookmarks() | tags())' --no-graph -T 'commit_id ++ "\n"' --ignore-working-copy -R clone
rm -rf a; jj abandon -r '<the ids, | -joined>' --ignore-working-copy -R clone; jj workspace forget a --ignore-working-copy -R clone
git --git-dir=clone/.git worktree prune; (cd b && jj log -r 'all()')
```

Run the three read forms after each step; compare with the table. The test file's `realJjRig`
(`packages/workspace/src/workspace.test.ts`) is the executable version of this recipe against
both binaries.
