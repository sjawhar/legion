---
title: "State the cases and let the reader draw the rule: a general sentence about scoped behaviour is wrong in a corner"
category: best-practices
tags:
  - documentation-drift
  - pull-request-body
  - review-rounds
  - proof
date: 2026-10-25
status: active
module: packages/envoy/internal/broker
applies_when:
  - You are writing a pull request body sentence about behaviour that has conditions
  - You reach for "exactly N ways", "at every moment", "never", "cannot" or "the only"
  - A reviewer or tester has already falsified one sentence of the same document
related_issues:
  - "INFRA-228"
  - "sjawhar/legion#2187"
---

# State the cases and let the reader draw the rule

Extends docs/solutions/best-practices/a-documentation-sentence-is-an-assertion-evaluate-its-quantifier-its-derivation-and-its-neighbours-INFRA-212.md.

That document says to evaluate a quantifier over the enumerated set before writing it. The delta
this one adds: for behaviour whose truth depends on a condition, **the enumeration does not save
the sentence, because the general form is the defect**. Write the cases; the rule is the reader's
to draw.

- **A count is a quantifier.** "A name moves in exactly two ways" fails the first time someone
  finds a third, and the correction ("exactly three") fails again. Replace the count with the
  cases and no number: *what moves it is this, and this*. A reader who needs the count can count.
- **Write the condition into the sentence that needs it, not into a neighbouring one.** A
  paragraph whose first half discloses a window and whose last sentence denies it is two
  statements that contradict each other, and the reader believes whichever one they read.
- **A sentence that separates a strong property from a messy one is the most dangerous sentence
  in the document.** It is written to reassure, so it reaches for the widest form of the
  reassurance. If you need one, scope it explicitly on both sides: what the property holds
  against, and what it does not.
- **A pull request body cannot be amended after the merge.** A sentence that is false in a corner
  either gets its clause now or ships wrong, and a merged body is what the next reader plans a
  deployment from.
- **The same false claim usually has a second home.** When you correct a sentence, search the
  document for its other copies: a thread ledger line, a commit message, a code comment restating
  it. Correct them in the same edit or the reader finds the uncorrected one.

The cure that works is mechanical: for each sentence, name the case it is true of, then ask what
the system does outside that case, and write that too. If the answer is long, the behaviour has
more shapes than one sentence can carry, which is the signal to write a list.

## Evidence

INFRA-228 took five corrections of this exact kind across eight implementing rounds, on one
paragraph of one pull request body (sjawhar/legion#2187). Each was a reviewer or tester
reproducing the corner on real processes:

- "A pod's token is accepted only while it is bound to a live pod" — the check is offline, so a
  deleted pod's token works until it expires.
- "A session name is held by the identity that first claimed it" — a lapse handed it to another
  principal.
- "A name moves in exactly two ways" — a configuration change was a third.
- "Two replicas whose configurations disagree cannot take a name from each other" — during a
  rollout they do, by turns.
- "At every moment the name is held by an entry some running configuration fixes it for, and a
  principal no configuration designates is refused throughout" — a rollout that newly fixes a
  held name leaves it with a principal no configuration designates, and answers it.

The last was written by the issue's architect, asked for in the round before to separate a strong
binding from an untidy window — the reassurance sentence this document warns about. The
implementer's own first replacement for it reached for another universal ("it cannot be acquired
by one"), false for a name whose retention has run out, and was caught before the edit went up.
Two of the eight rounds existed only to correct sentences, and the paragraph settled only once it
stated cases and no rule at all.
