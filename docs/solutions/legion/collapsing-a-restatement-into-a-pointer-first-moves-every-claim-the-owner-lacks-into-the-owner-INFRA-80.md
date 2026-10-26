---
title: "Collapsing a restatement into a pointer first moves every claim the owner lacks into the owner"
category: legion
tags:
  - documentation
  - agent-read-text
  - skills
date: 2026-10-26
status: active
module: skills
related_issues:
  - "INFRA-80"
  - "sjawhar/legion#2173"
---

# Collapsing a restatement into a pointer first moves every claim the owner lacks into the owner

Extends docs/solutions/legion/a-sentence-about-a-mechanism-is-written-after-reading-the-branch-it-describes-and-cites-the-line-INFRA-80.md.

That document says to state a contract once, in the file that owns it, and have other sites point
there. Following that rule has a side effect: a fact the restatement held and the owner never
stated is lost.

- **Before you replace a restatement with a link, read it claim by claim against the owner's
  text.** Move every claim the owner does not make into the owner, in the same commit.
- **The pointer's own summary is a claim about the target.** "When the pod and Sandbox go is in
  merge-gate.md" is true only if merge-gate.md names the Sandbox. Check the summary against the
  target's words.
- **A link test proves the target exists, never what it says.** A skills-guard failure on a
  respelled link is a negative control for the link, not for the claim around it.
- **After fixing one flagged sentence, read the whole owning section against every exit of the
  code it describes, once.** The flagged sentence is seldom the only one a mechanism change made
  false.

## Evidence (INFRA-80, sjawhar/legion#2173)

`ae3f92ecd6c7` replaced the `child-closed` bullet in
`skills/legion-architect/references/acceptance.md` with a pointer: "When the issue's pod and
Sandbox go is in `skill://legion-worker/references/merge-gate.md`". The removed bullet said the
Sandbox goes with the pod when a look ends with no wake queued. merge-gate.md's `pending`
paragraph (`:254-256`) named only "the pod … and the issue's disk", although `endLookPending`
(`workflow/check.go`) enqueues `record.IssueSuspend`, which deletes the Sandbox. The round's
proof was the guard failing on `merge-gates.md`. Review 5647835985 found the gap, and
`19aca29ca15c` closed it. Reading that whole section against `closeCheck`, `endLook`,
`endLookPending` and `SuspendIssue` then found one more omission no review had raised: neither
ending retires or suspends anything of an issue back in the workflow (`InWorkflow`). One round
earlier, review 5647511344 had found `:282-284`'s "for a child" in the same section: a line from
main that every earlier round had read past.
