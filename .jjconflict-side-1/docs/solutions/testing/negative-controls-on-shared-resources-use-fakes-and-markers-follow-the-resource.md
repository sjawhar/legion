---
title: "A negative control whose failure mode is destruction runs against a fake or a decoy, and a marker file is written only after the resource it describes exists"
category: testing
tags:
  - negative-control
  - fake-docker
  - fake-tmux
  - shared-machine
  - smoke-rig
  - marker-file
  - write-ordering
  - design-review
date: 2026-09-13
status: active
module: retired smoke rig
related_issues:
  - "sjawhar/legion#1010"
---

# A negative control whose failure mode is destruction runs against a fake or a decoy, and a marker file is written only after the resource it describes exists

Two learnings from LEGION-41 (PR #1010) that together explain both live incidents on
2026-09-13 and three of its six review rounds.

## 1. Never probe a destructive path with real tooling against a shared resource

The failure mode of "does `down.sh` leave the *other* rig's container alone?" is that it does
not. Both incidents were negative controls run with the real `down.sh`, real `docker`, and real
`tmux` on a machine where the resource under test belonged to someone else:

- 09:34Z: a review subagent, "does a teardown from a throwaway directory refuse?" — it did not
  refuse; it killed LEGION-16's tmux server.
- 11:44:50Z: a tester, "does the other-rig branch leave `legion-smoke-nats`?" — assigned to run
  it on real docker from its own scratch directory; the shared checkout was mid-rebase and the
  script that ran was the pre-fix one, which removed LEGION-44's container. The architect
  recorded the assignment itself as the error.

The rule, now in the LEGION-41 spec (acceptance 8's check) and in every later round's tester
evidence:

- **Fake the tool.** A `docker`/`tmux` first on `PATH` that logs its argv and answers
  `container inspect` / `port` / `has-session` the way the case needs, and exits 1 for anything
  else. Assert on the argv log (`rm -f legion-smoke-nats` present or absent), never on the
  machine. `down.test.sh` and the tester's round 5–6 evidence do exactly this.
- **Or use a decoy you own.** A container or server with a *distinct* name that you created this
  session with your own project, so the worst case destroys only yours.
- **Read the real state read-only** (`docker ps`, `docker port`, `tmux has-session`) to learn
  the shape the fake must reproduce — the real `docker port` prints two lines, IPv4 then IPv6,
  and the fake must too.
- An assignment that says "run the other-rig branch on real docker" is the error, whoever wrote
  it. Push back before running it.

## 2. Nothing the teardown keys on may exist before the resource it describes

`up.sh` writes two files that `down.sh` later trusts: `legion.yaml` ("this directory started a
rig") and `nats-container` ("this is the container to remove"). Three review rounds moved
their write points:

| round | finding | fix |
| --- | --- | --- |
| 3 (thread r3999296900) | record written before the port asserts and `ensure_nats`; a refused start left a record naming a container that never existed | record after `ensure_nats` returns |
| 4 (tester) | `legion.yaml` written before `ensure_nats`; a refused start left `legion.yaml` without a record — the exact shape the legacy fallback treats as "a rig from before the record existed" | `legion.yaml` after `ensure_nats` too |
| 5 (reviewer nit) | with both writes adjacent, an interruption between them could still leave `legion.yaml` without a record | record **first**, then `legion.yaml`: an interruption leaves record-without-yaml, which the teardown ignores |

The design rule that would have collapsed those into one round: **list every file the teardown
reads, and for each one name the point in the start script after which the thing it describes
definitely exists; write it there, and write the more-consequential marker last.** Every
refusal point (`fail`) must precede all of them. The test for it is mechanical — for each
refusal point, a case that triggers it and asserts none of the marker files exists afterwards
(`up.test.sh`'s two acceptance-7 cases).

A related review observation worth carrying: rounds 5 and 6 answered one question — "is the
legacy fallback's ownership test exact against real `docker port` output?" — in two
installments. Once a fallback that removes a shared resource exists, ask about the exactness of
its gate in the same round you add it.
