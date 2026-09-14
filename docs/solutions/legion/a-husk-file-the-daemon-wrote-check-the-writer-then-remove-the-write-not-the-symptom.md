---
title: "A husk file the daemon wrote into every workspace: find the writer in the tool's source and a scratch boot before designing an ignore, then remove the write — with the jj ignore shapes measured on 0.45.1-sami and why each was rejected"
category: legion
tags:
  - legion
  - workspace-provisioning
  - jj
  - omp
  - dead-code
  - ignore-rules
  - negative-control
  - docs-hygiene
  - rebase
  - implementer
  - planner
date: 2026-09-13
status: active
module: packages/workspace, packages/daemon
related_issues:
  - "LEGION-58"
  - "sjawhar/legion#1039"
  - "LEGION-59"
  - "sjawhar/legion#753"
  - "sjawhar/legion#813"
  - "sjawhar/legion#816"
symptoms:
  - "`jj status` in a freshly provisioned issue workspace shows `A .omp/config.yml` before any worker has typed a thing"
  - "Every phase worker commits with a path-scoped `jj split` to keep an empty file out of its commits, and a solutions doc tells it to copy the file aside before `jj new`"
  - "A solutions doc says 'OMP writes an empty `.omp/config.yml` when a session boots'; OMP's source has no such write"
  - "The obvious fix is an ignore rule, and two of them are on the table"
---

# A Husk File the Daemon Wrote: Check the Writer, Then Remove the Write, Not the Symptom

LEGION-58 (sjawhar/legion#1039) removes an empty `.omp/config.yml` that the daemon's own
workspace provisioning (`provisionIssueWorkspace`, `packages/workspace/src/workspace.ts`) and
tmux controller runtime (`TmuxRuntime.spawnController`,
`packages/daemon/src/daemon/runtime-tmux.ts`) wrote into every issue workspace and into
`<state_dir>/controller`. Nothing ignored the path, so jj snapshotted it into every worker's
working copy; workers dodged it with path-scoped commits and one solutions doc had grown a
copy-it-aside recipe around it. The fix deletes both writers, adds no ignore, and leaves the
controller directory as a plain `mkdir`. Everything below is what a future worker meeting a stray
file, a "tool X writes this" claim, or an ignore-rule proposal should already know.

## 1. Recognise a husk: the writer's own history and a claim nobody sourced

The file was created with content by sjawhar/legion#753 (`9278a29c50bc`, "rebuild Legion on
oh-my-pi"): `task.maxRecursionDepth: <n>` and `extensions: [<pi-envoy package>]` — the
per-workspace way to load the extension and cap recursion. #813 (`397839c71723`, "load pi-envoy
once … drop --extension") removed the `extensions:` line; #816 (`037a6b92a641`, headless phase
workers) removed `maxRecursionDepth`. Both left the writers in place producing an empty file, and
#816's description records only "teach merged fixtures the emptied omp config.yml" — no reason to
keep the file was ever written down. The test that pinned it (`readFile(… ".omp/config.yml") === ""`)
asserted the husk, not a behaviour.

Two signals, and either alone is enough to stop and check before designing around a file:
the writer's history shows its content being removed field by field with no replacement, and the
only surviving explanation of the file is a retro doc's sentence that names a tool nobody had read
the source of (`long-lived-branch-mechanics` §1 said OMP wrote it on boot; it never did — the
daemon did, at provisioning). A tool-attribution claim in `docs/solutions/` is a hypothesis with a
date on it, not a fact about the tool.

## 2. Verify "tool X writes this" against the tool's pinned source and one scratch boot

Both checks together took the planner under an hour and settled the whole design. Do them before
any ignore rule is drafted.

