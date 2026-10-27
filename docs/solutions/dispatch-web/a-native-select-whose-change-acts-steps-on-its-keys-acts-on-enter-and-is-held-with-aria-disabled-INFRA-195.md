---
title: "A native select whose change acts steps on its keys, acts on Enter, and is held with aria-disabled"
category: dispatch-web
tags:
  - select
  - keyboard
  - accessibility
  - firefox
  - webkit
date: 2026-10-26
status: active
module: packages/dispatch/web/src/features/agent-view/SessionSettings.tsx
related_issues:
  - "INFRA-195"
  - "sjawhar/legion#2205"
---

# A native select whose change acts steps on its keys, acts on Enter, and is held with aria-disabled

Extends docs/solutions/dispatch-web/a-write-queue-that-must-outlive-its-component-lives-in-the-query-client-and-keeps-every-guarantee-it-replaces-INFRA-266.md.

That document coalesces a select's writes. When each `change` is an action nothing can coalesce
(a session switch, a message sent), the select needs a keyboard rule instead.

## Rule

- Tell a keyboard step from a pick by the task the `change` arrives in. Every engine fires a closed
  select's `change` inside the key's own task, so mark the step in `onKeyDown` (any key but Enter)
  and `onKeyPress` (type-ahead), and clear the mark with `setTimeout(0)`. A `change` while the mark
  is set only moves the shown choice; one outside it (a pointer pick, a pick in the native popup)
  acts at once.
- `Enter` acts on the choice reached (`preventDefault`, then act). Leaving the select without
  `Enter`, or a newly reported value for it, puts it back on the reported value.
- While an action is pending, hold the select with `aria-disabled="true"`, never `disabled`: a
  disabled select loses focus and throws a keyboard reader out. A held select's keys still step;
  `Enter` and outside-task `change`s act on nothing. A step taken while held survives the action's
  end unless a new value for that select was reported meanwhile.
- Reuse the existing mark (`markKeyStep` in `features/agents/AgentMessageComposer.tsx`;
  `useKeySteps` in `features/agent-view/SessionSettings.tsx`) rather than writing a third copy, and
  run the browser rows in Chromium, WebKit and Firefox: the rule rests on each engine's dispatch.

## Evidence

- Before the rule, the tester's red row showed each ArrowDown on the focused model picker switching
  the session (17406960 with the new spec: 5 of 5 rows failed — the arrow sent a switch, the
  picker was disabled, the choice snapped back).
- `e2e/keyboard-session-pickers.e2e.ts` holds rows 11a-11d (arrows then Enter, leave by Tab or
  click, type-ahead, held picker); `packages/dispatch/e2e/playwright.config.ts` runs it in `webkit`
  and `firefox` too, and it passed in all four projects in CI (job 121436803081).
- The tester's negative control with `markKeyStep` removed from `onKeyDown` failed 4 rows while
  type-ahead still passed, since `onKeyPress` covers it: each handler pins a different path.
