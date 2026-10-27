---
title: "A state that switches off a background duty does its last run at the transition, and a durable sweep covers what arrives after"
category: daemon
tags:
  - state-transitions
  - background-work
  - repair-pass
  - rolling-deploy
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "INFRA-158"
  - "sjawhar/legion#2233"
---

# A state that switches off a background duty does its last run at the transition, and a durable sweep covers what arrives after

Extends docs/solutions/daemon/a-state-that-starts-lasting-days-gets-a-census-of-every-inbound-fact-and-its-re-entered-rule-tells-nobody-INFRA-229.md.

- At plan time, ask of every state change: what does this state stop doing, and what can still be
  owed, or still arrive, once it stops?
- The transition does that duty's last run itself, from the durable rows, inside or right after the
  transaction that enters the state.
- Whatever can arrive after the transition gets a sweep that runs at boot and on an interval:
  - a write that commits between the last run and the transition;
  - a write made through another process's in-memory state;
  - a transition made by the previous build during a rolling deploy.
- State the window the sweep leaves open (at most one interval plus one pass) where operators read it.

## Evidence

In sjawhar/legion#2233, a closed issue gets no document settlement. Three cases were stranded:

- an answer or resolve that commits between the pre-close settlement and the close's transaction;
- a write into another process's room after the close, since the close reaches only its own
  process's rooms;
- closes made by the old build during the roll.

Each needed the close's sweep (`sweepIssueAskBlocksTx` in
`packages/envoy/internal/dispatch/api/issue_ask_lifecycle.go`, written from the database rows), or the
repair pass `api.RunIssueAskRepair`, which runs at boot and every 10 minutes. Tests:

- `issue_close_races_test.go` in `packages/envoy/internal/dispatch/api`;
- `TestTheRepairSweepResolvesADecisionBlockWrittenOnTheOtherProcessAfterTheClose` in
  `packages/envoy/cmd/dispatch/multiprocess_test.go`.

The roll window is in `packages/envoy/cmd/dispatch/README.md`.
