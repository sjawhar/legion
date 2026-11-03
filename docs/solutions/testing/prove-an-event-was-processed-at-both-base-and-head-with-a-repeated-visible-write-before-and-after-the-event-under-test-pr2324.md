---
title: "Prove a browser handled an event at both the base and the head with a repeated visible write, before and after the event under test"
category: testing
tags:
  - playwright
  - dispatch
  - event-stream
  - base-run
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# Prove a browser handled an event at both the base and the head with a repeated visible write, before and after the event under test

A test asserting "this event caused at most N reads" must tell "the page handled the event and read nothing" apart from "no event arrived". The same file must also run unchanged at the base commit, where it has to fail.

- **Use only helpers the base has, and no test-only hooks.** That means no `/api/v1/events/_test/*`: a transport change can delete them, and the base may not have them.
- **The barrier is a write both commits must show:** rename the issue the page has open. Wait for *both* a new GET of that issue's detail *and* the new title in the header (`eventBarrier` in `e2e/refetch-budget.e2e.ts`). Either alone can come from something else.
- **Repeat the write every 2 s, a bounded number of times.** The first write can land before the stream has subscribed. Waiting for the stream's 200 instead ties the test to one transport.
- **Put a barrier before the measured window, and one after the event under test** when the assertion is "sent nothing". Events arrive in order, so a barrier that shows proves every earlier event was handled. Wait past the client's own refresh interval (here the scheduler's 1 s) before counting.
- **Follow a zero-read window with a positive check:** a write the page must show within the stated time (the budget test's two freshness checks). A dead stream then fails the test instead of passing it.

## Evidence

The budget and sidebar tests of sjawhar/legion#2324 use this barrier. The same file (sha256 `253c18d8…7290`) failed on its counts at the base `16e4bb0f758e`: 1,120 reads against a limit of 5, and each held sidebar read sent twice. It passed at every head, on Chromium and iPhone.

One later test, the 25-document budget test in `e2e/refresh-freshness.e2e.ts`, counts reads after its event without a closing barrier, so a stream that dropped the event would pass it. That gap is why this document asks for a barrier after the event under test.
