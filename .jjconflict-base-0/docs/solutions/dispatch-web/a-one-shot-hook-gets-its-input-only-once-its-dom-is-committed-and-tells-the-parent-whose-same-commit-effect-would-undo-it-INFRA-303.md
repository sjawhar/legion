---
title: "A one-shot hook gets its input only once its DOM is committed, and tells the parent whose same-commit effect would undo it"
category: dispatch-web
tags:
  - react-hooks
  - uselayouteffect
  - effect-order
  - scroll-restoration
  - tanstack-query
date: 2026-10-26
status: active
module: packages/dispatch/web/src/features
related_issues:
  - "INFRA-303"
  - "sjawhar/legion#2221"
---

# A one-shot hook gets its input only once its DOM is committed, and tells the parent whose same-commit effect would undo it

Extends docs/solutions/dispatch-web/a-dom-measuring-hooks-two-stale-views-the-commit-a-runtime-owns-and-the-node-a-boundary-replaced-INFRA-43.md.

That document covers a DOM a third-party runtime commits late. This is a third stale view. Here the
hook's input arrives before its DOM, and a parent's effect undoes what the hook did.

- **Withhold a one-shot input until the DOM it acts on is committed.** A hook that does something
  once, on the first defined value of an input, spends that once on whatever is on the page. A list
  built from several reads (`useQuery` each) has a value as soon as one read answers, while the
  component still renders its loading text. Pass `undefined` until every read has answered.
  Compute that from the same `loading`/`failed` values the early returns use, so the condition that
  hides the list and the one that withholds the input cannot drift apart.
- **Do not guard inside the hook with a DOM query.** A layout effect re-runs only when a dependency
  changes. When the late read answers without changing the input (the newest item stays the
  newest), nothing re-runs the effect, so a hook that skipped its first run "because no card was
  there" never places the reader. The caller must hold the input back. The hook cannot wait for it.
- **A parent's layout effect runs after the child's, in the same commit.** React commits layout
  effects child first. A parent that restores something on a state change will overwrite what the
  child just did in that commit; `IssuePage`'s tab-scroll restore on `[activeTab]` is one example.
  Make the hook call back (`onFirstPlacement`) from inside its layout effect at the moment it spends
  the one-shot. The parent then forgets its stale record before its own effect reads it. Fire it
  only on the first time, so an ordinary return still restores.
- **Hold the callback in a ref synced by a layout effect declared before the acting effect.** That
  keeps a new callback identity from re-running the acting effect, and the ref is current in the
  same commit.

## Evidence

`packages/dispatch/web/src/features/conversation/ConversationTab.tsx` passed `useFollowLatest` the
newest item's time from whichever of its asks, comments and messages reads had answered. On a phone,
with the asks read held, the hook's first placement found no `[data-turn]` and scrolled to 0. When
the asks answered, the list rendered with the newest message under the fixed composer at `scrollY`
0. That failed 20 of 20 times in a browser row forcing that order. Withholding `latestItemSeq` while
`loading || failed` fixed it (sjawhar/legion#2221). The plan rejected a DOM guard in the hook for the
reason in the second bullet.

The tester then found the parent case. Leave the Conversation while it loads, let the list render in
the hidden panel, and come back. The hook placed the reader on the newest card, then `IssuePage`'s
parent layout effect restored the `scrollY` 0 recorded while only the loading text showed. That
failed 4 of 4 times. `onFirstPlacement`, wired to delete `panelScroll.current.conversation`, fixed
it. Two mutations bound the fix. Removing the forget made the leave-while-loading row fail again,
per the tester. Firing the callback on every enabled run made `doc.e2e.ts` "tab round-trips keep one
document connection, the typed text, and the Conversation scroll position" fail 2 of 2
(`Expected: 500, Received: 0`), so that row guards the ordinary restore.
