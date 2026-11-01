---
title: "A finding can be right about the defect and wrong about the record behind it: open the record before you write the fix, and grep the fix for residue of the wrong premise"
category: legion
tags:
  - legion-worker
  - review-round
  - review-threads
  - evidence
  - docs-solutions
date: 2026-10-21
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-69"
  - "sjawhar/legion#2088"
---

# A finding can be right about the defect and wrong about the record behind it

A reviewer who has read the diff carefully can still have misread the record it cites. Accepting
the finding is not accepting its evidence, and the two fail separately.

- **Open every record a finding cites before you write the fix,** including the ones you already
  believe. The finding tells you where to look; it does not tell you what is there.
- **An instruction that repeats a finding's premise inherits its error.** A brief written from a
  finding is not independent of it.
- **After disputing a number, grep your own answering text for anything derived from it.**
  Correcting the premise in a reply does not sterilise the draft you wrote beside it.
- **Say in the thread which record you opened and what it says,** so the reviewer adjudicates the
  evidence rather than your word.

## Evidence

Review round 2 on `sjawhar/legion#2088` found that the note printed Legion's `defaultResources`
numbers as the deployment's standing budgets when the deployment had changed them. That was right,
and it is the finding the round fixed. Its evidence was not: it named
`dispatch://INFRA-51/comment/a9f52454` as a **memory** change. Opened in full, that comment is
`agent-infra` #80 rebalancing every role's **ephemeral-storage** request and limit after the tester
began being evicted too, its figures summing to the cluster's 10Gi per-pod ephemeral ceiling.
Memory was not changed that night: `dispatch://INFRA-51/comment/12ce8e6b` reads a live issue pod
created after the applies and finds exactly the repository defaults, recording that the deployment
stack sets no memory at all.

The architect's instruction for the fix repeated the finding's reading, and the subagent brief
written from that instruction repeated it again, so the draft carried it into the note. Opening all
three comments before committing is what stopped it, and the correction went into the reviewer's
own thread rather than only into the commit message.

The residue rule has its own instance one round later. Having corrected the
`512Mi`-is-ephemeral-not-memory error in a thread reply, the same push wrote *"the smallest role's
memory budget may be well under a gigabyte"* into the note — a figure that could only have come
from those same `512Mi`. The smallest memory limit in any record the note has is 1Gi. The premise
had been disputed in one artifact and acted on in another, in the same round.
