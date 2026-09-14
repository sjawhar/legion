> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Fresh-state daemon coverage must not depend on pre-created test state"
category: testing
tags:
  - fresh-state
  - daemon-boot
  - test-harness
  - first-boot
date: 2026-09-12
status: active
module: packages/daemon
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
  - "LEGION-52"
---

# Fresh-State Daemon Coverage Must Not Depend on Pre-Created Test State

The retired rig pre-created directories, seeded state, and reused resources. It could therefore
miss first-boot failures. Its operating procedure is suspended.

## Retained lessons

- A boot path must be exercised with an empty state directory and an empty application state home;
  a fixture that creates them first only proves steady state.
- Use a daemon test harness and isolated real-process fixture to test the first-boot boundary. The
  fixture owns and cleans up every process, state path, and container it creates.
- When a test reveals an adjacent fault, reproduce it against the correct baseline, confirm the
  changed code is not its cause, and fix it in the current work rather than silently widening or
  deferring the change.
- Assertions about ports and processes should identify the resource that actually owns them rather
  than infer ownership from a launcher pid.

Any live observation not covered by the harness is recorded on the pull request for the operator's
next authorized daemon restart.
