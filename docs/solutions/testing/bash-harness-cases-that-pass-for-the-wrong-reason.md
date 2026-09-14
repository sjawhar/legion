> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Bash harness cases must prove the guarded behavior rather than a coincidental failure"
category: testing
tags:
  - bash-harness
  - mutation-testing
  - set-e
  - process-group
  - fake-binaries
date: 2026-09-13
status: active
module: test-harnesses
related_issues:
  - "LEGION-10"
  - "sjawhar/legion#957"
---

# Bash Harness Cases Must Prove the Guarded Behavior Rather Than a Coincidental Failure

The retired rig's shell harnesses exposed general rules that still apply to a maintained bash
fixture.

## Retained lessons

- Isolate one guard per negative case and satisfy every other guard. Mutate each guard to prove
  that its corresponding case fails.
- For a `set -e` probe, assert the success-only side effect is absent. An `if` condition and a
  declaration with command substitution can mask a failure while the script exits successfully.
- A group-kill fixture needs a surviving child process; a singleton process cannot show that a
  process-group kill was reduced to a pid kill.
- A PASS line may name only the cases actually exercised. Boundary cases belong beside the claimed
  set, not in an untested label.
- A fixture must represent a state that a compliant real system can reach. A hand-built state that
  satisfies contradictory assertions proves only that the fixture parser accepts it.

Run current daemon behavior through its TypeScript test harness and real-process fixtures, not
through the retired rig.
