---
title: "A control's instruction line is re-derived for every state the control gains"
category: dispatch-web
tags:
  - accessibility
  - refusable-button
  - busy-state
  - hint-text
date: 2026-10-26
status: active
module: packages/dispatch/web/src/features/agent-view/AgentThread.tsx
related_issues:
  - "INFRA-442"
  - "sjawhar/legion#2227"
---

# A control's instruction line is re-derived for every state the control gains

Extends docs/solutions/dispatch-web/a-placeholder-that-is-also-a-saveable-value-tells-the-reader-something-false-INFRA-266.md.

## Rule

- When a control gains a state (busy, sending, held), list every text that describes it: the
  visible hint line, the button's label, `title`, accessible description, and the refusal reason.
  Check each one in every combination of the old states and the new one. On-screen text never
  tells the reader to press a key that does nothing in the current state.
- Derive the line from the same predicate that gates the action. With `RefusableButton`, `busy`
  already disables the button and names the press in flight, so no refusal is shown while busy.
  The line says what holds the key, and `describedBy` points the disabled button at that line.
- Pin the attributes as well as the text: a test that asserts only the line's words passes when the
  refusal or `aria-describedby` logic is reverted.

## Evidence

- In the live view, Send gained "Sending…" (one send out per session, `isSendDisabled`). The line
  under the box still said "Enter to send · Shift+Enter for a new line". Enter did nothing then:
  `requestSubmit` reaches `ComposerPrimitive.Root`, and `send()` returns because `canSend` is false.
  With an empty box during a send, the line said "Type a message first." beside a button reading
  "Sending…". The reviewer raised it as an important finding in round 1 of sjawhar/legion#2227.
  The diff-scoped deep review had rated it Minor.
- The fix: no refusal while sending; the line reads "Sending… Enter sends again once Dispatch
  answers." and is the disabled button's description. The committed unit test asserts the line's
  text only; the reviewer noted that reverting `&& !sending` or `describedBy` still passes it. The
  tester's throwaway browser probe asserted the button's accessible description and `aria-disabled`
  state, but it was not committed, so no permanent test holds those attributes.
- The same round found a docs sentence calling the live view "the exception to the first two rows"
  of a key table, which included `Ctrl+Enter | Send`, though assistant-ui's `ComposerInput` sends on
  any Enter without Shift. That class is
  docs/solutions/best-practices/a-documentation-sentence-is-an-assertion-evaluate-its-quantifier-its-derivation-and-its-neighbours-INFRA-212.md.
