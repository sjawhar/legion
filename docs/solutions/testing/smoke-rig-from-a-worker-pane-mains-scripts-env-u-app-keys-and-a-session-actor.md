> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Pane-originated test commands must isolate inherited environment and actor identity"
category: testing
tags:
  - pane-environment
  - identity
  - test-harness
  - dispatch-api
  - secrets
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-79"
  - "sjawhar/legion#1037"
  - "LEGION-92"
---

# Pane-Originated Test Commands Must Isolate Inherited Environment and Actor Identity

The retired rig was formerly started from worker panes. Its startup procedure is suspended, but
these environment and identity lessons remain valid for supported test fixtures.

## Retained lessons

- A test command started from a worker pane must set or remove every inherited variable its result
  depends on. A shell-level mutation is not a substitute for an environment attached to the exact
  child command.
- A boot probe must exercise the native modules that the real process loads. A version command can
  pass without loading them and therefore cannot prove compatibility.
- A bearer write must include the actor shape required by the receiving API. A realistic API error
  is a contract failure to fix in the driver, not permission to bypass the actor requirement.
- Assertions about a queued issue must identify the issue's actual tree state, not infer it from a
  queue length or another test's events.

Use the daemon test harness (`packages/daemon/src/daemon/__tests__/`) and isolated real-process
fixtures for pre-merge proof. Record any necessary live restart observation on the pull request.
