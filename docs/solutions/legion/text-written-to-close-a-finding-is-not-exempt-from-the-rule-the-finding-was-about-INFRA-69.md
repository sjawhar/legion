---
title: "Text written to close a finding is not exempt from the rule the finding was about: re-run the finding's own criterion against your replacement, and fail a round on a prose defect you can already state"
category: legion
tags:
  - legion-worker
  - review-round
  - testing-gate
  - docs-solutions
  - evidence
date: 2026-10-21
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-69"
  - "sjawhar/legion#2088"
---

# Text written to close a finding is not exempt from the rule the finding was about

This is the generalisation of the five learnings beside it. Nothing in the phase order checks a fix
against the standard that prompted it: the tester measures the head, the reviewer reads the diff,
and neither step asks whether the replacement sentence obeys the rule the finding invoked. So the
sentence written to close a finding is the least-checked sentence in the change, and in practice
the most likely to carry the same defect.

- **Before you push a round, re-run that round's own finding criterion against the text you just
  wrote.** The question is the finding's, turned on your answer: does this new sentence state more
  than I ran, cite a record I have not opened, or give advice its reader cannot follow?
- **Name in the thread reply which command or record backs the replacement,** not only which
  commit contains it. A disposition reply that cites a commit proves authorship, not support.
- **A prose defect you can state in one sentence is a failure, not an observation.** If you can
  write the finding, fail the round. Passing a head you have already criticised and recording the
  criticism for the next role spends a full phase cycle to reach the same text.

## Evidence

Of the ten defects in INFRA-69, five were introduced by the sentence written to close an earlier
finding:

- The tester's round-2 handoff states it outright — *"Two defects, both introduced by the corrected
  bullet"*: the fix for the `du` claim invented `df -x` as a one-filesystem flag and attributed a
  cache measurement to a command its record never names.
- The memory paragraph exists only because a review thread asked for an OOM warning; the paragraph
  then quoted a cgroup path this runtime does not have and asserted per-container charging from a
  sandbox-wide counter.
- Review round 3's two findings were both in text added to answer review round 2, and the review
  said so: *"Two sentences stand, both in text added to answer me."* One hedged below its
  evidence; the other told a reader to read their own container's limits, which the rest of the
  same sentence said is impossible from inside — the defect this very note documents, written into
  the note itself.

The third rule is here because this retro's first draft got it wrong, and the fresh-eyes reading
caught it. The implementer's handoff claimed *"nothing in the process checks it twice — the
reviewer reading the fix is what caught both"*. The record says otherwise: the tester's round-6
handoff passed the head *"with two wording observations recorded for the reviewer"*, and those two
observations are verbatim the two findings the reviewer then requested changes for. The tester
found both one round earlier and had no failing channel for a defect it could state but not
measure. Passing them cost an implementing round, a test round and a review round to reach the same
two sentences.

Two structural notes from the same record, for whoever tunes the gate. The tester's pass/fail is
measurement-shaped, so on a documentation deliverable a precise prose finding has nowhere to go but
a note to the next role; binding the tester to fail on any sentence whose support it cannot locate
would have ended this issue three rounds earlier. And the architect's standing authority to cut the
offending paragraph to its two measured sentences — granted after round 4, when that paragraph was
already the locus of the defects — went unspent through three further rounds. A standing cut exists
to be spent at the round it is granted for.
