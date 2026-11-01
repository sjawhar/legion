---
title: "A queued-write UI is proven by a held route, a wire log, a remount and a refusal while closed"
category: testing
tags:
  - playwright
  - optimistic-writes
  - remount
  - tanstack-query
  - negative-control
date: 2026-10-25
status: active
module: packages/dispatch/e2e/settings.e2e.ts, packages/dispatch/web/src/__tests__/settings.test.tsx
related_issues:
  - "INFRA-266"
  - "sjawhar/legion#2178"
---

# A queued-write UI is proven by a held route, a wire log, a remount and a refusal while closed

For a control that saves optimistically or queues its saves, the browser suite needs these rows.
Each catches a defect the others cannot.

- **Hold the save and log the wire.** `page.route` holds the first PUT, and every request is
  recorded as `sent X` and `answered X`, in order. Assert what is shown during the hold and the
  exact wire after release. This catches a control that snaps back, concurrent saves, and saves
  that should have been dropped.
- **Leave and return while a save is out**, through the app's own links and never a reload. A
  reload makes a new query client and hides every remount defect.
- **Refuse a save while the page is closed**, then assert the error on return. A refusal while
  mounted proves nothing about state held by an observer.
- **Drive the rhythm a person really uses.** Rapid changes of mind against slowed saves, and
  ArrowDown on a closed `<select>`, which fires one `change` per key press. Read what the server
  stored over time, not only at the end.
- **Unit tests share one QueryClient across the unmount and the remount.** A fresh client per
  render, or a stateless stub, cannot see any of these defects. Assert the displayed value after
  each step, not the PUT arguments.
- **Barrier on the answer.** A test flag set before `route.fulfill` resolves can race the response
  it stands for. Set it after the fulfil.

## Evidence

Each of the picker's four defects on sjawhar/legion#2178 was found by a probe that held a save.
The reviewer's throwaway tests found the first two, and the tester's runs in a real browser found
the last two. Each was then pinned by one of these rows, red before its fix:

- the snap-back and the concurrent PUTs: `settings.e2e.ts:433`;
- the remount race: `:499`, red `Expected "alice" Received "carol"`;
- every intermediate pick sent: `:569`, the wire carried `sent carol … sent dave`;
- a refusal lost while Settings was closed: `:627`, the alert not found.

The unit suite of the first implementation stubbed the API without state and asserted PUT
arguments and a refetch count. It stayed green while the first defect stood. The reviewer's last
minor note, left as a Fast-follow, is the barrier above: `:643` sets its flag before
`route.fulfill` delivers the 500.
