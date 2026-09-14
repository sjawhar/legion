---
title: "A flake spec names no cause before the retained trace is read: four 'timing' failures were two self-cancelled requests, one editor bug, and no slow server"
category: testing
tags:
  - playwright
  - flake
  - trace
  - e2e
  - dispatch
  - spec
  - root-cause
date: 2026-09-14
status: active
module: packages/dispatch/e2e
related_issues:
  - "LEGION-94"
  - "sjawhar/legion#1084"
  - "LEGION-135"
symptoms:
  - "expect(locator).toContainText(...) timed out at 1000 ms on a runner that passed the same code an hour earlier"
  - "expect(locator).toBeVisible() timed out at 5000 ms after a page.reload() or page.goto()"
  - "a different browser scenario fails on each re-run attempt while ~200 others pass"
---

# A Flake Spec Names No Cause Before the Retained Trace Is Read

## What happened

The Dispatch dashboard's Playwright suite (`packages/dispatch/e2e`, about 206 scenarios on two
projects) failed four times on 2026-09-13 on pull requests that changed nothing under
`packages/dispatch`. Each failure was one or two scenarios timing out on a wait while the rest
passed, and each was a different scenario. The first version of LEGION-94's spec read the error
text — `toContainText("hello from bob") timed out at 1000 ms`, `toBeVisible() timed out at
5000 ms` — and named the cause: cross-user propagation is slower than one second on a loaded
two-core runner, so raise the assertion timeout.

