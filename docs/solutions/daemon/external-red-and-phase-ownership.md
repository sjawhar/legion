---
title: "A By-Design Red PR Exhausts fixAttempts, and worker/started Stole the Active Phase (fixed in LEGION-37)"
category: daemon
tags:
  - daemon
  - reducers
  - pr-blocked
  - phases
  - worker-started
  - github-apps
  - review
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "sjawhar/legion#966"
  - "LEGION-34"
  - "sjawhar/legion#1003"
symptoms:
  - "`pr-blocked` wakes on every push to a PR whose failing check is external and expected"
  - "`legion handoff complete` → 409 `Phase for <KEY> is no longer owned by this worker` for the worker the architect just prompted"
  - "the review App cannot push its handoff commit or resolve review threads (`Resource not accessible by integration`)"
---

# A By-Design Red PR Exhausts fixAttempts, and worker/started Stole the Active Phase (fixed in LEGION-37)

Three daemon-side behaviours LEGION-23 ran into that were not defects of the PR under review.
Two are daemon design gaps to file, not to fix inside an unrelated PR; the third is a GitHub App
permission fact every review round on this repository meets.

## 1. `fixAttempts` counts every new head while the PR is red, whatever the push contained

`reducers.ts` `resetPrHead` runs on every new head and does
`if (pr.verdict === "red") pr.fixAttempts += 1;` before clearing the verdict; `reduceCiEmission`
routes `{type: "pr-blocked", attempts}` to the active phase once
`fixAttempts >= config.maxFixAttempts` on a red settlement. Nothing distinguishes a push that
tried to fix the red from a handoff-only `.legion/*.json` commit, and nothing distinguishes a red
the PR's own diff caused from one that is external and expected.

PR #966 carried a by-design red for two review rounds: its `Worker Image` check failed at
"Require the Depot project variable" until a root-level decision landed (and then Depot itself was
removed). Every handoff push counted as a failed fix attempt, the counter hit the threshold, and
the daemon emitted a `pr-blocked` wake per push — noise the architect had to tell every worker to
ignore.

**What a worker should do:** nothing. Report the red's external cause in the PR body's CI line so
the merge queue and the reviewer know why it is red. Handoff pushes no longer count, so a
`pr-blocked` wake on such a PR means real fixes were pushed onto the red, not bookkeeping.
**Landed in LEGION-33:** a head whose push changed only `.legion/` paths no longer counts toward
`fixAttempts` (the listener forwards `changed_paths`; the daemon classifies), and `pr-blocked`
fires once per exhausted count. An expected-external red still counts real fixes as before.

## 2. `/worker/started` overwrote `phases[issue]` unconditionally — fixed in LEGION-37 (#991)

**Historical (main before #991).** `api/routes/workers.ts` (the `/worker/started` handler) ended
its claim update with `ctx.deps.state.phases[issue] = { phase: role, sessionId };` for **whatever**
role was registering, and the prompt paths (`promptExistingWorker`, the `pendingAssignment`
delivery at `/worker/ready`) also set `phases[issue]` to the prompted worker. Nothing guarded the
*next* registration: a tester whose pane (re)registered, a merger booting, a resumed sibling, or —
the common case — the daemon's own no-holder recovery relaunching a *finished* worker with a
catch-up. `phase/complete` then checked `phase.phase === grant.role && phase.sessionId ===
grant.sessionId` and answered 409 `Phase for <KEY> is no longer owned by this worker` to the worker
that actually held the task. That is what the LEGION-23 implementer saw after the cleanup round,
and it bit LEGION-37's own tree three times while the fix was being written.

**Current behaviour (since #991).** `state.phases[issue]` has exactly one writer:
`promptExistingWorker` in `processes.ts`, and only when the prompt it delivers is an architect
**assignment** (`pendingAssignment.kind === "assignment"`, whether prompted straight into a live
worker or delivered from the claim at `/worker/ready`). `/worker/started` never writes it (a
bystander registration logs one `… registered session … while <KEY>'s active phase is …` line), a
reconnect never writes it, and the daemon's own `catchup-worker` prompt never writes it. A finished
phase worker is not sent a catch-up at all, and a catch-up already queued for it is dropped at
delivery if the phase has moved on. The phase changes only through the architect's next
`spawn_worker`. Full design, delivery-point checklist, and the review-round lessons:
`one-writer-for-the-active-phase-and-bystander-catchups.md` and
`retire-then-drain-a-pane-you-decide-not-to-prompt.md` in this directory.

**What a worker should do on a 409 today:** it is no longer the phase record being stolen by a
relaunch. Check `phases[<KEY>]` in the daemon's `state.json` (not `GET /state`, which redacts
`phases`): if another role holds it, the architect issued a `spawn_worker` for that role, which
*supersedes* the caller's phase by design (`skills/legion-architect/SKILL.md`). Report the 409 to
the architect over Envoy with the work already pushed; do not retry in a loop.

## 3. The review App can neither push nor resolve threads; the implementer App does both

The `legion-reviewer` GitHub App holds `pull_requests: write` and no `contents` permission, and
GitHub lets only the pull request's author or an account with write (push) access to the repository
resolve a review thread or push to its branch. The review App is neither by design, so
`resolveReviewThread` from it returns `Resource not accessible by integration`, and so does its
push. Widening the review App is rejected by design, not because it would not work. So in every
round:

- the reviewer's `.legion/review.json` commit exists only in the shared workspace until the
  implementer's next push carries it (check `jj log` that it is an ancestor before building on it);
- the reviewer answers each thread it opened with one of `Accepted: fixed in <commit> — <one line>`,
  `Accepted: not a defect — <reason>`, or `Still open: <what remains>`, and the **implementer**
  runs `legion threads resolve --pr <number> --repo <owner>/<repo>` (LEGION-34) before its next
  push: it resolves every unresolved thread whose newest comment is the opener's own `Accepted:`
  reply, one `resolveReviewThread` per thread, prints `resolved <url>` /
  `left open <url> — newest reply by <login> is not an acceptance`, and exits 1 naming the thread
  and GitHub's message when GitHub refuses one (report it to the architect; a human resolves that
  thread). The merger runs it once more before READY. Never a hand-written GraphQL mutation and
  never a bulk resolve;
