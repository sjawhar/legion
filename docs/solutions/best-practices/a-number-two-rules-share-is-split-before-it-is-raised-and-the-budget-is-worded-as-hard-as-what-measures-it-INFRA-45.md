---
title: "A number two rules share is split before it is raised, and the budget is worded as hard as what measures it"
category: best-practices
tags:
  - limits
  - sweep
  - skills
  - e2e
date: 2026-10-22
status: active
module: skills, scripts/e2e, packages/contracts
related_issues:
  - "INFRA-45"
  - "sjawhar/legion#2097"
---

# A number two rules share is split before it is raised, and the budget is worded as hard as what measures it

- Before you change a site a sweep found, decide which of two things its number is: **a
  restatement of the server's refusal**, or **somebody's own budget that happens to equal it**.
  Only the first follows the constant. The two look identical in a grep and read identically in
  prose, and nothing in a diff tells them apart — the test is what the sentence is *for*.
- A budget gets its own sentence, with its reason attached, and keeps its old value. The reason is
  what stops the number drifting back into permission the next time someone sweeps: "of at most
  2,000 characters, since it is a report rather than a log of the day" survives a raise that
  "at most 2,000 characters" does not.
- Do not restate the server's refusal beside the budget. A report of 2,000 characters never meets
  a 32,000-character refusal, so the restatement is the same conflation in a second sentence.
- Grep `scripts/e2e/` for the number in any instruction you edit. A live proof that measures the
  budget is what makes the budget real, and the instruction's wording must then be **exactly as
  hard as the assertion**: `of at most 2,000 characters` against `<= 2000`, never "about".
- When they disagree, fix the instruction, not the assertion. The assertion is the only thing that
  measures the budget; loosening it leaves a budget nothing measures, which is advice.
- A fix to prose is unreviewed code. When a review hands you an exact wording and you write a
  different one, re-check it against the thing that measures it before you push.

## Evidence (INFRA-45, sjawhar/legion#2097)

Raising Dispatch's comment and message body cap from 2,000 to 32,000 UTF-16 units swept every text
that stated 2,000. One of them was `skills/legion-controller/SKILL.md:341`, which read "One
`dispatch message … --body-file -` of at most 2,000 characters, written as `skill://dispatch`'s
'Writing for the human' says". That sentence carried two facts at once: the server would refuse
more, and a daily report to a person should be about that long. The two had the same value by
accident. The sweep raised it to 32,000 and `scripts/e2e/stage4b-sandbox-tree.sh:5460` was relaxed
to `(.payload.body | length) <= 32000` to match, which told every controller it may write a
32,000-character report to a human reading it on a phone — a product change nobody asked for. The
reviewer caught it as a minor and reported it to the architect as a product consequence rather
than holding it; the architect ruled it a regression and sent the round back.

The fix split the sentence: the report keeps its own budget with its reason, the server's refusal
is not restated there at all, and the assertion and its README row went back to 2,000. Two
counterpart sites were checked and left alone, because both state the server's cap plainly under
wording that says so: `skills/legion-retro/SKILL.md:181` and
`docs/site/src/content/docs/dispatch/for-agents.md:101`.

That fix then produced the next round's only held finding. The new budget read "under **about**
2,000 characters" while `stage4b-sandbox-tree.sh:5461` asserts `<= 2000` with no margin, inside
`until_true 300 … report_posted` whose timeout ends in `fail` (`scripts/e2e/lib/rig.sh:79`): a
controller obeying the instruction with a 2,050-character report would burn 300 seconds and fail
that row blaming a *missing* report. Both halves of the thermonuclear pair and the tester found it
independently. It was fixed at `042d4a764166` by hardening the instruction to `of at most 2,000
characters` and leaving the assertion as it was.
