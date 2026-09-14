> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Destructive test cleanup must key every action to a resource the fixture owns"
category: legion
tags:
  - cleanup
  - shared-machine
  - ownership-test
  - test-fixtures
  - fail-closed
date: 2026-09-13
status: active
module: test-fixtures
related_issues:
  - "sjawhar/legion#1010"
---

# Destructive Test Cleanup Must Key Every Action to a Resource the Fixture Owns

The retired rig destroyed another test's tmux server and NATS container. Its cleanup procedure is
gone; the safety lesson remains.

## Retained lessons

- Before removing a process, server, container, or directory, identify it through a record written
  by the same fixture that created it. Never derive it from an ambient environment variable or a
  shared default name.
- Parsing an ownership record fails closed: a missing, unreadable, malformed, or directory-shaped
  record means no destructive action.
- Compare resource identity exactly. A numeric port match must parse each reported port and require
  equality; substring or prefix matching can select another test's resource.
- Fixtures must use non-default values and boundary-shaped values so a constant or substring check
  cannot pass by coincidence.
- A fixture's cleanup must tolerate a resource concurrently disappearing after ownership is proven.

The current daemon test harness owns and cleans up its real-process fixtures. Do not recreate the
retired rig's cleanup workflow.
