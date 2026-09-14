---
title: "Every issue workspace is a jj workspace of one shared main repo: identity, op log, and active-phase hazards a worker must defend against, and how"
category: legion
tags:
  - jj
  - workspace
  - shared-repo
  - commit-identity
  - op-log
  - phase-complete
  - catch-up
  - review-rationale
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "LEGION-30"
  - "sjawhar/legion#973"
  - "LEGION-44"
  - "LEGION-45"
  - "LEGION-37"
  - "LEGION-12"
  - "LEGION-34"
  - "sjawhar/legion#1003"
---

# Every issue workspace is a jj workspace of one shared main repo: identity, op log, and active-phase hazards a worker must defend against, and how

## Context

`provisionIssueWorkspace` creates each issue's `$LEGION_WORKSPACE` with `jj workspace add` off one
main clone (`~/.local/state/legion/<team>/repos/github.com/<owner>/<repo>`). Every concurrent
tree's workers therefore share one `.jj/repo`: one operation log, one repo-scoped config, one set
of bookmarks. LEGION-30 ran while five other trees were active and hit three distinct hazards of
that sharing. Hazard 1 is closed by the daemon (LEGION-44); Hazards 2 and 3 are daemon issues,
and until they land the defences below are the worker's job. The legion-worker skill's rules
("never `jj undo`/`op restore`", "carve commits by path") are partly these hazards written down.

## Hazard 1 — commit identity is repo-scoped, so another tree's provisioning flips yours (LEGION-44, closed)

jj's repository-scoped config (`~/.config/jj/repos/<hash>/config.toml`) is one file for every
workspace of the shared clone, so a `user.name`/`user.email` written there by any tree's worker
boot became the committer for every concurrent commit everywhere. Observed on LEGION-30: a
corrective-round fix commit carried committer `legion-reviewer[bot]` (author correct); a handoff
commit was born with a *reviewer author* because another tree's reviewer had just booted. The
`Omp-Session` trailer (a per-process `JJ_CONFIG` overlay) was correct throughout — only the
identity flipped.

Since LEGION-44 nothing writes identity into any jj config scope. Your identity is your pane's
environment: the daemon puts `JJ_USER`/`JJ_EMAIL` and the four Git author/committer variables,
resolved from your role's GitHub App lease, on the pane when it opens it (jj reads `JJ_USER`/
`JJ_EMAIL` over every config scope), and at every assignment it re-authors the workspace's
undescribed working-copy commit for your role (`jj metaedit --update-author`), because `jj split`
and `jj describe` keep that commit's author and only refresh the committer. Provisioning removes
the repository-scoped identity earlier boots left behind, once, logged. The jj facts behind each
of those three steps, and the two failure-mode decisions the design took, are in
[jj-commit-identity-is-per-process-and-the-working-copy-is-adopted-at-assignment](jj-commit-identity-is-per-process-and-the-working-copy-is-adopted-at-assignment.md).

There is nothing to pin. **Never set or override `user.name`/`user.email` in any jj or Git scope
— not `jj config set`, not `--config`, not `git config`.** `--config` outranks the pane
environment: a pinned value would put the wrong App back on your commits, which is the defect this
closed. Verify before a push instead:

```bash
jj -R "$LEGION_WORKSPACE" log -r 'main@origin..@' -T 'author.email() ++ " | " ++ committer.email() ++ " " ++ description.first_line() ++ "\n"'
```

Both columns must be your role's App on every commit you made — not on the whole list: earlier
phases' commits are legitimately authored by their own role's App, and a conflict-forced rebase
legitimately sets the committer of every rebased commit, other roles' included, to the rebaser. If
one of your own commits is wrong, the pane environment is wrong — a pane opened before the daemon
that set it, or a command run outside the pane with the variables unset — and the fix is to report
it to the architect, not to pin.

## Hazard 2 — another workspace's `jj undo` / `op restore` rewinds your operations (LEGION-45)

The operation log is shared. Mid-rebase, another workspace ran `undo: restore to operation …`
twice (op log entries 40 seconds apart), which rolled back a squash this workspace had just made.
Symptom: `jj` refuses with `The working copy is stale (not updated since operation <id>)`; the
working copy still holds your edits as an uncommitted diff, but the commit you squashed them into
is back to its pre-squash content.

Defence: `jj -R "$LEGION_WORKSPACE" workspace update-stale`, then *re-derive* what survived — read
the chain (`jj log -r 'quyvmrkx::@' -T '... ++ if(conflict, "CONFLICT", "")'`) and the working
copy diff, and re-apply whatever the rollback dropped. Never answer with your own `undo` or
`op restore`: that rewinds the other tree in turn. Keep the edits you are about to squash in a
scratch copy (`/tmp/…`) until the chain shows them landed, so a rollback costs a `cp`, not a
re-edit.

