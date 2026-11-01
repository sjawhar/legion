---
title: "A scratch-rig proof of an actor-keyed rule sweeps its fixtures' identities, sends a human-only route its exact body, and anchors each negative on the applied event"
category: testing
tags:
  - scratch-dispatch
  - test-fixtures
  - actor
  - negative-control
  - processed_events
  - admission_cap
date: 2026-11-01
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2619"
---

# A scratch-rig proof of an actor-keyed rule sweeps its fixtures' identities, sends a human-only route its exact body, and anchors each negative on the applied event

Extends docs/solutions/testing/prove-an-event-was-processed-at-both-base-and-head-with-a-repeated-visible-write-before-and-after-the-event-under-test-pr2324.md.
That note's barrier is a later visible write, for a browser stream the test cannot read into.
When the system under test persists what it applied, anchor on that row instead.

When the daemon starts to judge *who* acted, and you prove it on the scratch Dispatch rig
(`blockers_scratch_dispatch_test.go`'s rig: a real Dispatch with its events on, a real Postgres,
a real NATS, the daemon process-in-process):

- **A fixture's identity is now an input.** Grep every rig helper that creates or edits the entity
  under the convenient credential (the trusted `X-Dispatch-User` header) and convert the ones whose
  act the new rule would answer, with the reason in the helper's doc comment. A labelled `triage`
  fixture a person creates is, under the new rule, a handover the daemon queues before the test's
  own write. Give the helper an identity parameter rather than a second helper.
- **A session's write carries its session in the body's `actor`** (`{"kind":"session","id":…}`):
  Dispatch's create, update, upload and edit routes require it of a bearer caller. A session's read
  sends no body.
- **A human-only route is refused for what the caller is only on the route's own body.** Dispatch
  rejects unknown body fields before `requireHuman`, so a bearer's `POST
  /api/v1/artifacts/{id}/reviews` with an `actor` field is 400, and the 403 `HUMAN_ONLY` the proof
  wants is reached only with the documented `{"state": …}`.
- **Anchor every "wrote nothing" on the daemon having applied the very act**, never on a wait: the
  `processed_events` row the fact's transaction writes (`dispatch-<event id>`), the other row the
  same event queues (a `triage` notice), the `dispatch_issues` mirror at the act's own sequence, or,
  for an act the API refused and made no event of, your later act on the same issue, which the
  daemon applies in order. Read the negative after the anchor shows.
- **Hold the cap so nothing races promotion.** Run at `admission_cap: 1` with an occupant in the
  slot; every root the rule queues then stands in `admission.waiting`, readable from
  `GET /legion/v1/state`, and a `backlog` move releases a visible slot.
- **Make a multi-path decision observable from one log message with a `source` field**, so the
  proof counts per source: it asserts both that the expected path logged once and that the other
  paths logged nothing (the boot proof: `listing` = 1 and `event` = 0).
- **Leave `EnvoyURL` at the rig's unreachable default** so notice rows stay in `outbox` to be
  counted; leave the design gate at its default when a case needs a registered gate to stay closed.

## Evidence

sjawhar/legion#2619 (LEGION-755), `packages/daemon/internal/daemon/person_handover_scratch_dispatch_test.go`
unless named otherwise:

- The identity sweep: `blockers_scratch_dispatch_test.go:1007-1040`; `createBlockersIssue` creates
  `asSession` (`:1022`) and `createUnhandedIssue` stays `asPerson` (`:1029`), because the ten
  person-created labelled `triage` fixtures would have been queued by the daemon ahead of each
  test's own `todo` (the plan's D9).
- The session actor in the body: `withSessionActor` at `:111`; the exact body for the human-only
  route: `approveDocument` at `:341` and its doc comment (the worker who wrote it met the 400 first).
- The anchoring rule: the file's header at `:42-46`; `applied` (`processed_events`) at `:494`,
  `mirrored` (`dispatch_issues` at the retitle's sequence) at `:508`, `notQueued` at `:638`; the
  bearer's 403 anchored by the person's later approval of the same document (A3 step 3).
- The cap: `newPersonHandoverRig(t, 1)` at `:741`, `:834`, `:957`, with an occupant admitted first.
- One message, one `source` field: `packages/daemon/internal/admit/admit.go:423` (`event`,
  `listing`) and `packages/daemon/internal/workflow/gate.go:581` (`approval`); counted by
  `personHandoverLogLines` at `:590`; the boot proof's `listing` = 1 and `event` = 0 at
  `TestABootListingQueuesAPersonsLabelAgainstScratchDispatch` (`:956`).
- The tester's independent rig (`.legion/LEGION-755/test.json`, `implementerProof.how`) re-ran the
  three tests and added live negatives on the same anchors: a park and a label removal between the
  queue and the delivery, each read after the delivery's own log line.
