---
title: "A gate that ignores events for subjects it has not registered yet must read the source of truth at registration: the human approval that landed before register_gate"
category: daemon
tags:
  - design-gate
  - register_gate
  - event-ordering
  - dispatch
  - reducers
  - review-finding
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/api/routes/issues.ts, packages/daemon/src/daemon/reducers.ts
related_issues:
  - "LEGION-20"
  - "sjawhar/legion#975"
---

# A gate that ignores events for subjects it has not registered yet must read the source of truth at registration

## The race

The design gate is event-driven: `artifact.approved` for the registered document at its current
version opens it. The reducer correctly ignores an approval for a document no gate names yet —
nothing else could match it. Dispatch never re-emits that approval: `dispatch_request_approval`
on an already-approved document answers "already approved" and opens nothing. So a human who
approved the spec from the document header while the architect was still writing, or who answered
the Inbox question during the model turn between `dispatch_request_approval` and `register_gate`,
produced an approval the daemon threw away, and the architect parked forever on a wake that could
not come. The ask-based gate this replaced had the same window; the reviewer found it in round 1
(the one blocking thread on PR #975).

## The fix

`handleGatesRegister` computes the gate from the prior record and, when it would be closed and
`gates.design` is not `off`, reads the issue from Dispatch once *before recording anything*
(`getIssue`, the same read the state migration's resolver makes) and seeds the gate from the
registered document's `approval`: `approved` at the latest version → `approvedVersion` set, gate
open, one `design-approved` wake through the same publish path the gate-off self-approval uses;
`stale` → the older approved version recorded, gate closed, no wake; any other state → no
approval. Dispatch's `latest_version` raises `latestVersion` in every case. A failed read is a 502
that records nothing, so `register_gate` retries; a document the issue does not carry is a 404
naming both. Humans still write every approval — the daemon only copies one a human already gave,
so `gates.design: off` remains the only non-human path (commit `620b9362`).

## The reusable rule

If a component (a) reacts to events about registered subjects and (b) discards events about
subjects it does not know yet, then registration is a moment when state may already have moved,
and the events that moved it are gone. At registration, read the subject's current state from
its source of truth and seed from it — through the same "closed became open, so wake" path the
event handler uses, so there is one place that decides. Do the read before the write: a failed
read must leave nothing recorded, so the caller's retry is a clean second attempt.

Corollaries that mattered here:

- **Do not fix ordering by reordering the skill.** Calling `register_gate` before
  `dispatch_request_approval` would close the Inbox window and leave the document-header window
  open. The read closes both.
- **Seed through the source's own semantics.** Dispatch's `approval.state` already distinguishes
  `approved` (at the latest version) from `stale` (an older version); the seed mirrors the
  reducer's rule (below-latest is recorded silently) rather than inventing a second one.
- **Prove both orderings live.** The tester ran the header approval before the request and the
  Inbox answer between the two calls and confirmed exactly one `design-approved` each time; the
  unit tests cover approved / stale / awaiting / read failure / unknown document.

## Related

- `docs/solutions/daemon/plugin-daemon-api-contract-version-gate.md` — the contract that carries
  `register_gate`'s request shape.
- `docs/solutions/skill-patterns/one-tool-result-feeds-the-next-call-name-the-field-print-the-value-refuse-the-wrong-kind.md`
  — the other half of registering the right document.
