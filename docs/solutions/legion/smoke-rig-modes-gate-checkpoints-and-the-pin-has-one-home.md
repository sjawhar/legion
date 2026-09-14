> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Retired smoke rig: recorded configuration must gate a test rather than fabricate a result"
category: legion
tags:
  - test-harness
  - recorded-configuration
  - fail-closed
  - single-source-of-truth
date: 2026-09-13
status: suspended
module: packages/daemon
related_issues:
  - "LEGION-10"
  - "sjawhar/legion#957"
  - "LEGION-32"
  - "sjawhar/legion#983"
---

# Retired Smoke Rig: Recorded Configuration Must Gate a Test Rather Than Fabricate a Result

The retired rig had a webhook mode, a Dispatch-ingress mode, and its own copy of the OMP pin. Its
procedure is suspended, but its design lessons remain useful for daemon test fixtures and operator
verification at the next authorized restart.

## Retained lessons

- A test whose configured event source cannot produce the required event must report that it is
  blocked before making a network request. It must not report a false failure or infer a mode that
  was never recorded.
- A related configuration axis must be recorded independently. One setting cannot safely stand in
  for a different fact about where an event came from.
- A value owned by another component is read from that component's one source of truth and verified
  through the consumer's real lookup. Copying a version or a fallback path turns an environmental
  accident into an apparently supported configuration.
- A failure message must name an action that changes the outcome. A generic remedy is misleading
  when the code path does not consult the value it tells an operator to change.

Use the daemon test harness (`packages/daemon/src/daemon/__tests__/`) and real-process fixtures for
pre-merge proof. Record any required live observation on the pull request for the operator's next
authorized daemon restart.
