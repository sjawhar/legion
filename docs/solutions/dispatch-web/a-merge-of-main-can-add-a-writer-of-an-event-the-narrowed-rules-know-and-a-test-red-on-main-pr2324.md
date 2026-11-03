---
title: "A merge of main can add a new writer of an event type the narrowed rules already know, and can bring in a test that is red on main itself"
category: dispatch-web
tags:
  - event-refresh
  - forward-merge
  - playwright
  - semantic-conflict
date: 2026-10-28
status: active
module: packages/dispatch
---

# A merge of main can add a new writer of an event type the narrowed rules already know, and can bring in a test that is red on main itself

Extends docs/solutions/dispatch-web/narrowing-a-broad-cache-invalidation-starts-from-every-read-and-the-writes-that-move-what-it-shows-pr2324.md, whose "after every merge of main" rule names only new event types.

- **Search main's range for every place that appends an event, not only for new types.** A new caller that appends a known type (`ask.resolved`, `ask.opened`) with the old payload builder sends no `thread_logins`, so the client falls back to the broad refresh for every such event. Nothing fails, and the budget test does not catch it unless it drives that writer. Run `jj diff --from <old base> --to main@origin` and grep the server packages for `appendEvent(` and the payload constructors. Each new call site must use the narrowed builder (`events.AskEventPayload`), and each event the new call site sends ahead of an old event can change what the old event must name: after a close withdraws the asks, `issue.closed` names nobody.
- **When a test that main added fails on the merged head, run main's own copy on an archive of main before changing anything.** Two pull requests can each pass their own runs and still conflict once both are on main: one adds a test, the other changes the behaviour that test assumes. Export main with `git --git-dir="$(jj git root)" archive <main sha> | tar -x -C $TMPDIR/main-<n>`, `bun install --frozen-lockfile`, and run only that test. If it fails there too, it is main's defect, not the merge's.
- **Fix it in the pull request when the test is in code the pull request owns, and tell the controller.** Keep the test's subject and move its setup to what main now does. Prove the rewritten test with a negative control that removes the rule under test. Main's CI can be queued for an hour, so other pull requests see the red only later: give the controller the test name, the main commit it fails at, and the pull request that fixes it.

## Evidence

- Main's #2233 (a close withdraws the issue's open asks, a reopen restores them) added `recordWithdrawnAsksTx` and `restoreIssueAsksTx` with `model.NewAskEventPayload`. On this branch they now use `events.AskEventPayload` (`packages/envoy/internal/dispatch/api/issue_ask_lifecycle.go`, merge `85597891`). `TestIssueEventsSayWhatTheWriteChanged` asserts that the withdrawal and the restore name the thread's logins and that the close names `[]`.
- #2225's "a route change that closes the issue moves the open card's route row" failed 2 of 2 on an archive of main `08572e01`, where #2233 had made the close withdraw that card. Commit `4682d7bf` moves the route in a status write instead, and asserts that the close shows "Withdrawn when alice closed the issue". It passed 3 of 3, and failed at "Routed next (e2e)" with the `route` check taken out of `askThreadsIssue`. Record: https://github.com/sjawhar/legion/pull/2324#issuecomment-6470853481.
