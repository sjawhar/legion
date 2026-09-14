> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "A real daemon boot on a fresh state dir beside a busy smoke rig: scratch NATS and ports, no pre-created directories, and what to do with the bug it finds"
category: testing
tags:
  - smoke-rig
  - real-boot
  - fresh-state
  - XDG_STATE_HOME
  - legions-registry
  - scope
  - adjacent-finding
  - nats
date: 2026-09-12
status: active
module: packages/daemon, retired smoke rig
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
  - "LEGION-52"
  - "sjawhar/legion#1018"
  - "LEGION-41"
  - "sjawhar/legion#1010"
  - "LEGION-94"
---

# A Real Daemon Boot on a Fresh State Dir Beside a Busy Smoke Rig

## Context

The LEGION-21 rebase round needed one real boot of the branch daemon to prove a seam (the
worker-stream listener binding `config.bind`) — enough to see both `listening on` lines and stop.
The shared smoke rig could not provide that isolation: it hard-coded the NATS container name and
ports while a sibling tree held them. Bringing it up would have torn down a sibling's run. The
historical isolated boot took under a minute, needed no shared resource, and found a first-boot
bug the original setup structurally could not find.

## Retained isolation constraints

The historical proof isolated its state directory, ports, NATS server, and event stream, and it
confirmed readiness from the worker-stream log, sockets, API state, and persisted state. Those
facts remain useful, but the recipe that assembled and ran the scratch daemon is suspended. Current
pre-merge coverage uses the daemon test harness (`packages/daemon/src/daemon/__tests__/`) and
fixture-owned real processes. Any live daemon observation is recorded on the pull request for the
operator's next authorized restart.
## Why pre-created state hid a first-boot bug

The retired setup pre-created its application-state directory before daemon startup. A first boot
with an empty state home instead failed opening `legions.json.lock`: `withRegistryLock` used
`O_CREAT|O_EXCL` before `writeRegistry` created the parent directory. The defect existed on main;
the production daemon and prior test environments already had the directory. The fix created the
parent before taking the lock and added a concurrent registry-write test.

Rule: coverage of a boot path must include an empty state directory and empty application state
home. A fixture that creates them first proves steady state only.

## What to do with a bug that is not yours

The ENOENT was outside the PR's declared file list. The disposition that worked, and that the
repository's no-deferral rule requires:

1. Reproduce it on `main`, not only on the branch, and record the exact command and error.
2. Confirm the branch did not touch the file (`jj diff --from main@origin -- <path>` empty).
3. Name the one-line fix.
4. Hand the decision to the architect in the handoff, with all three facts.

The architect ruled it rides in the PR as its own commit — because a PR hold was in force,
"its own PR" would have been an indefinite deferral — with its own reproduction (pre-fix boot
fails, post-fix boot creates `legions.json` under the fresh directory), unit test, and PR-body
line. Neither silent option is acceptable: fixing it unannounced widens a reviewed diff; leaving
it "for later" is the deferral the rules forbid.

## The same contention one day later, and what has changed since (LEGION-52)

LEGION-52's rounds (2026-09-13) hit every part of this note's setup, on a box with three live
rigs, and some of the workarounds are now history:

- **The fixed container name was corrected on `main`.** Earlier isolated boots used a copy because
  the shared setup could collide with another test's NATS container and ports. The later correction
  derived resource names per project; this is historical evidence that fixtures need unique,
  recorded ownership rather than shared defaults.
- **A shared Dispatch project made an unfiltered event bridge unsafe with siblings.** The bridge
  relayed every project event, so a daemon could admit another test's roots. The durable
  requirement is an event source scoped to the fixture's own tree; the former relay procedure is
  suspended.
- **A docs-only branch still runs the whole package workflow, flakes included.** A doc-comment
  change under `packages/contracts/src` matches `packages/contracts/**` in
  `.github/workflows/envoy-and-contracts.yaml`'s `dispatch` filter, so every push of the branch —
  including the `.legion/`-only handoff pushes each phase makes — ran the seven-minute Dispatch
  dashboard browser suite, which failed three consecutive attempts on one run with a different
  fixed-timeout scenario each time (`doc.e2e.ts:105`, `inbox.e2e.ts:236`, `approval.e2e.ts:24`;
  filed as LEGION-94 with the four failure records). The rule held: re-run the failed job
  (`legion gh -- run rerun <run-id> --failed`), never push a commit to refresh CI. Budget the
  round for it: five implementer rounds on a docs PR meant five full workflow runs.
- **The scratch runner scrubs the pane's own identity.** A worker pane carries `LEGION_*`,
  `DISPATCH_URL`/`DISPATCH_TOKEN_FILE`, `OMP_PROFILE`, and (on this box) a stale `LEGION_OMP_PATH`
  pointing at an rpcfix build whose natives fail to load under a fresh profile. The runner that
  worked `env -u`'d all of them and set `OMP_PROFILE`/`PI_PROFILE` to the scratch profile, so the
  daemon resolved the pinned OMP through `mise where` and read the scratch profile's plugin
  manifest — the one the negative and positive controls vary. Without the scrub, the first attempt
  refused on `dispatch_project is required` and the second probed the wrong OMP.

## Related

- `docs/solutions/testing/smoke-rig-fakes-and-live-run-notes.md`: what the full rig needs and
  what its fakes cover.
- `docs/solutions/testing/live-proof-over-real-daemon-state-snapshots.md`: proving a change over
  a copy of real state, the complementary discipline for non-boot changes.
