---
title: "A statement about evidence is held to the standard of the evidence it describes"
category: best-practices
tags:
  - evidence
  - verification
  - checks
  - review
date: 2026-10-24
status: active
module: process
related_issues:
  - "INFRA-71"
  - "sjawhar/legion#2092"
---

# A statement about evidence is held to the standard of the evidence it describes

Hold every sentence about what was checked to the standard of the check itself.

- Write a check that can fail where the fault would be. One that cannot reports clean and is worse
  than no check, because it leaves a reader believing the area was looked at.
- Never waive a check because you predict its result. The prediction is not evidence; the value of
  a check is not predicted by its expected outcome.
- Before trusting a clean result, confirm the query was capable of a hit: search for a string you
  know is present, or break the thing on purpose and watch the check go red.
- Describe what was verified exactly. Understating misleads as much as overstating: both leave a
  reader with a false picture of what was run.
- Disclose only a gap you have confirmed exists. A disclosure that invents its gap commits, in the
  statement about the work, the error the work was being checked for.
- A proof that cannot distinguish the thing under test from the thing never having started is not a
  proof.

The last rule is the sharpest operational form of the others: before quoting a pass, ask which
outcome the check would have produced had the subject never existed, and if the two are the same,
the check measured nothing.

## Evidence

Three instances, from one change. The first two are one failure in different clothes — a check
reporting clean while unable to fail — and the third is the same failure with its sign reversed.

**A check that could not fail where the fault was.** A repository's documented lint command resolved
its tool by name through a package runner rather than from the project's own dependencies. In an
environment where the dependencies were not installed, the runner fetched an unrelated package of
the same name from the public registry, checked nothing, and exited 0. Two written notes already
described this failure; both asked a reader to remember it rather than making the command fail, so
it happened again. The fix — make the command resolve the pinned binary or exit non-zero, and have a
required check enforce it — is filed and has not shipped. That is recorded here as it stands: a
lesson whose every instance ends in a fix teaches that these are caught.

**A census argued to be worth skipping, then incapable of a hit.** Before applying a new capability
to a cluster, someone required an inventory of what was already running there. Its reviewer
predicted the result would be empty and argued the step was cheap to skip; the waiver was refused on
principle and the inventory ran. One of its rows then searched for a string that appears in no
source file of the repository it was searching, so it could not have matched the objects it was
looking for, and it reported clean — *the query was incapable of a hit, which is worse than a query
that found nothing*. When that was caught and the row was corrected, the real look found pods and
compute classes the repository had never declared, and the apply was retracted on the strength of
them. Both halves belong to one instance: the prediction said there was nothing to find, and the
finding sat beside it.

**A disclosure that invented its gap.** A reviewer was told three times that a review step was
required which the process does not define. It drafted an honest disclosure for its review body
saying that step was missing, then read its own instructions, found the step named nowhere, and
dropped the sentence — which would have put, in a statement about the review, the exact error the
review objects to in code: a claim of a gap that does not exist. The same error was made one level
up in the same hour, by an architect who told a coordinator that two findings came from two model
families reaching the same place independently when they came from two agents with different briefs
on one model: a mechanism inferred from an outcome.
