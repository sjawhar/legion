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

The `legion-reviewer` GitHub App holds no `contents` permission and cannot resolve review threads:
`resolveReviewThread` returns `Resource not accessible by integration`. So in every round:

- the reviewer's `.legion/review.json` commit exists only in the shared workspace until the
  implementer's next push carries it (check `jj log` that it is an ancestor before building on it);
- the reviewer replies on each thread with its acceptance naming the fixing commit, and the
  **implementer** resolves the threads afterwards with
  `legion gh -- api graphql -f query='mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } } }' -F id=<PRRT_…>`,
  one mutation per thread, then confirms with a `reviewThreads(first: 20) { nodes { id isResolved } }`
  query that none remain unresolved;
- the reviewer's approval is the review itself; the head it approves by name is the
  implementer-pushed `.legion/` deletion.

Thread ids are the `PRRT_…` node ids the reviewer records in `review.json` (`findings[*].threadId`);
they are stable across pushes even when the thread goes `isOutdated`.
