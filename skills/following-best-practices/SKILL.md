---
name: following-best-practices
description: "Use before choosing a design, writing options, a spec or plan, or requesting approval."
---

# Following best practices

A design a person sends back usually skipped practices that everyone who builds such things knows to follow, or stopped short of a world-class answer for no reason the case required. Its author knew both when asked, but nobody asked until the person did, after the work was done.

- **Best practice** is every practice that people who build this kind of thing know they should follow, and often skip. It is the floor.
- **World-class** is the best answer for this problem, with time and money no object. It is not over-engineering, and it is not built for a scale the work does not have. It runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. It is the target.

## 1. Before you design

Before you write a design, a recommendation or options, research and write down your answers to these three questions. Answer from the problem, before you sketch a design, so a design you already have in mind does not choose the answers.

1. What is best practice here? List every practice that applies to this kind of problem, each with a source you read (the tools' own documentation, two or three well-regarded projects) and why it applies here.
2. What is the world-class answer? Name the criteria that make it the best answer for this problem.
3. Which fact of this setup, if any, rules out a practice or the world-class answer? A cost in time, money or effort is not such a fact.

Then:

- Follow every best practice unless a fact of this case rules it out, and write that fact in the design. Less work, less money, no new credential, or a precedent here that does it another way is not such a fact.
- Design the world-class answer. Fall short of it only when a fact of this case requires it, and name each shortfall with its reason. Name a cost the person must accept as a cost, for them to decide.
- Settle a disputed fact with evidence: the documentation, the code or a probe. Ask the person only for a cost to accept or a choice of priority.
- In a spec, open the `Design` part with a `### Best practice and world-class` entry: each best practice with its source and whether the design follows it, the world-class answer, and each place the design falls short with its reason. The approver then sees the comparison without asking for it.

## 2. Before each approval request

Start two fresh subagents, one per brief. A fresh subagent starts with no history and gets only the text you send it. In each first-round reviewer's first message, give only the problem: what must work, for whom, the constraints and numbers, the person's words that describe the problem, and the setup. Do not include the proposal's title, design, options, decisions, recommendation or conclusion, or a person's words that state a proposal: a reviewer that reads the design first tends to accept it. After it answers, send that same subagent the whole proposal, never a path, and ask it to compare. If your harness cannot message a finished subagent, give a second fresh subagent the first reviewer's answer and the whole proposal, and ask it only to make that comparison. Where your harness offers a subagent on a different model family, use it for these reviews, because a reviewer on the author's model shares its blind spots.

Best practice:

```text
First message:
<The problem only.>

From this problem alone, list every best practice for this kind of problem: each practice that people who build such things know they should follow and often skip, with a source you read. Do not inspect or ask for a design.

Follow-up:
<The whole proposal.>

List each best practice the proposal does not follow, and whether it states a fact of this case that rules the practice out. Skipping a practice to save work, time, money or a credential is a finding. End with FOLLOWS BEST PRACTICE, or the findings.
```

World-class:

```text
First message:
<The problem only.>

Assume time and money are no object. World-class is not over-engineering and is not built for a scale the work does not have; it runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. From this problem alone, write what a world-class solution would look like. Do not inspect or ask for a design.

Follow-up:
<The whole proposal.>

List each place it accepts less than that answer (a manual step, a workaround, a second system, a narrower scope, a temporary fix), and whether the proposal gives a fact of this case that requires it. End with WORLD-CLASS, or the findings.
```

- Fix each finding from either review, or record it in the `### Best practice and world-class` entry with the fact or the cost that requires it. Never request approval while a finding is neither. A NOT WORLD-CLASS verdict whose findings are all fixed or recorded does not block the request.
- Write both reviewers' final verdicts in that entry, with each reviewer's model and the time it answered, and name the verdicts in the approval request, so the approver sees that the reviews ran and what they found.
- Run step 2 again before a later approval request when the design, the problem, its constraints or the evidence changed since the last review.
- Stop after three rounds of step 2 on one proposal: list each change made after the last round and each finding still open, with its cost, in the entry. Review loops between agents do not end by themselves.
- When a reviewer names a practice or an answer you did not consider, go back to step 1 with it instead of defending the design.
- When a person questions the approach, in any words, run step 1 again before you answer.
