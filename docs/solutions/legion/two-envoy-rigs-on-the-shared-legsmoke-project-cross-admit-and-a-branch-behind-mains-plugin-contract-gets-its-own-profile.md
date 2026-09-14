> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Retired smoke rig: shared event feeds must not cross-admit unrelated issue trees"
category: legion
tags:
  - event-isolation
  - admission
  - daemon-api-version
  - test-harness
date: 2026-09-13
status: suspended
module: packages/daemon
related_issues:
  - "LEGION-60"
  - "sjawhar/legion#1030"
  - "LEGION-61"
---

# Retired Smoke Rig: Shared Event Feeds Must Not Cross-Admit Unrelated Issue Trees

Two test daemons consuming a shared Dispatch event feed admitted each other's issue trees. The rig
that exposed that failure is retired; its operational procedure must not be recreated.

## Retained lessons

- Admission must be scoped by an identity that is unique to the test, rather than a project prefix
  shared by concurrent test runs.
- A test environment needs one owner for every stateful resource. Never repair another test's
  issue, pane, process, state, or event stream.
- A branch whose plugin and daemon contracts differ must fail its compatibility check explicitly;
  a fresh profile or an unstated fallback is not evidence of compatibility.
- Pre-merge coverage belongs in the daemon test harness (`packages/daemon/src/daemon/__tests__/`)
  and real-process fixtures. Any remaining live observation belongs on the pull request for the
  operator's next authorized daemon restart.
