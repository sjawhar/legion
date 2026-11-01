> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Two envoy-mode rigs on the shared LEGSMOKE project cross-admit each other's issues; a branch behind main's plugin contract runs its rig on a dedicated OMP profile built from the branch"
category: legion
tags:
  - smoke-rig
  - LEGSMOKE
  - cross-admission
  - webhook-mode
  - envoy-bridge
  - OMP_PROFILE
  - pi-legion-envoy
  - daemonApiVersion
  - LEGION_DAEMON_API_VERSION
date: 2026-09-13
status: active
module: retired smoke rig, packages/daemon
related_issues:
  - "LEGION-60"
  - "sjawhar/legion#1030"
  - "LEGION-61"
  - "LEGION-44"
symptoms:
  - "your rig's daemon grows trees for LEGSMOKE issues you never created (their parent is another rig's root)"
  - "another rig's daemon spawns an architect on your root or child and its implementer pushes to your `legion/LEGSMOKE-<n>` branch"
  - "your root issue is moved triage→todo by `legion-daemon:LEGSMOKE` before your own daemon has seen it"
  - "the daemon refuses to start: `pi-legion-envoy … speaks daemon API contract <n>; this daemon requires <m>`"
applies_when:
  - Understanding a historical cross-admission incident or validating daemon/plugin contract compatibility
  - Designing an isolated test fixture whose event source must not admit unrelated trees
---

# Two envoy-mode rigs on the shared LEGSMOKE project cross-admit each other's issues

## What happened

The LEGION-60 implementer's rig (project `sjawhar/60`, root LEGSMOKE-129, child LEGSMOKE-131) ran
while LEGION-44's tester rig was live on the same box. Within minutes each daemon had admitted the
other's issues as trees of its own: mine grew LEGSMOKE-132/133 (children of their LEGSMOKE-127),
theirs grew architects for my 129 and 131 — and their implementer pushed to *my*
`legion/LEGSMOKE-131` branch and opened a sandbox PR on it. Before I knew whose LEGSMOKE-132 was, I
moved it backlog→done through the API; its owner moved it back.

Why: every rig's `envoy-bridge.ts` subscribes to `notifications.dispatch.issue.>` — **every**
Dispatch issue project's events — and the daemon (`reducers.ts`) admits any issue whose
`issue.created` it saw and that later moves to `todo` (`reduceIssueUpdated` → `admit`). The daemon
filters by project key prefix only; every LEGSMOKE rig shares that prefix. The tester's later rig
saw the other direction too: a rig elsewhere relaying production NATS moved its freshly created
root to `todo` before its own daemon had seen the issue.

This is LEGION-61's problem to solve: test admission needs an identity that does not share an event
stream with other trees.

## Incident containment (retired)

The incident arose because every rig bridge subscribed to every Dispatch issue event and the daemon
filtered only by project-key prefix. An attempted isolated mode relayed only one tree's events, but
the other rig still received them; cross-admission remained possible. The operational containment
procedure is suspended with the rig. The durable rule is to scope admission with a test-unique
identity and to keep every stateful test resource under one fixture owner.

## A branch behind main's daemon API contract

The daemon refuses to start unless the installed `@sjawhar/pi-legion-envoy` manifest's
`legion.daemonApiVersion` equals the branch's `LEGION_DAEMON_API_VERSION`
(`verifyLegionPluginContract`; see
`docs/solutions/daemon/plugin-daemon-api-contract-version-gate.md`). When main bumps the contract
under a long-lived branch, the `legion` profile's plugin is ahead of the branch, and rebasing to catch
up is forbidden unless GitHub reports CONFLICTING (Sami, 2026-09-11).

The attempted remediation built a dedicated OMP profile with the plugin from the branch. That exact
profile-building procedure is suspended because it existed only to launch the retired rig. The
durable compatibility invariant remains: the installed plugin's `legion.daemonApiVersion` must equal
the daemon's `LEGION_DAEMON_API_VERSION`; a mismatched contract must fail explicitly rather than
falling back to a profile or a different build. The daemon test harness and real-process fixtures
cover pre-merge behavior, and a live observation is recorded for the next authorized restart.

When the contract is *equal* after a rebase — as it was for the tester's round — no dedicated
profile is needed; check `jq .legion.daemonApiVersion ~/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json`
against `packages/contracts/src/legion-daemon-api.ts` before deciding.

## Pane-environment lesson

A worker shell carries `LEGION_*`, `TMUX*`, `JJ_CONFIG`, `GIT_CONFIG_*`, `GH_*`,
`PI_CODING_AGENT_DIR`, and `OMP_SESSION_ID`. A supported fixture controls that environment on its
exact child process. It must not inherit a worker's daemon URL, state directory, credentials, or
launch prefix and treat the resulting behavior as independent evidence.
