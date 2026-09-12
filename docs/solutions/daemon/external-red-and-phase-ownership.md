---
title: "A By-Design Red PR Exhausts fixAttempts, and worker/started Steals the Active Phase"
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

# A By-Design Red PR Exhausts fixAttempts, and worker/started Steals the Active Phase

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
the merge queue and the reviewer know why it is red, and expect the `pr-blocked` wakes.
**Daemon follow-up (not this PR):** either do not count a head whose diff is confined to
`.legion/` toward `fixAttempts`, or let the architect acknowledge a known-external red so it stops
accumulating.

## 2. `/worker/started` overwrites `phases[issue]` unconditionally

`api/routes/workers.ts` (the `/worker/started` handler) ends its claim update with
`ctx.deps.state.phases[issue] = { phase: role, sessionId };` for **whatever** role is registering.
The prompt paths (`promptExistingWorker` in `processes.ts`, the `pendingAssignment` delivery,
`/worker/ready`) also set `phases[issue]` to the prompted worker, and regression tests pin that
each of them does. What no code guards against is the *next* registration: a tester whose pane
(re)registers, a merger booting, a resumed sibling — any `/worker/started` on the issue after the
architect prompted the implementer replaces `phases[issue]` with the newcomer.

`phase/complete` then checks `phase.phase === grant.role && phase.sessionId === grant.sessionId`
and answers 409 `Phase for <KEY> is no longer owned by this worker` to the worker that actually
holds the task. That is what the LEGION-23 implementer saw after the cleanup round: its
`legion handoff complete` 409'd although its own claim was valid, because `phases[LEGION-23]` had
been re-pointed at the previous tester's session.

**What a worker should do:** report the 409 to the architect over Envoy with the work already
pushed; the architect treats the message as the completion (the daemon-side status write did not
happen) and moves the issue on by hand. Do not retry in a loop — the phase record is wrong, not
transient.
**Daemon follow-up (not this PR):** `/worker/started` should set `phases[issue]` only when no
active phase exists or the registering session already owns it; a registration by a role other
than the active phase's must never overwrite it. Alternatively key `phases` by role.

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