The planner downloaded the four failing attempts' retained traces before writing the plan and
read `test.trace` (the action timeline in monotonic milliseconds), `*-trace.network` (one HAR
entry per request with the server's `wait`), and the screencast frames. Not one of the four was a
slow propagation:

- **Slowest server answer across all five traces: 15 ms** (`GET /api/v1/inbox`). Every other
  request answered in under 11 ms. Both document websockets in each doc trace exchanged frames
  throughout the failing window.
- **Two were the test cancelling its own request.** The inbox-ordering scenario clicked
  "Ask back" for two asks and called `page.reload()` in the same loop: the second POST
  `/api/v1/issues/<key>/comments` started 3 ms before the reload and never completed (HAR
  status −1). The approval scenario clicked "Answer", passed `toHaveCount(0)` 27 ms later
  because the Inbox removes the card optimistically, and called `page.goto()` 30 ms after the
  POST `/api/v1/asks/<id>/answer` began; that POST never completed either. In both, the state
  the assertion waited for never existed on the server. No timeout would have passed them.
- **Two were an edit-loss bug in the collaborative editor.** In the two-users scenario, bob's own
  page — 14 ms after his typing returned — showed his `Enter` newline and none of his fourteen
  characters, inside the fixture's trailing code block, while alice's remote cursor sat in the
  same block. The text never entered the typist's document, so alice could not receive it.
  Filed as LEGION-135 against `@sjawhar/proof-editor` 0.3.6 with the two traces attached.

The one-second literal was still worth removing — 36 assertions on a two-core shared runner
promised a latency the tests cannot own, and the number came from a product design target in
LEGION-2 that a browser test proves the wrong way — but it was the hygiene, not the fix. Had the
plan stopped at "raise the timeout", the two racing scenarios would have failed identically at
15 s and the ten-attempt proof would have restarted on the first red attempt.

## The rule

A spec about a flaky browser test names no cause until the retained trace of a failing run has
been read. Until then the spec says "cause: not yet read" and the plan's first task is the trace.
The error text names only the assertion that gave up, never why the state it waited for did not
arrive.

What to read, in this order, for each failing scenario:

1. `test.trace` — the action timeline. Find the assertion that failed, then the last action the
   test performed before it. A `page.reload()`, `page.goto()`, or `context.close()` within a few
   milliseconds of a click that starts a write is the test racing itself.
2. `*-trace.network` — the HAR. For the request the assertion depends on, read `time` and
   `timings.wait` (the server's share). A status of −1 is a request the browser abandoned; look at
   what navigation started right after it. A websocket's `time` is the span from its first to its
   last frame (playwright-core 1.63 `harTracer`), so a socket that stays busy through the failing
   window rules out a stalled connection.
3. The screencast frames on **the typist's own page**, not only the receiver's. If the typed text
   is missing from the page that typed it, propagation is not the question.

Then classify: a test-owned race (fix the test — wait for the server-visible effect of the write
before reloading or navigating: the reply rendered in its thread, an `expect.poll` on the API
until the record exists), a product or library bug (file it, with the traces, and keep the
scenario off that path only if the scenario's purpose is something else), or a genuinely slow
path (fix the server, and say which request took how long).

## Getting the traces

The workflow retains traces only on failure (`trace: "retain-on-failure"`) and uploads them from
the `dispatch` job as an artifact. Since LEGION-94 the artifact is
`dispatch-e2e-test-results-${{ github.run_attempt }}`, so each re-run attempt's traces are
addressable by name; see
[`github/run-download-by-name-picks-one-of-several-same-named-artifacts.md`](../github/run-download-by-name-picks-one-of-several-same-named-artifacts.md)
for why the unnumbered name lost two of the three attempts here. Unzip `trace.zip`; the files
above sit at its root, the frames under `resources/` named by the `file` field of each
`screencast-frame` event (not by `sha1`).

## Fixing the two races

The approval scenario now polls the API for the recorded review before leaving the Inbox
(`packages/dispatch/e2e/approval.e2e.ts`):

```ts
await card.getByRole("button", { name: "Answer" }).click();
await expect(card).toHaveCount(0);
await expect
  .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
  .toBe("approved");
await page.goto(`/issues/${issue.key}`);
```

The inbox-ordering scenario's fix landed on `main` first (sjawhar/legion#1078) in a different
shape — the two clarifications are created through the API and awaited, so there is no click for a
reload to race — and this branch's version was dropped at the conflict-forced rebase. Either shape
is right; the rule is that a write the test starts is awaited to its server-visible effect before
the test navigates.

## Which DOM assertions prove the write landed

Whether a DOM assertion after a click is server-confirmed depends on the mutation behind the
control, and in `packages/dispatch/web` the answer differs per control:

- **"Answer"** (`useAskAnswerForm.ts`, the `answer` mutation) has an `onMutate` that removes the
  card from the `["inbox"]` cache before the POST resolves. `expect(card).toHaveCount(0)` therefore
  passes with the write still in flight — that is exactly the 27 ms window the approval trace
  showed — and proves nothing; the API poll is the confirmation.
- **"Ask back"** (the `clarification` mutation) has no `onMutate`; the reply text appears in the
  card's thread only after `onSuccess` invalidates `["ask-thread", id]` and the refetch renders
  it. `expect(thread.getByText(reply)).toBeVisible()` is therefore server-confirmed, which is why
  the sibling test at `inbox.e2e.ts` ("a clarification moves an ask under Waiting on agents") can
  reload right after it, and why the plan modelled the fix on it. A fresh-eyes reviewer at retro
  flagged that sibling as the same race; reading the mutation refuted it.

Rule: before trusting a DOM assertion as the wait for a write, read the mutation's `onMutate`. An
optimistic update makes the assertion satisfiable from the cache; poll the API instead. Cache-
rewriting `onMutate` users in this app at the time of writing: `useAskAnswerForm.ts` (answer),
`IssueHeader.tsx` (priority, pin), `useMarginItems.ts` (comment accept/reject/resolve/reopen),
`SettingsPage.tsx` (repository mappings) — `grep -rn onMutate packages/dispatch/web/src` is the
current list.

## After a conflict-forced rebase, check which fix survived

This branch's inbox-ordering fix and `main`'s #1078 fixed the same scenario in different shapes;
the rebase conflicted, and resolving it to `main`'s side left this branch's commit empty. A green
run after a rebase proves that *some* version of the test passes, not that the fix you wrote is in
it: diff the rebased file against both the pre-rebase head and `main` (`jj diff --from <old-tip>
--to @ -- <file>`; `jj diff --from main@origin --to @ -- <file>`) and say in the PR which shape
survived and why it closes the same race. The general mechanics of a sibling PR rewriting your
lines are in
[`legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md`](../legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md).