**Source.** The pinned OMP is `OMP_FORK_PIN` in `packages/daemon/src/daemon/omp-pin.ts`
(`github:sjawhar/oh-my-pi@<version>` = tag `v<version>` in `/home/ubuntu/oh-my-pi`; read with
`git show <tag>:<path>`, never by touching that checkout's working copy). What was found at
`18.1.18-sami.20260912-104423`:

- `getProjectDir()` (`packages/utils/src/dirs.ts`) is `process.cwd()` — no upward search for any
  marker; `getProjectAgentDir(cwd)` is `path.join(cwd, ".omp")`. Nothing locates a project by the
  presence of `.omp/config.yml`.
- `#readProjectSettings` loads the project file with `#loadYamlIfPresent`, which returns
  `{kind: "missing"}` on ENOENT → `{}`; an empty file parses to `null` → `{}`. **Absent equals
  empty.** The pane's real settings come from the profile (`OMP_PROFILE=legion`,
  `PI_CODING_AGENT_DIR=~/.omp/profiles/legion/agent`, inherited from the daemon's environment) and
  from `--append-system-prompt`.
- The only writers under `<cwd>/.omp` are `#saveProjectNow` (fires only after project-scoped model
  roles were modified), plan-mode autosave (`.omp/plans`), and the omfg rule writer
  (`.omp/rules/<name>.md`). All user actions; none is boot.

**Scratch boot.** The pinned binary
(`/home/ubuntu/.mise/installs/github-sjawhar-oh-my-pi/<version>/bin/omp`) started in a fresh
`mktemp -d` the way a pane starts (`--mode rpc --append-system-prompt …`, `OMP_PROFILE=legion`, the
real profile plugin tree, `LEGION_*`/`DISPATCH_*` scrubbed so the Legion extension stays inert)
answered `ready`, negotiated protocol v2, answered `get_state`, exited 0 — and
`find <dir> -mindepth 1` printed nothing. A first run without the scrub (the extension tried its
boot handshake and exited on 403) also wrote nothing. Boot writes no file into the working
directory.

Consequence: dropping the daemon's write is sufficient. There is nothing to ignore.

## 3. Remove the write; the ignore shapes were measured and rejected — do not re-propose them without a new measurement

The spec allowed "no longer written, or ignored through a workspace-scoped ignore jj honours".
The planner measured the ignore shapes on the box's jj (`0.45.1-sami.20260910`, the daemon's) on a
temp repo laid out like production — colocated `jj git clone <origin> repo` (`.git` and `.jj` both
present) plus `jj workspace add <ws> --name … --revision main -R repo` per workspace, exactly the
layout under `$LEGION_STATE_DIR/repos/github.com/sjawhar/legion` and its workspaces:

| shape | `jj status` in a fresh workspace with the stray file | why rejected |
| --- | --- | --- |
| baseline, no ignore | `A .omp/config.yml` | the bug |
| `.omp/` appended to the shared clone's `.git/info/exclude` | `The working copy has no changes.`; `jj file list` omits it; one file covers every workspace of the clone | an ignore must precede the first snapshot: a workspace that had already snapshotted the file still shows `A .omp/config.yml`, so it cannot repair existing workspaces; and it means nothing for the controller directory, which is not a jj workspace. Suppresses a symptom nothing produces once the write is gone. **This is the working fallback if a writer ever returns.** |
| `snapshot.auto-track = '~glob:".omp/**"'` via `jj config set --repo` | file not tracked, but `Untracked paths:` / `? .omp/` — not the clean status the acceptance asks for | on this jj fork `--repo` config lives at `~/.config/jj/repos/<config-id>/config.toml`, in the operator's home, not inside the clone: daemon-provisioned state in a per-user file |
| drop the write (chosen) | `The working copy has no changes.`; `echo x > work.txt && jj describe -m probe && jj log -r @ --summary` → `A work.txt` only | — |

Adding `.omp/` to the repository's tracked `.gitignore` from provisioning was rejected in the spec
before measurement: it mutates a tracked file in every workspace. The old doc's "fix candidate:
add `.omp/` to `.gitignore`" was that shape; the PR deleted the sentence rather than implementing
it, so a reader of `jj blame` alone could mistake the missing ignore for the missing half of the
fix. It is not: nothing writes the file, and the acceptance criterion (`jj status` clean) is met
without it.

Rule: when the root cause is "our own code writes X", the fix is to stop writing X. An ignore is
a fallback with its own failure modes (must precede the first snapshot; repo-level jj config lands
outside the clone), and it is only worth its cost when the writer is not ours.

## 4. Replacing a wrapper that also created a directory: keep the position and the failure