- the reviewer's approval is the review itself; the head it approves by name is the
  implementer-pushed `.legion/` deletion.

Thread ids are the `PRRT_…` node ids the reviewer records in `review.json` (`findings[*].threadId`);
they are stable across pushes even when the thread goes `isOutdated`. On `sjawhar/legion`, `main`'s
ruleset (id 12331919, read with `legion gh -- api repos/sjawhar/legion/rules/branches/main`) sets
`required_review_thread_resolution: false`, so an unresolved thread does not stop the merge queue
there today; the command keeps the Threads record truthful whatever that setting is.

**The rule is easy to misstate, and the first draft of LEGION-34 misstated it in four files.** The
intent — "whoever raised the point signs it off" — reads as if the *comment's author* could resolve
the thread, and the docs, the worker skill, the daemon `AGENTS.md`, and a doc comment all said
"the pull request's author, a comment's author, or an account with push access" until the
reviewer caught it (review 5189854058 on sjawhar/legion#1003). GitHub's own text is "you opened the
pull request or you have write access to the repository where the pull request was opened"; the
review App *is* the author of every comment in the threads it opens and is still refused, which is
the whole reason the command exists. Two corollaries the same draft got wrong: "widening the App
does not change that" is false (an App with `contents: write` would satisfy the rule — the design
decision is to keep the review App without it), and the same rule governs pushing to the branch.
Quote GitHub's sentence when writing the *why*; do not re-derive it from the intent.

**Why the reply grammar is exact.** GitHub stores no verdict on a thread — `isResolved` is the only
state, and the review App cannot set it — so the reviewer's reply text is the only machine-readable
signal. `isAcceptance` (`packages/daemon/src/cli/review-threads.ts`) is a strict prefix check on
the newest comment after `trimStart()`: `Accepted (round 2): …` (sjawhar/legion#966's own wording),
`Fixed in …`, a bare "fixed, thanks", or an `Accepted:` written by anyone but the thread's opener
leaves the thread open, silently and by design (the unit test pins each). The opener's own later
follow-up leaves it open too — the rule reads the newest comment only — so nobody replies after an
`Accepted:`. A worker who free-forms the reply gets a truthful `left open` line, not an error.

**GraphQL facts the command depends on** (verified live on sjawhar/legion#992 and #1003): a Bot
actor's `login` is the bare App slug (`legion-reviewer`, `legion-implementer`), never the
`<slug>[bot]` that `/gh-token`'s `appLogin` and REST `user.login` report — compare logins from the
same query, never against `appLogin`; a review thread has no URL of its own, the opening comment's
`url` (`…/pull/<n>#discussion_r<id>`) is the anchor the PR page scrolls to; two aliases on one thread,
`opener: comments(first: 1)` and `newest: comments(last: 1)`, read exactly the opener and the newest
comment without a second pagination loop; `reviewThreads(first: 100, after: $after)` pages with
`pageInfo { hasNextPage endCursor }`, and every page is read before any mutation.
