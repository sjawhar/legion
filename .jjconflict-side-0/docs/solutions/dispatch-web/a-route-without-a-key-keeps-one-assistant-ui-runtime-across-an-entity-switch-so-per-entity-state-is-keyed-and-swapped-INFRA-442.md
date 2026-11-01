---
title: "A route without a key keeps one assistant-ui runtime across an entity switch, so per-entity state is keyed and swapped"
category: dispatch-web
tags:
  - assistant-ui
  - react-router
  - composer
  - per-session-state
  - refusal-restore
date: 2026-10-26
status: active
module: packages/dispatch/web/src/features/agent-view
related_issues:
  - "INFRA-442"
  - "sjawhar/legion#2227"
---

# A route without a key keeps one assistant-ui runtime across an entity switch, so per-entity state is keyed and swapped

Extends docs/solutions/dispatch-web/a-write-queue-that-must-outlive-its-component-lives-in-the-query-client-and-keeps-every-guarantee-it-replaces-INFRA-266.md.

That document is about state lost when a component unmounts. This is the opposite case: a component
that stays mounted while the entity its URL names changes.

## Rule

- A `<Route path="/x/:id">` with no `key` keeps the page, every hook in it and a library runtime
  built once (`useExternalStoreRuntime`) when only `:id` changes. Anything held for "the" entity (a
  draft, an error line, a busy flag, a late async answer) then shows on the next entity. Key each by
  the entity id, and swap the library's own shared state (the composer text) in a `useLayoutEffect`
  on the id, so the box never paints one entity's text under another.
- Capture the entity id before the `await`. When the answer arrives, write into the shared
  library state only if a ref says that entity is still on screen; otherwise write into that
  entity's kept state, which the swap puts back when the reader returns.
- assistant-ui's thread composer empties the box when the send starts, before `onNew` runs. A
  caught error there loses the person's text. Have the send return an outcome (`"sent" | "refused"`)
  and never reject into the library, and restore with `runtime.thread.composer.setText`. The
  library's `MessageNotSentError` restore knows only the one shared composer and its newest send,
  not which entity sent it. Hold one send per entity with `isSendDisabled`, so two restores never
  overlap.
- An external-store runtime that starts no run of its own passes `isRunning: false` explicitly.
  `isRunning` is a composer gate, not a rendering flag: while it holds without the `queue`
  capability, Send is disabled and Enter is swallowed. Leaving it out is not neutral either: the
  library then reads a trailing running assistant message as a running thread. Each message's own
  `status` still drives the streaming display.
- Prove it with in-app navigation that keeps the page mounted: `<Link>`s inside the test's
  `MemoryRouter`, or `history.pushState` plus a `popstate` event in Playwright. A `page.goto` or a
  reload remounts the page and hides the whole class of bug.

## Evidence

- `packages/dispatch/web/src/app.tsx` routes `/agents/:sessionId/live` with no key. At the base,
  text typed for session A stayed in the box after in-app navigation to session B, ready to send
  to B; a refused send emptied the box for good; and the error line of A's refusal showed on B.
- The fix (`AgentRuntimeThread.tsx`): a `sendingTo` set and `shown`/`drafts` refs, the session
  taken before the await, restore into the box or the kept draft, and the `useLayoutEffect` swap.
  `AgentConversationPage.tsx` keeps errors in a `Map` by session and bumps `ownSendCount` only when
  the landed send's session is still on screen.
- `AgentConversationPage.test.tsx`'s `SessionLinks` moves the one mounted page between two
  sessions. The tester's mutation `sending = sendingTo.size > 0` (a busy state shared across
  sessions) failed the late-refusal test.
- `isRunning: isRunning(conversation)` (any message streaming) disabled Send whenever a turn
  streamed, and a late streaming snapshot could leave it stuck. Restoring that rule fails three
  unit tests, and the e2e row asserts exactly two assistant turns after a mid-turn send, so the
  empty upcoming placeholder the library adds for a running thread cannot come back unnoticed.