`TmuxRuntime.spawnController` called `writeOmpConfig(controllerDir)`, whose `mkdir` was the only
thing creating `<state_dir>/controller` — the controller pane's cwd (`cd <controllerDir> && …
worker-shim …`). Deleting the wrapper naively would have made the pane's `cd` fail and the
controller never register. The replacement is `await mkdir(controllerDir, { recursive: true })`
at the identical call site, before `preparePane` and `tmux.openWindow`. A failure rejects through
`ensureController`'s same controller-spawn path as before; it adds no error path and needs no new
test. The tester's negative control confirms the dependency:
with the `mkdir` removed, the gated real-tmux controller e2e
(`LEGION_E2E=1 LEGION_TMUX_LIVE=1 bun test src/daemon/__tests__/real-deployment-instructions-e2e.test.ts`)
fails at `tmux window ownership marker failed (exit 1)` from `openWindow` — the pane cannot even
open without its cwd.

When you delete a helper, list what its callers depend on beyond the obvious side effect: the
directory it created on the way, *where in the sequence* it ran, and *how* it could fail.

A near-miss in the same commit: `ProvisionIssueWorkspaceDeps.extensionPackage` (the deps field,
dead since #813) and `EXTENSION_PACKAGE` (the daemon constant that still names the role prompts,
`path.join(EXTENSION_PACKAGE, "roles", "<role>.md")`) share a name shape. Only the field went; a
grep-and-delete sweep on the constant would break role-prompt resolution.

## 5. The proof is the real `jj` driving the real provisioning against a local origin — and its negative control

`bun test` was the regression lock; the proof was a scratch run of the branch's
`provisionIssueWorkspace` (imported by absolute path from `$LEGION_WORKSPACE`) driven by the box's
`jj` and `git`, against a local bare origin laid out like production. Pre-seeding the clone
(`jj git clone <origin> <state>/repos/github.com/acme/widgets`) makes `ensureRepoClone`
short-circuit so no GitHub network is touched; provisioning's own `jj git fetch` then fetches from
the local origin. The whole run took under five seconds. Three things about it are reusable:

- **Negative control from the fork point.** `jj file show -r main@origin packages/workspace/src/workspace.ts > /tmp/…/workspace-prefix.ts`,
  point the same driver at that copy (its deps object still needs `extensionPackage`), run it
  against a second identically seeded origin: `A .omp/config.yml`. The check bites; the fixed run's
  clean status is not an artefact of the rig.
- **Mutation check for a deletion.** The tester reintroduced the writer just before
  `provisionIssueWorkspace` returns, ran the suite (11 pass / 2 fail: fake-jj `readdir` received
  `[".omp"]`, real-jj `[".jj", ".omp"]`), then `jj restore`d. "Prove the deletion is safe" has the
  same shape as "prove the addition works": put the old code back and show exactly which
  assertion catches it (see
  [race-regression-tests-that-fail-before-the-fix](../testing/race-regression-tests-that-fail-before-the-fix.md)).
- **Scratch-rig gotcha.** With `GIT_CONFIG_GLOBAL` scrubbed, `git init --bare` defaults HEAD to
  `master`, so `jj git clone` creates no local `main` and `jj workspace add --revision main` fails
  `Revision main doesn't exist`. Seed with `git init --bare --initial-branch=main` (GitHub's
  default), or seed through a jj repo that pushes `main`.

Nothing under `$LEGION_STATE_DIR` or any live workspace is touched by this rig; the tester
recorded the live workspaces directory's mtime before and after as evidence.

## 6. jj 0.45 puts a `.git` gitdir link in every workspace of a colocated repo; the unit test pins 0.44's layout

The plan expected `ls -A <fresh workspace>` to show `.jj` and `README.md` only. On the box's jj
0.45.1-sami it shows `.git` too — a one-line file, `gitdir: <clone>/.git/worktrees/<name>`, the
same file every production issue workspace has (`cat $LEGION_WORKSPACE/.git`). It is jj's, not
provisioning's: a workspace of a colocated repository is a git worktree. The real-jj unit test in
`workspace.test.ts` runs the stock `mise x github:jj-vcs/jj@0.44.0` and asserts both that
`readdir(workspaceDir)` equals `[".jj"]` and that `.git` is absent. When `STOCK_JJ` moves to
0.45, that test must add `".git"` to the directory expectation and update the `.git` assertion to
present (round-1 review, fast-follow item 4 — a test-file change, not a `docs/solutions/` one, so
it lands with the next PR touching that file). When copying this test as a template, prefer the
observable contract — `jj status` containing `The working copy has no changes.` — over exact
directory layout; that layout is a jj-version detail.