Confirmed again on LEGION-34 without any `undo`: a plain `jj git push` raced another workspace's
bookmark push, jj's `reconcile divergent operations` re-minted this workspace's working-copy commit
as a fresh empty one (the old change id went hidden), and the next command refused with the same
stale-working-copy error. The only content of that commit was the daemon-provisioned, untracked
`.omp/config.yml`, which `workspace update-stale` removes from disk (a file LEGION-58 has since
stopped writing; nothing depended on it). Nothing tracked was touched; the chain and the bookmark
were exactly where the push had left them.

Since LEGION-45 (#1020) the extension refuses these commands in every phase-worker pane — bash,
eval, hub, and a worker's `task` subagents — once the pi-envoy release carrying the guard is
installed into the profile. How the matcher is built and proven, and why it binds subagents:
`shell-command-gates-derive-from-bash-word-splitting-not-example-forms.md`.

## Hazard 3 — a daemon catch-up prompt can overwrite the active phase, so a real completion 409s (LEGION-37 acceptance 7)

`legion handoff complete` checks `phases[issue]` names this role and this session. A
`{"type":"catchup-worker","unhandled":[]}` prompt delivered to a *different* role's session by
the daemon's no-holder recovery rewrites `phases[issue]` to that role. The implementer's genuine
completion then answers `409 Phase for <issue> is no longer owned by this worker` although its
claim and grant are fine (the earlier check at workers.ts:320 passes).

Defence: the architect's standing instruction for this case — report the same summary to the
architect's role topic with `envoy_publish` and say the daemon refused the completion. Do not
retry in a loop; the phase will be re-pointed or the message suffices. Later rounds on the same
issue completed normally once the phase was the implementer's again.

## Hazard 4 — two roles editing one issue workspace at once: `jj workspace update-stale` hides the other's snapshot (LEGION-20)

A tree with a tester round and an implementer fix in flight at the same time has two live
sessions in one jj workspace. On #975 the tester committed its handoff (`.legion/test.json`
only) while the implementer had uncommitted code edits in the same working copy; the
implementer's next snapshot made the working copy stale, and the tester ran the documented
remedy, `jj workspace update-stale`, which moved the working copy to a fresh empty commit. The
implementer's edits were not lost: jj had snapshotted them into a now-*hidden* commit whose
parent was the tester's handoff. `jj log` and `jj edit <change id>` cannot see a hidden commit
(`Revision doesn't exist`); `jj edit <full commit id>` can, and revives it. Check with
`jj log -r <commit id> -T 'change_id ++ " hidden=" ++ hidden'`.

Defence, in order: (a) the two roles agree who holds the workspace before either commits, and the
one that yields says so in a message (the tester did — that message is how the implementer knew
the commit id); (b) the role that runs `update-stale` reports the hidden commit's id and file list
to the other; (c) the other resumes with `jj edit <commit id>`, never by re-typing the edits.
The skill assumes one active phase per workspace; a corrective round breaks that assumption, so
say who has the workspace out loud.

## Also from this issue

**A rationale that sounds right can be contradicted by code.** The first three drafts of the
architect-exclusion rationale said a retired sub-architect's wakes "arrive by a publish that is
rejected with no live holder, never by the dead-worker recovery path." Review round 1 traced the
code: `onUndeliverable` (index.ts) and `handleException` (processes.ts) route a sub-architect's
404 to `resumeWorker` — the recovery path *does* reach it. The behaviour (exclude architects) was
right for a different reason (an architect has no phase of its own, parks for the life of its
subtree, and a relaunch per wake costs more than one idle process). The spec was corrected to v7
and three doc sites reworded. Rule: before writing "X never happens" in a doc comment, find the
code that would make X happen and quote the line that prevents it; if you cannot, the sentence is
a hypothesis, not a rationale.

**The merge queue's "pair" gate on this repository is one reviewer record naming both lenses.**
The review App runs `thermonuclear-deep-review` and `thermonuclear-code-quality` once per head and
records both verdicts explicitly ("Lens 1 — correctness/security/breakage", "Lens 2 —
structure/abstraction/type boundaries") in the review body and the PR body's **Thermo** line. That
single record satisfies the queue's pair requirement for `src/` changes; a second human reviewer
is the repository's own branch-protection question, not Legion's.

**Grant stacking (LEGION-12).** Only the first of several stacked `export LEGION_GRANT=` blocks in
one bash call redeems; the rest 403. Install a recorder in the persistent shell
(`export() { …GRANTS_SEEN+=…; builtin export "$@"; }`), then in the next call `unset -f export`,
POST each seen grant to `/legion/v1/gh-token`, and export the one that answers 200 before `jj git
push` / `legion gh` / `legion handoff complete`. It worked on every one of ~40 credentialed calls
across this issue's four rounds.
