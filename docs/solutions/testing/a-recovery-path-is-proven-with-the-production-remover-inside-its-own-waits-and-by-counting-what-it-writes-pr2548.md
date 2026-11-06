---
title: "A recovery path is proven with the production remover, inside its own waits, and by counting what it writes"
category: testing
tags:
  - recovery
  - browser-tests
  - interleaving
  - re-entrancy
date: 2026-10-31
status: active
module: packages/dispatch
related_issues:
  - "sjawhar/legion#2548"
---

# A recovery path is proven with the production remover, inside its own waits, and by counting what it writes

A recovery test is evidence only when the broken state arrives the way production makes it, the
test acts during the waits the recovery opens, and it asserts **how many** effects were written
rather than that the right one was.

- **Break it the way production breaks it.** A test that produces the broken state by a local
  shortcut proves the shortcut. Find out what actually removes the thing — a server sweep, another
  writer's edit, a reconnect — and use that, even when it costs the row a minute of real time.
- **Act inside every wait the recovery opens.** The moment a handler gains an `await`, the user can
  press the button again, a second refusal can arrive, and two recoveries can run at once. Write
  the row that does it.
- **Count the effects.** Assert the number of marks, writes or calls, not only that one of them is
  right. A recovery that leaves a second, orphaned effect behind passes every assertion about the
  first.
- **Never let the fixture supply the ordering the code is supposed to establish.** A stub that
  resolves "already sent", or a pre-state set by hand just before the call, deletes the very
  sequence under test. Hold the real signal open across the assertion instead.
- **A fix that adds a wait, a branch, a fallback or a new refusal makes new states reachable, and
  the round that ships it covers them.** Enumerate what a user can now do inside the new window and
  what inputs can now reach the new branch, and write one case for each. Three of the four defects
  found after this change's first review were created by the previous round's fix, and the
  reviewer labelled two of them "new with item 1" and "new with item 7".
- **One case per reachable precondition of a message, not one per message.** Where a refusal can be
  reached from two branches, its wording can be true in one and false in the other; see
  `docs/solutions/testing/a-test-that-recognises-a-refusals-wording-must-recompute-the-claim-the-wording-makes-pr2433.md`
  for recomputing the claim rather than the text.

## Evidence

All from sjawhar/legion#2548, a browser recovery that re-attaches a comment whose provisional mark
is gone.

- Its range-tracking plugin's one test removed the mark with a local `tr.removeMark` and called
  that "another writer". Production removes a mark through a whole-document replace from
  y-prosemirror, under which the plugin could never answer. The test was green for a year of
  rounds; the plugin was deleted.
- `whenSent`, which waits for a freshly written mark to reach the server, was proven by a fixture
  stubbing `async () => true` and by a unit test that set the pending count to 1 by hand *before*
  the call. Neither ran the real order, and in production the wait answered from a report taken
  before the mark existed, so the retry raced the mark it was waiting for.
- A Retry pressed during a 10-second bridge wait started a second recovery: two marks written, the
  second request stale, one mark orphaned in the shared document. The test that catches it counts
  `remarkMark` calls and expects exactly one.
- Two browser rows had to take a provisional mark away while leaving the reader's words in place.
  A `dispatch doc-edit` cannot do it: an edit that rewrites exactly the marked run leaves the mark
  on the new text, and where the words read identically elsewhere no quote-anchored edit can even
  name the reader's copy (`TARGET_AMBIGUOUS`). Both rows take the tab offline past the mark's
  lifetime and let the server's own sweep remove it.
- The negative control for the block-edge rule is a fixture in which the flattened text spells a
  hard break exactly as it spells a block boundary: restoring the character test puts a fresh mark
  on the next paragraph's line, with placement `prefix "Notes:\n"`.
