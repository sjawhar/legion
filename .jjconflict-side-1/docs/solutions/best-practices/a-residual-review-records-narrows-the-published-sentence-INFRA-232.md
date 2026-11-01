---
title: "A residual that review records narrows the published sentence too"
category: best-practices
tags:
  - documentation-drift
  - proof
  - review-rounds
date: 2026-10-25
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-232"
  - "sjawhar/legion#2195"
---

# A residual that review records narrows the published sentence too

Extends docs/solutions/best-practices/a-documentation-sentence-is-an-assertion-evaluate-its-quantifier-its-derivation-and-its-neighbours-INFRA-212.md.

When a review records an unmeasured exception to a behaviour (in `Not proven / risk` or in a
handoff's residual risk), find the sentence on the page that states that behaviour. Give it the
same condition in the same round. If you don't, the pull request says "not proven" while the page
states the behaviour as a property, and the page is what a reader keeps.

Check this before the approval, not at retro: a retro may change only `docs/solutions/`, so a
sentence found too strong there costs a full test and review round.

## Evidence

sjawhar/legion#2195: the reviewer's residual risk says that a create the server finishes after
the trap's `kubectl delete -l` has listed the Jobs leaves a Job until its deadline, and that this
was not measured. The merged trap paragraph says, with no condition, that a stop that landed while
the create was in flight "finds the Job the create made". It rests on two such stops, both of
which the label delete caught. The recovery paragraph's printed-label delete covers the residual
case, but the trap paragraph does not point to it. The fresh-eyes retro read found the gap after
approval, too late for retro to change the page.
