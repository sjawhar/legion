---
title: "A fact field the prepare stage fills is filled for the boot listing by a read, is a safe no everywhere else, and is proven on a first boot after the act"
category: daemon
tags:
  - prepare-stage
  - boot-listing
  - admission
  - design-gate
  - actor
  - status-write
date: 2026-11-01
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2619"
---

# A fact field the prepare stage fills is filled for the boot listing by a read, is a safe no everywhere else, and is proven on a first boot after the act

Extends docs/solutions/daemon/a-decider-in-a-no-io-transaction-reads-a-seq-fenced-mirror-the-first-handler-writes-INFRA-225.md.
That note sends the boot listing through the same fenced write as a live event. This one is about
a decision that needs something the event carried and the listing does not.

A decider in `intake.ApplyFact` does no I/O, so a new rule that needs outside state (who set a
label, what Dispatch shows of an issue a review is on) gets it as a field of the fact, filled by the
consumer's prepare stage (`intake.ConsumerSpec.PrepareRepository`, `PrepareArtifact`). When you
add such a field:

- **Fill it for the boot listing too, by the same read.** The listing carries no actor and no
  payload beyond the summary, so whatever the event carried that the decision needs is re-derived
  by a read per listed row, and the decision runs in `applySummary` *before* its existing
  status early-return, which would otherwise drop the row. Widen the listing loop's lazy
  `listingAncestry` build to the rows that now need a record lookup.
- **Enumerate every other construction of the fact type, and make the unfilled value a safe no,
  never an error.** A `DispatchArtifact` is built by the live decoder, the boot catch-up
  (`intake.DecodeDispatchEvent`, which carries the actor but no view), the gate seed and an API
  route's own write; only the prepare stage fills the view. A nil view queues nothing. Pin the
  unfilled case with its own test row.
- **A failed read fails the fact; never mint the zero value.** The zero (`LabelSetter == ""`) is
  also what a legitimate log yields when nothing added the label, and it falls back to the old
  behaviour, so a read error that produced it would be indistinguishable from a real answer. Return
  the error so the consumer redelivers.
- **Derive "who did this" by folding only the event types that move the attribute**, read from
  sequence 0, and pass every other event over: a comment between a session's label and a person's
  unrelated edit leaves the session the setter.
- **An actor the source names a person may still be the server's own write in their name.**
  Dispatch's carried approval is recorded under the person's actor; tell it apart by its
  discriminator (`reason`'s prefix), held as a constant that cites the producing function, and
  confirm a producer-side test pins that wording.
- **"No live tree" is not `!record.TreeLive` alone.** `TreeLive` is false for a lingering root, so a
  rule keyed on it would change a reopen inside the linger. Tie the new behaviour (here, skipping
  the root's own gate) to the whole trigger it serves, judged per gate.
- **Write the status that already means "queued" rather than teaching admission a second admissible
  state.** The daemon writes `todo` and the existing line, holds and blocked-by rule take the root;
  `record.Waiting`, `record.RankLess` and `record.Issue`'s columns stay untouched.
- **Prove the listing path on the first boot after the act.** `createConsumer` uses
  `DeliverNewPolicy` for a durable consumer with no stored position, so a daemon booted for the
  first time after the act never receives its event and only the listing can see it; a reboot
  resumes the consumer and delivers the event. Assert the one log line names the listing as the
  source and that the event source logged nothing, then boot again on the same store and assert
  nothing more is written.

## Evidence

sjawhar/legion#2619 (LEGION-755), which has the daemon queue a person's handover itself:

- The listing fill and its placement: `packages/daemon/internal/admit/admit.go:619-622` queues
  before `if summary.Status != "todo"` returns; `packages/daemon/internal/daemon/workflow.go:507-526`
  reads the setter per labelled triage row whose record is nil, inside the widened lazy-walk guard.
- The read, shared by the event and the listing: `packages/daemon/internal/daemon/repositories.go:1280`
  (`labelSetter`), called from `prepareIssue` at `:1241`; a failed read or decode is the fact's
  error (`person_handover_prepare_test.go`, "a failed read of the log", "a log that does not decode").
- The fold over event types: `packages/daemon/internal/intake/label_setter.go:25-49`, pinned by
  `label_setter_test.go` ("a person's comment between a session's label and the person's unrelated
  label counts for nothing", "an issue.closed carrying the label moves nothing").
- The constructor census and the safe no: `packages/daemon/internal/intake/facts.go:248` (the view
  "is nil for every other fact, a seed's or the boot catch-up's among them … never an error");
  `packages/daemon/internal/workflow/gate.go:611` leads with `view == nil`; the row "a fact with no
  view of the issue" at `workflow/person_handover_test.go:134`.
- The carry: `facts.go:258` (`CarriedApprovalReasonPrefix`, naming `approval_carry.go`'s
  `carryReason`) and `:273` (`PersonApproval`); the producer-side pin the reviewer recorded is
  `packages/envoy/internal/dispatch/api/approval_carry_test.go` (`carrySentence`); the scratch proof
  drives Dispatch's real carry (`person_handover_scratch_dispatch_test.go`, step 7 of A3).
- `TreeLive` and the linger: `gate.go:609-630` (`personHandoverQueues`'s last paragraph) and
  `TestAnApprovalOfALiveOrClosedRootsSpecIsAppliedToItsGateAsToday`
  (`workflow/person_handover_test.go:192`): a lingering root closed as done keeps its gate taking
  the approval; the plan recorded it as its second spec departure (D3).
- The status write, not a new state: the spec's decision "A status write, not a new admissible
  state" (dispatch://LEGION-755/spec v9); the runtime change is one `record.StatusWrite` at
  `admit.go:420` and `gate.go:578`.
- The first-boot proof: `TestABootListingQueuesAPersonsLabelAgainstScratchDispatch`
  (`person_handover_scratch_dispatch_test.go:956`), whose doc comment names the premise at
  `packages/daemon/internal/intake/consume.go:248` (`DeliverNewPolicy`) and whose assertions read
  one line with `source=listing`, none with `source=event`, and no second row after the second
  boot; the tester re-ran it on a rig of its own (the `E2E (tester)` line of sjawhar/legion#2619).
