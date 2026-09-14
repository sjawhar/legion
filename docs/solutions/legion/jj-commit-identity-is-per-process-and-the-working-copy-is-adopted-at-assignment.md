---
title: "jj commit identity is per process, and the working copy is adopted at assignment: the five jj facts that decide where identity lives in a multi-workspace clone"
category: legion
tags:
  - jj
  - commit-identity
  - workspace
  - shared-repo
  - JJ_USER
  - metaedit
  - include-overridden
  - github-app
date: 2026-09-13
status: active
module: daemon
related_issues:
  - "LEGION-44"
  - "sjawhar/legion#1025"
  - "LEGION-30"
  - "LEGION-20"
  - "LEGION-42"
  - "LEGION-45"
---

# jj commit identity is per process, and the working copy is adopted at assignment: the five jj facts that decide where identity lives in a multi-workspace clone

## Context

Every Legion issue workspace is a `jj workspace` of one shared clone
(`<state_dir>/repos/github.com/<owner>/<repo>`). Before LEGION-44 the pi-legion-envoy extension
set the worker's GitHub App identity at boot with `jj config set --repo user.name`/`user.email`,
and the last worker to boot — in any tree — became the committer of every concurrent tree's
commits (LEGION-30's implementer commit `95f0f339` carried committer `legion-reviewer[bot]`; a
LEGION-20 reviewer flipped the config under a committing LEGION-20 implementer). The fix
(sjawhar/legion#1025) rests on five jj facts, all verified on the box's fork `jj 0.45.1-sami` and
on stock `jj 0.44.0` (what CI runs). Each fact rules out a design that looks right on paper.

## Fact 1 — repository-scoped config is one file for every workspace of the clone

`jj config path --repo` from any workspace of a clone prints the same file,
`~/.config/jj/repos/<hash>/config.toml`. jj has user and repository scopes only; there is no
per-workspace scope. So `jj config set --repo` run from a workspace is a write to shared state
across every tree that shares the clone, and anything per-process — identity above all — must
not live there.

Consequence: identity rides the process environment. `JJ_USER`/`JJ_EMAIL` outrank every config
scope (user, repo, `--config`-less), so a value in the shared file is inert for a process that
carries them. The four Git variables (`GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_NAME`/
`GIT_COMMITTER_EMAIL`) mean nothing to jj — with only those set, jj still used the repo config's
identity — so a process that runs both jj and plain git needs all six. One mapping produces them
(`gitIdentityEnv`, `packages/daemon/src/daemon/github-app-env.ts`), used for the daemon's own
`gh`/git children (`buildRoleEnv`) and for every phase-worker pane (`launchWorker`, `processes.ts`).

The one thing that does outrank the environment is `--config user.name=…` on the command line.
That is why the old hazards-doc workaround (pin the App with `--config` on every write) had to be
rewritten as a prohibition, not annotated as closed: a worker following it literally would put
the wrong App back on its commits.

## Fact 2 — `jj split` and `jj describe` keep the author and refresh only the committer

A phase's work is carved out of the workspace's working-copy commit with `jj split`; both halves
keep that commit's author, and only the committer becomes the identity running the split. The
working-copy commit is born under whoever ran `jj workspace add` — the daemon, under its own user
config — and a split preserves that author even when it leaves the original working copy empty.
Per-process identity alone therefore fixes the committer and can leave every commit in the
workspace authored by the daemon operator for the workspace's whole life. Evidence at the time of
the fix: `legion/LEGION-42`'s commits were author `legion-reviewer[bot]` / committer
`legion-implementer[bot]` — the author was whatever the repo config said when the daemon ran
`workspace add`, inherited by every split since.

Consequence: the working copy is adopted for the role at the moment the role takes over:

```bash
JJ_USER=<app name> JJ_EMAIL=<app email> jj metaedit --update-author -r '@ & description(exact:"")' -R <workspaceDir>
```

`--update-author` takes the identity from `JJ_USER`/`JJ_EMAIL`; the revset touches only an
undescribed `@` — a described working copy is a previous phase's work and keeps its author
(`No revisions to modify.`, exit 0); a `@` already authored by the role is `Nothing changed.`,
exit 0. `jj describe --reset-author` does not exist in either build; `metaedit --author '<name>
<email>'` sets an explicit value and is the manual repair form.

## Fact 3 — the adoption belongs at assignment delivery, not at boot

A boot-time hook runs once per pane lifetime. A live idle worker resumed after another role's
phase (the implementer prompted again for a corrective round after the tester and reviewer ran)
never reboots, so a boot hook would leave `@` authored by the last role that did boot. The daemon
runs the adoption in `promptExistingWorker` — the one place `state.phases[issue]` is written —
for `kind: "assignment"` only, before the prompt frame and before any state write, so it covers a
fresh launch's `/worker/ready`, a `--resume`, and a live idle worker prompted over its socket
uniformly, and never a `catchup` (recovery plumbing that must leave everything as it was). A
failing `metaedit` fails the delivery with the command's failure and leaves the claim untouched:
no worker is prompted whose commits would carry the wrong author.

Two decisions taken with that placement, recorded here because only the review thread carried
them:

- A stale working copy (only another workspace's `jj undo`/`op restore` produces one, which
  LEGION-45 forbids) makes the adoption fail loudly at assignment. No `jj workspace update-stale`
  runs before the `metaedit`: it would hide a LEGION-45 violation and can move a worker's
  uncommitted edits. The live-idle path rejects `spawn_worker` with jj's hint on every retry until
  the idle-retire window relaunches the worker through provisioning, which runs `update-stale`.
  Loud and self-healing; do not "fix" it by adding the call.
- On the queued-promotion path an adoption failure counts against `promptFailures` like a socket
  rejection and, at the threshold, retires an otherwise healthy pane, which converges through the
  relaunch. Only reachable through the stale case above.

## Fact 4 — `jj config list --repo` hides a value the environment overrides

With `JJ_USER` in the calling process's environment, `jj config list --repo -R <clone> user.name`
prints `Warning: No matching config key for: user.name` (exit 0, empty stdout) for a value the
repo file genuinely holds; `--include-overridden` prints it as `# user.name = "…"`. The one-time
cleanup that removes the identity earlier boots left in the shared file
(`removeRepoScopedIdentity`, `packages/workspace/src/workspace.ts`) probes before it unsets
(`jj config unset` exits 1 on a missing key), and a bare probe run by a process carrying the
six variables — a daemon started from inside a Legion pane, or any injected runner the workspace
package cannot assume strips them — would print nothing and silently skip the unset. The
implementer's own test caught this only because the test process was a Legion pane.

Rule: any probe-before-mutate against a layered config must read the unmasked layer when the
caller's own environment can plausibly satisfy the query. The same flag belongs in a test helper
that reads repo scope from a pane.

## Fact 5 — a pane-only variable that is not a secret must still never reach the daemon's other children

The six identity variables are set by exactly one pane's own `-e` pair. A daemon started from
inside a worker pane (how every smoke rig and scratch daemon runs) inherits them, and the private
tmux server it forks would hand them to every pane that does not override them — the root
architect and the controller, which must carry none. The review caught this, not the
implementation: the exact-env tests see only `-e` pairs, so an inherited variable is invisible to
them. This branch first added the six to the daemon's strip list; LEGION-74 then replaced that
list with an allow-list (`PANE_ENV_ALLOW_LIST`, `environment.ts`), which passes no commit-identity
name, so the lock is now the allow-list test: it leaks all six into the daemon's environment and
asserts by exact equality that `paneEnv` and the `tmux new-session` command carry none.
`buildRoleEnv` and `adoptWorkingCopy` set them explicitly on the children that do commit as an
App. Rule for the next pane-only variable: check it is absent from the allow-list, add it to that
test's leaked environment, and never add it to the allow-list "for convenience". See
[config-env-keys-that-panes-also-carry](../daemon/config-env-keys-that-panes-also-carry.md) for
which keys in the rig's `env -u` scrub matter for the daemon's own reads.

## Which App a role is: one mapping, read, never copied

Which GitHub App a role acts as is `appRoleForLegionRole` (`api/github.ts`), and LEGION-42 is
changing it (planner, tester, reviewer, architect → review App; implementer, merger → code-writing
App). LEGION-44 reads that function for the pane identity and the adoption and never duplicates
the table, so the identity follows the mapping whenever it lands. The permanent test pins a named
App only for the two roles both mappings agree on (reviewer → review App, implementer → code-writing
App) and asserts the sub-architect against `appRoleForLegionRole("architect")` computed in the
test — the pattern for any test that touches a mapping another issue owns.

## Verifying identity before a push

```bash
jj -R "$LEGION_WORKSPACE" log -r 'main@origin..@' -T 'author.email() ++ " | " ++ committer.email() ++ " " ++ description.first_line() ++ "\n"'
```

Both columns must be the role's App **on every commit that role made** — not on the whole list:
earlier phases' commits are legitimately authored by their own App, and a conflict-forced rebase
legitimately sets the committer of every rebased commit, other roles' included, to the rebaser.
Once LEGION-42 lands, one PR carries commits by different Apps by design; a tester or reviewer
reading the whole list as one identity reports a non-problem.
