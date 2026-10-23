---
title: "A retired model is swept by belief, then by symbol"
category: best-practices
tags:
  - documentation-drift
  - sweep
  - code-comments
  - corrections
date: 2026-10-23
status: active
module: packages/envoy
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A retired model is swept by belief, then by symbol

Extends docs/solutions/best-practices/deleting-a-ui-concept-is-a-vocabulary-sweep-and-every-sentence-it-falsifies-is-re-derived-from-the-code-INFRA-42.md.
That document warns that rewording a claim carries it across in words no grep holds. The delta
here is the answer to that warning, and the order to search in.

- When a round establishes that a mechanism does **not** do what the tree says it does, the
  correction is a sweep of the retired *model*, not of the sentence that was flagged.
- Sweep twice, with different instruments. Once **by belief** — the claim in each of its
  phrasings. Then once **by symbol** — every mention of the mechanism's constant, every mention
  of the retired concept inside the owning package. The second pass is what finds what the first
  structurally cannot.
- Prioritise, in this order: the mechanism's **only caller's** doc comment; the regression row's
  **premise** and its failure message; any comment reasoning about a *count* of callers. Those
  are the words someone reads at the moment they decide whether the mechanism may be re-tuned or
  deleted.
- Where the mechanism is separable and no test covers it, say so where it is documented: the
  comment is then the guarantee, not a description of one.
- A claim that a correction is **complete** is a claim like any other and needs the same
  evidence. Record the sweep's commands, its hits and each hit's disposition. A record that
  overstates a correction is worse than one that omits it, because it stops the next person
  looking.

## Evidence

A liveness ping had been thought to depend on a caller's retry ceiling; a round measured that
false and moved the guarantee into the component owning the connection. Two comments were
corrected and the pull request body said the correction was complete.

**Six more places still taught the retired model**: the ceiling's *only* caller's doc comment
(where a maintainer stands when re-tuning or deleting it), the closing clause of the function
that consumes it, a regression row's premise, that row's failure message — read at exactly the
moment someone decides what the row is for — and two statements in the listener's own test,
contradicted by the row fifty lines below them. The next round was an entire review round in
which every diff hunk was a comment, a doc string or a test message.

The sweep by symbol then found a seventh nobody had listed: a pool-size comment that still
reasoned about a single caller where three permanent listeners now dialled through it. Five times
on that change a widened sweep axis found a site the review had not named; none came from reading
harder.

Related: docs/solutions/legion/text-written-to-close-a-finding-is-not-exempt-from-the-rule-the-finding-was-about-INFRA-69.md,
and docs/solutions/legion/an-operator-facing-sentence-outlives-what-it-points-at-INFRA-31.md —
whose stronger answer, a fence that derives the sentence from the code, was unavailable here
(nothing derives a Go comment) and is the shape to reach for when it is.
