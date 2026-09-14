> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Daemon test fixtures need complete fake argv handling and owned real-process probes"
category: testing
tags:
  - test-fixtures
  - tmux
  - proc-environ
  - fake-binaries
  - fail-closed
date: 2026-09-11
status: active
module: packages/daemon
related_issues:
  - "LEGION-6"
  - "sjawhar/legion#923"
  - "LEGION-40"
---

# Daemon Test Fixtures Need Complete Fake Argv Handling and Owned Real-Process Probes

The retired rig's fixtures uncovered these durable test-design constraints.

## Retained lessons

- A fake stateful binary must parse every argv shape the real command receives before it dispatches
  on a subcommand. Record full calls and assert the complete log, not an early prefix of it.
- A `/proc` environment probe needs real, long-lived processes owned by the fixture. Clean them up
  in the fixture's exit path and report variable names, never secret values.
- A test that expects an environment default must control the environment in that test. Do not rely
  on whatever its launching shell happened to export.
- A test that proves a configuration lookup does not occur uses a fake with a call log; absence of a
  binary on the test machine is not proof.
- Current daemon proof belongs in `packages/daemon/src/daemon/__tests__/` and real-process fixtures;
  live observations are recorded for the operator's next authorized restart.
