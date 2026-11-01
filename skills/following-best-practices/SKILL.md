---
name: following-best-practices
description: "Use before you choose a design, write options, or ask a person to approve a design, spec or plan."
---

# Following best practices

A design a person sends back usually fell short of an answer that already existed: a standard tool, or the vendor's documented setup. Its author knew that answer when asked, but nobody asked until the person did, after the work was done.

## 1. Before you design

Before you write a design, a recommendation or options, research and write down your answers to these three questions. Answer from the problem, before you sketch a design, so a design you already have in mind does not choose the answers.

1. Is there a standard answer? How do the tools' own documentation and two or three well-regarded projects solve this? Name the tool or practice, link the sources you read, and say why they apply here. If none applies, say why.
2. With time and money no object, what would a world-class solution look like? Name the criteria that make it the best answer for this problem.
3. Which fact of this setup, if any, makes either answer wrong here? A cost in time, money or effort is not such a fact.

Then:

- Design toward the world-class answer. A standard answer, where one exists, is the floor: go below it only for a fact of this case, and write that fact in the design. Less work, less money, no new credential, or a precedent here that does it another way is not such a fact. Name such a cost as a cost the person accepts, and fix the underlying problem instead of adding a workaround.
- Name each place the design falls short of the world-class answer, with the reason.
- Settle a disputed fact with evidence: the documentation, the code or a probe. Ask the person only for a cost to accept or a choice of priority.
- In a spec, open the `Design` part with a `### Standard answer` entry: the standard answer and its sources, the world-class answer, and a verdict. The verdict is "matches", or each named difference with the fact that requires it, or each named shortfall with its cost. The approver then sees the comparison without asking for it.

## 2. Before each approval request

Start two fresh subagents, one per brief. A fresh subagent starts with no history and gets only the text you send it. In each first message, give only the problem: what must work, for whom, the constraints and numbers, the person's own words, and the setup. Do not include the proposal's title, design, options, decisions, recommendation or conclusion: a reviewer that reads the design first tends to accept it. After each answers, send that same subagent the whole proposal, never a path, and ask it to compare. If your harness cannot message a finished subagent, give a second fresh subagent the proposal and the first one's answer. Where your harness offers a subagent on a different model family, use it for these reviews, because a reviewer on the author's model shares its blind spots.

Standard practice:

```text
First message:
<The problem only.>

From this problem alone, write how it is normally solved: the tool or practice, with two or three sources you read. If no standard practice applies, say why. Do not inspect or ask for a design.

Follow-up:
<The whole proposal.>

List each place it departs from that practice, and whether the proposal states a fact of this case that requires the departure. A departure that saves its builder work, time, money or a credential is a finding. End with FOLLOWS STANDARD, NO STANDARD, or the findings.
```

World-class:

```text
First message:
<The problem only.>

Assume time and money are no object. From this problem alone, write what a world-class solution would look like. Do not inspect or ask for a design.

Follow-up:
<The whole proposal.>

List each compromise: each place it accepts less than that answer (a manual step, a workaround, a second system, a narrower scope, a temporary fix), and whether the person asked for it. End with WORLD-CLASS, or the findings.
```

- Fix each finding from either review, or record it in the `### Standard answer` entry with the fact or the cost that requires it. Never request approval while a finding is neither. A NOT WORLD-CLASS verdict whose findings are all fixed or recorded does not block the request.
- Write both reviewers' final verdicts in that entry, with each reviewer's model and the time it answered, and name the verdicts in the approval request, so the approver sees that the reviews ran and what they found.
- Run step 2 again before a later approval request when the design, the problem, its constraints or the evidence changed since the last review.
- Stop after three rounds of step 2 on one proposal: list each change made after the last round and each finding still open, with its cost, in the entry. Review loops between agents do not end by themselves.
- When a reviewer names a standard you did not consider, go back to step 1 with it instead of defending the design.
- When a person questions the approach, in any words, run step 1 again before you answer.
