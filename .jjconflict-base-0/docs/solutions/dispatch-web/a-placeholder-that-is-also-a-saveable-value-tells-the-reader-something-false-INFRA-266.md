---
title: "A placeholder that is also a saveable value tells the reader something false"
category: dispatch-web
tags:
  - loading-states
  - error-paths
  - forms
  - tanstack-query
date: 2026-10-25
status: active
module: packages/dispatch/web/src/features/settings/ProjectsSection.tsx
related_issues:
  - "INFRA-266"
  - "sjawhar/legion#2178"
---

# A placeholder that is also a saveable value tells the reader something false

When a control's empty value (`""`, **Unassigned**, **None**) is also a real state the reader can
save, it must never stand in for data the page has not read yet.

- Until the read answers, render the control disabled, with a single option that says what is
  known: **Loading…** while the read is out, **Unavailable** once it failed. Put the load error and
  its Retry beside it. Only data that has arrived makes a missing entry mean "empty".
- Key "unknown" on `query.data === undefined`, not on `!query.isSuccess`. A refetch that fails
  after a good read keeps the last known values on screen beside the error, which is true. It
  does not blank every row. A control left editable over those values is fine when the write sets
  an absolute value.
- In a browser test, assert `toBeEnabled()` before `toHaveValue("")`. Otherwise the empty-value
  assertion also passes on the unread state, and the negative control proves nothing.

## Evidence

The Settings page built its default-assignee map from `defaults.data ?? []`. So while the read
was pending, and for as long as it had failed, every project read **Unassigned** with the picker
enabled. Unassigned is the state that ends an issue create's assignee chain with nobody, so the
page told the reader their configuration was something it was not, and invited edits from that
false baseline (review round 1, thread `PRRT_kwDORFy7ds6v_sz2`). Fixed in bf2e0861. The browser
row `settings.e2e.ts:673` (`a default assignee the page has not read yet is never shown as
Unassigned`) failed before the fix: `toBeDisabled()` received enabled.
