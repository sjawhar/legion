---
title: "A plan decision that binds a later phase goes in the committed handoff, and a review's \"everything else holds\" is not a ruling"
category: legion
tags:
  - handoffs
  - review-rounds
  - planning
  - envoy-messages
date: 2026-10-24
status: active
module: skills/legion-worker
applies_when:
  - A planner records decisions in the Dispatch plan document and a summary in plan.json
  - A phase worker is about to act on something the plan did not spell out in its own handoff
  - A reviewer submits a round while still checking part of the diff
  - A finding or ruling lands after a review has been submitted
related_issues:
  - "INFRA-212"
  - "sjawhar/legion#2152"
---

# A decision that binds a later phase goes in the committed handoff, and "everything else holds" is not a ruling

Extends docs/solutions/legion/a-decision-taken-between-rounds-dies-with-the-pane-and-an-endorsement-is-a-claim-INFRA-62.md.
That document says a between-rounds decision must reach a durable artefact and that an
endorsement is a claim. Two deltas, each of which cost a round here.

- **Planner: a decision that binds another phase is its own enumerated field of `plan.json`, not a
  sentence inside an answer to a gap finding, and not only a letter in the Dispatch plan
  document.** `plan.json` is what a later role reads first and what a recreated pane reads at all.
  Keep the reasoning in the document; put the binding rule in the file.
- **Phase worker: open the plan document `plan.json` names before your first commit.** The `plan`
  field is a summary string, never the plan; a decision you never saw still binds you.
- **Reviewer: enumerate what you checked and name what is still open. Never write a blanket
  "everything else holds" over a clause you are still checking** — a later role, and the merger,
  read that sentence as a ruling on the whole diff.
- **A finding that lands after your review is submitted gets a thread, not a message.** An
  out-of-band ruling leaves the pull request showing one finding where there were two, and the
  only record is prose in the body. Where the message is already sent, the implementer records it
  in the body's `Threads` section and its handoff, and says there that it has no thread.

## Evidence (INFRA-212, sjawhar/legion#2152)

**The decision nobody saw.** The plan's `plan.json` carried a 90-word summary pointing at
`dispatch://INFRA-212/artifact/plan-md (version 2)`; decision **D1** — this repository commits no
check that reads repository text, on the architect's instruction of 20:20Z — lived only in that
document, and appeared in `plan.json` solely inside the *answer* to gap finding F12, as a sentence
about a test the planner had already dropped. The tester, reading its own handoff, found a real
defect and pinned it with `4b8ffbdb`, a Go test that reads `docs/kubernetes.md` and asserts its
prose. It was removed ten minutes later by the implementer on the architect's instruction
(`0c24884a`), after being left byte-identical until it passed against the corrected prose, so the
removal is a recorded deviation rather than a weakened check. The defect it pinned was genuine;
only the instrument was ruled out. A second effect worth knowing: that file's presence would have
made this a Go-code pull request under different review gates — a file one phase adds and another
removes changes the review regime of the whole pull request, not just the diff.

**The blanket endorsement.** The round-1 review, submitted at 21:43:48, requested changes with one
finding and wrote "Everything else in the diff holds: I checked all eleven rows, both citations
and all six anchor targets against the code." At **21:45:39** the same reviewer ruled by Envoy on a
second clause of the same paragraph, which it had not finished checking when it submitted. The
ruling arrived in time to join the round, so the fix cost nothing; it carries no review thread,
and GitHub shows one thread, one resolved. A merger reading threads alone would see a clean
one-finding round.

**One more fact about reading a Legion branch's history:** `plan`, `test` and `review` commits are
all authored by `legion-reviewer[bot]`, only `implement` commits by `legion-implementer[bot]`. The
tester's Go test above is authored by the review App. Do not reconstruct who did what from commit
authorship; the discriminator is the `Omp-Session:` trailer.
