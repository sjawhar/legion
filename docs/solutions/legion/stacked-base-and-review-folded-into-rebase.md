---
title: "Do not stack a T0 fix on an unmerged PR: main plus a dropped duplicate, then main only; fold reviewer findings into a forced rebase pass; expect the daemon's own credential and phase-record faults in a long-lived implementer"
category: legion
tags:
  - legion
  - jj
  - stacked-pr
  - rebase
  - review-round
  - merge-queue
  - LEGION-12
  - LEGION-37
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-28"
  - "sjawhar/legion#980"
  - "LEGION-11"
  - "sjawhar/legion#955"
  - "LEGION-37"
  - "LEGION-12"
---

# Stacked Base, Review Folded Into the Rebase, and the Daemon's Own Faults

Process learnings from LEGION-28 (PR sjawhar/legion#980), a production T0 fix whose
implementer session lived through a base change, a forced conflict-only rebase, two review
rounds, an operator-driven pane stop and `--resume`, and the retro. None of this is about the
code; all of it cost time that a future worker can skip.

## 1. A T0 fix does not wait behind another PR's pipeline

The spec said item 4 was another issue's (LEGION-11, #955) and the plan built on it: `main` plus
a `jj duplicate` of #955's single fix commit placed under this issue's commits, with the merger
told to `rebase --skip-emptied` once #955 landed. Sound in isolation — and the duplicate did
apply conflict-free. But #955 was not ready (back in testing, head moving), and the merge-queue
organizer would not let a production crash-loop fix sit behind it.

What made the change cheap was checking the real dependency rather than the assumed one:
nothing in #980 needed #955's ordering for correctness — the per-claim isolation made #955's
`TypeError` a contained per-claim failure instead of a loop abort, and item 4 stayed #955's.
So the architect chose "drop the duplicate, rebase onto `main`", the branch's own `index.ts`
hunks were re-resolved toward `main`'s placement, and #955 rebases over the restructure later.

Rules that fall out:

- A stacked base is a scheduling dependency, not just a code one. Before stacking on an open PR,
  ask whether the fix is *correct* without it; if yes, the default is `main`, and the other PR's
  behaviour is named in the PR body as unchanged here.
- When you do stack a duplicate, keep it as its own commit and never let it carry the other PR's
  handoff/docs commits (they conflict with `main` and later phases would misread them as
  predecessors). `jj duplicate <one commit> -d <plan handoff>` did exactly that.
- Escalate the swap as a decision with the facts (`envoy_publish` to the architect: options,
  verified consequences, recommendation, and "I am holding the push until you answer") rather
  than doing it on the organizer's say-so alone: the organizer owns the queue, the architect owns
  the tree.

## 2. Conflict-only rebase: resolve commit by commit, keep both sides, stop at a real change

`main` moved 40 commits while #980 waited (LEGION-21's runtime boundary, LEGION-30's idle-retire
config key, #978's CLI change). `jj rebase -s <plan handoff> -d main@origin`, then for each
conflicted task commit in order: `jj new <commit>`, resolve, `jj squash`. Every task stayed a
separate reviewable commit; the reviewer diffed the new head against the old.

Mechanical conflicts (both sides kept) were: two config keys added at the same six places
(`workerIdleRetireSeconds` beside `slowCommandTimeoutSeconds` in `config.ts` and every
`DaemonConfig` test literal); an import line both sides edited; renames in code this branch had
extracted (`probeWorkerSocket`→`probeWorker`, `workerClient`→`clientFor`,
`reconcileTmuxWindows`→`reconcileOrphans`, `WorkerLocator`→`Locator`, test fixtures moving to
`ProcessManagerDeps & RuntimeOverrides` and reading locators through `recordedTmuxLocator`/
`tmuxFields`). See `handoff-file-conflicts-during-rebases.md` for why resolving bottom-up with
`squash` avoids re-conflicting descendants.

The one non-mechanical conflict was a **new boot check** `main` had added (`verifyLegionPluginContract`,
#962) sitting between the two OMP probes this branch turns into a background chain. Its tests
asserted "before loading state or opening NATS". Putting it inside the chain would have preserved
`main`'s position but broken `main`'s tested guarantee; awaiting it synchronously *before* the
chain preserved both sides' tests untouched. That is what "conflict-only" means when a conflict is
not mechanical: find the placement that keeps every existing test green on both sides, and if
none exists, stop and ask. The architect was asked with both options and the verified facts, and
answered in minutes.

Two small things that bit: the shared `.jj` op log shows other issues' workspaces (do not
`jj undo` there), and abandoning a scaffold commit the local bookmark happened to point at
deleted the bookmark — re-point it (`jj bookmark set <name> -r <tip>`) before any push so
`--deleted` can never be what gets pushed.

## 3. Fold reviewer findings into the rebase pass

The reviewer's first round landed while the rebase was being prepared. Instead of pushing the
rebase, waiting for a re-review, then fixing, the architect asked for the findings as separate
small commits *on top of* the rebase commits, each with its test, in the same push. Six findings
(a negative marker followed by a hang is definitive; the runner claims a kill only for a
still-running child; a held resurrection is dropped for a tree no longer active; a failed
temp-clone cleanup never masks the clone error; a pre-hold boot failure aborts a probe still
waiting; wording) went in that way, then a second round of five plus the `.legion/` deletion.
Each round was one push, one CI run, one re-review of the delta. Keep the rebase commits and the
fix commits separate so the reviewer can diff either; never mix a finding into a rebase
resolution.

Every fix commit was written test-first and the red run was observed and recorded (the review
threads name the commit and the test). When a test needs a mechanism the environment may defeat
(a `chmod 000` directory to make `rm` fail), skip it where the mechanism does not bind
(`test.skipIf(process.getuid?.() === 0)`) rather than let it pass for the wrong reason as root.

## 4. The daemon's own faults show up in a long-lived implementer

A worker that lives across many rounds meets the daemon's rough edges:

- **Double-injected `LEGION_GRANT` preamble (LEGION-12).** The bash tool sometimes prefixes a
  call with two or three `export LEGION_GRANT=…` preambles; the shell's last export wins and only
  the *first* grant exists in the daemon (60 s TTL), so `legion gh`/`jj git push`/`handoff
  complete` answer 403 "Invalid or expired grant". Retry the call; if it recurs, pass the first
  preamble's grant explicitly (`LEGION_GRANT=<first> legion gh -- …`) within its TTL. A 403 that
  arrives within seconds of the call is this, not a revoked capability.
- **`handoff complete` 409 after a respawn (LEGION-37).** Documented in
  `worker-pane-shell-gotchas.md` §11; on this issue it happened on every round after the first.
  The architect accepted the completion report by `envoy_publish` message each time. Do not
  retry and do not write a second handoff file.
- **Operator-driven pane stop mid-work.** The process was stopped over the shutdown frame and
  relaunched with `--resume` (a plugin credential fix). What made it painless: every resolution
  already squashed into its jj commit, an empty working copy, and a checkpoint message to the
  architect naming which commits were resolved, which remained, and the exact next step. jj's
  auto-snapshot means nothing is lost even without that, but the message is what let the
  resumed session continue in one turn.
- **The kernel resets on relaunch.** Helpers defined in the eval kernel are gone after a
  `--resume`; redefine before use, and keep the bash-tool fallbacks in mind (it was unreachable
  for whole stretches; `subprocess` from the kernel ran jj and bun fine, but every credentialed
  call needs the bash tool's injected grant).

## 5. Real-surface proof is its own artifact

The acceptance criterion asked for a real daemon under load. A scratch `startDaemon` (own
`state_dir`, own private tmux socket, own ports; only the token manager, NATS, Dispatch and
`envoyPublish` faked) with the OMP launch wrapped to `exec sleep 60` for two attempts produced,
on this box at load average 85–110, the exact log lines, the 200 from `/legion/v1/state` during
the hold, the empty pane list, and the post-hold pane — plus the negative control's exit 1. The
implementer ran it as its own real-surface check and posted it on the PR; the tester's
independent run remained the gate. See `../testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md`
for the general recipe; the LEGION-28 specifics (the `exec sleep` wrapper — `exec` matters, a
surviving child would hold the runner's pipes — and `LEGION_SLOW_COMMAND_TIMEOUT_SECONDS=5`) are
in the PR's E2E section.