## 7. Process: CONFLICTING before any review, the resolution in the commit that owns the lines, and a per-bullet docs sweep

- **No `Tests` run for a pushed head is the CONFLICTING signature.** Two heads were pushed and
  neither got a `pull_request` run; `legion gh -- pr view 1039 --json mergeable,mergeStateStatus`
  said `CONFLICTING`/`DIRTY` (main had moved four commits under the branch). Read mergeability first
  ([conflicting-pr-gets-no-pull-request-ci](../github/conflicting-pr-gets-no-pull-request-ci.md)).
  The rebase (`jj rebase -s 'roots(main@origin..@)' -d main@origin`, whole chain) was done while the
  implementer phase was still active and no other role had been spawned — the one window in which
  the implementer may rebase without asking
  ([completed-phase-touches-nothing-and-conflicting-is-reported-not-rebased](completed-phase-touches-nothing-and-conflicting-is-reported-not-rebased.md)).
- **A conflict in a shared test file is resolved in the commit that owns the lines.** Main's new
  concurrent-clone test needed `readFile`, which the fix commit had dropped from the import; main's
  test also carried `extensionPackage,` and `LEGION_MAX_RECURSION_DEPTH = "11"`, which the refactor
  commit's type change made non-compiling. The import line went back to main's (the fix commit no
  longer touches it) and the two dead lines were deleted in the *refactor* commit, which already
  owned that deletion — not patched into the fix commit because that is where jj reported the
  conflict. The fingerprint changed (`aa99ced5… → 68f352d6…`) and the PR comment quoted the exact
  `diff` of the two fingerprint inputs: those three lines and nothing else. A changed fingerprint
  with its delta shown is a confirmation the reviewer can check in a minute; an unexplained one is a
  new round.
- **Three commits split on provable independence** (fix / dead-dependency refactor / docs) let the
  reviewer triage findings by commit: the docs nits rode retro's `docs/solutions/` commit without
  voiding the approval, the test nits wait for the next PR touching those files, and the refactor
  could have been dropped without touching the fix.
- **The docs sweep rule is per bullet, not per file.** "Present-tense guidance that would send a
  worker looking for, protecting, or dodging the file is rewritten; past-tense narrative of what
  happened on a tree is kept" — applied per file, it let one checklist bullet in
  `rebasing-a-branch-across-a-refactor-of-its-own-call-sites` §5 slip through in present tense
  with the wrong attribution ("the extension's uncommitted …"), one line above a bullet the same PR
  rewrote. Reviewer and tester both caught it; retro fixed it. Read each sentence's tense and
  attribution on its own.

## 8. What this fix does not touch

- **LEGION-59's push refusal is a separate mechanic.** With no stray file at all, `jj split -m … <paths>`
  still leaves `legion/<KEY>` on the empty, undescribed working copy, and `jj git push --bookmark`
  is still refused `Won't push commit … since it has no description`; only
  `jj bookmark set legion/<KEY> -r @- --allow-backwards` before the push fixes that
  (`worker-pane-shell-gotchas` §3). The file was never the cause of the refusal — it was only the
  reason the refused commit was non-empty. Verified on jj 0.45.1-sami with no `.omp` present.
- **In-flight workspaces keep their copy.** A tree provisioned before the fixed daemon deployed
  can retain `A .omp/config.yml` until it closes; the fix deliberately does not edit a live
  working copy.
- **Verify a fresh workspace before a split.** After a normal daemon restart provisions its first
  new tree, run `jj -R "$LEGION_WORKSPACE" status` before any split or commit and verify `The
  working copy has no changes.`; then run `test ! -e "$LEGION_WORKSPACE/.omp/config.yml"`. After
  its first user-authored commit is pushed, run `jj -R
  "$LEGION_STATE_DIR/repos/github.com/sjawhar/legion" git fetch` followed by `jj -R
  "$LEGION_STATE_DIR/repos/github.com/sjawhar/legion" log -r 'ancestors(legion/<KEY>@origin) ~
  ancestors(main@origin)' --summary`. The status must be clean, the file must be absent, and the
  first-push ancestry must not list `.omp/config.yml`.
