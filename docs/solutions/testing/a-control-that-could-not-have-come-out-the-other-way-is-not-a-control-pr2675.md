---
title: "A control that could not have come out the other way is not a control"
category: testing
tags:
  - negative-control
  - evidence
  - acceptance-criteria
  - false-green
date: 2026-11-02
status: active
module: scripts/e2e
related_issues:
  - "sjawhar/legion#2675"
---

# A control that could not have come out the other way is not a control

Extends docs/solutions/best-practices/a-negative-control-that-cites-an-existing-test-is-read-before-it-is-cited-INFRA-188.md.

That note says an assertion of absence can pass vacuously and must be paired with one that fails.
This adds the two shapes in which a *control* is vacuous, and the question that catches both
before the work starts.

- **Ask what the control prints at the other commit.** If the answer is "the same thing", it is
  not a control of this change. It may still be worth running as a regression check — say which
  of the two it is.
- **A control on a consumer that is indifferent to a producer-side change is green on both
  sides.** Put the control on the surface the change touches, run the same input through the two
  builds, and vary one thing.
- **Removing a required value proves only that the value is required.** To show the system reads
  the value rather than counting it, supply a well-formed wrong one and name the refusal it earns.
  That control needs a stand-in that holds state; one that answers a canned success can prove
  neither.
- **An acceptance criterion is reviewable for this before it is run.** A plan that writes a
  green-on-both-sides control has already found the gap; it must then name the criterion that
  does discriminate, rather than leaving the weak one as the evidence.

## Evidence (sjawhar/legion#2675, INFRA-609)

The change removed three fields from the Envoy listener's push envelope that no consumer reads.

**The consumer-side shape.** The plan's criterion A1b was a whole-daemon live proof —
`scripts/e2e/no-required-checks.sh`, a real daemon over a real PostgreSQL and a real
`nats-server` — driven with push envelopes carrying none of the three. It passed. It also passed,
unmodified, at the parent commit, where the driver sends all three, because the daemon ignores
them either way: the plan said so itself, and the pull request body repeats it. A1b's own control
(delete `after` from the driver's payload → `no_required_checks_live_test.go:315: timed out
waiting for the daemon's processed_events row`) proves the rig is live and nothing about the
removal. What discriminated was the producer-side pair: the listener binary built from the parent
commit `a22c2370` published thirteen payload keys with `changed_paths = .legion/implement.json`,
and the one built from `c1196cd3` published ten, through the same driver on the same captured
GitHub body.

**The missing-value shape.** The same pull request repaired a live proof whose completions named
no Dispatch handoff record. Two controls were run on it at one head. Removing the record name
again gave `400 HANDOFF_RECORD_REQUIRED` — which shows only that the field is required, a fact
`packages/daemon/internal/api/handoff.go:275-278` states in source. Seeding the record under
`ses_someone_else` instead of the completing claim's own session gave `403
HANDOFF_SESSION_MISMATCH handoff record … was written by session ses_someone_else, not by this
claim's session ses_implementer`, which is the one that matters: it shows the daemon reads the
seeded record back and compares its author, so the repair is a record and not a non-empty string.
`handoff.go:299-317` is the read-back the second control exercises and the first does not.

That second control exists only because the stand-in holds state: `liveDispatch.handoff(key, word,
session)` at `packages/daemon/internal/daemon/live_proof_test.go:326-335` keeps the record it
seeds, and `showHandoff` at `:396-410` answers Dispatch's own `404 HANDOFF_NOT_FOUND` for an id it
does not hold.
