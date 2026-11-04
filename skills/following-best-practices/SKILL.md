---
name: following-best-practices
description: "Use before choosing a design, writing options, a spec or plan, or requesting approval."
---

# Following best practices

A design a person sends back usually skipped practices that everyone who builds such things knows to follow, or stopped short of a world-class answer for no reason the case required. Its author knew both when asked, but nobody asked until the person did, after the work was done.

- **Best practice** is every practice that people who build this kind of thing know they should follow, and often skip. It is the floor.
- **World-class** is the best answer for this problem, with time and money no object. It is not over-engineering, and it is not built for a scale the work does not have. It runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. It is the target.

## 1. Before you design

Before you write a design, a recommendation or options, start a fresh subagent and send it only the problem: what must work, for whom, the constraints and numbers, the person's words that describe the problem, and the setup. Leave out every design, option or leaning you have, so a design you already have in mind does not choose the answers.

```text
<The problem only.>

From this problem alone, research and answer three questions. Do not ask for or propose a design.
1. What is best practice here? Every practice that applies to this kind of problem, as a table with one row per practice and the columns `Practice | Source | Why it applies to this case`. Source is a page you read (the tools' own documentation, two or three well-regarded projects), as a link. Why it applies names the facts of this problem that make the practice matter here; "it is a best practice" is not a reason. Put every practice you name in any answer below in this table, a practice a fact of the setup rules out included, and leave no cell empty.
2. What is the world-class answer? The criteria that make it the best answer for this problem.
3. Which fact of this setup, if any, rules out a practice or the world-class answer? A cost in time, money or effort is not such a fact.
```

Wait for its answers in the same turn: in Claude Code, start it with `run_in_background: false`; in omp, `wait` for its task. Never end your turn while it runs: in a headless session, the end of your turn stops every subagent still running, and its answers never arrive.

Then write the practice table yourself, in a file or a Dispatch comment, before any action that states or leans to a design: text you write, a file, a subagent brief, or a command's arguments, such as an issue title, even in a dry run. The table is step 1's record. It has one row for each practice its answers name, in the world-class criteria and the ruling-out facts too and a practice they rule out included, with the same three columns. Fill each row from its answers: a link it gives as the source, and a reason that names a fact of this problem. For each row whose source is not a link or whose reason names no fact of this problem, ask a research subagent for the missing cell (in Claude Code, a fresh subagent given its answers, started with `run_in_background: false`), and fill it from that answer, never from your own words. Do not take any action that states or leans to a design while a row is incomplete: an incomplete row is not a reason to drop the practice. Below the table, copy its world-class answer and the ruling-out facts. Recording them does not end your turn: go on with the work.

Then start a second fresh subagent, and wait for it in the same turn, to check the table. Send it the research subagent's whole answer and your table, and ask it to name every practice, every action a world-class criterion asks for (a drill or a test included) and every practice a fact rules out that has no row whose practice cell states that same thing. Add a row for each one it names, filled from the research answers or a research subagent as above, and check the new table again until it names none: in omp, send it the new table and wait; in Claude Code, start a new fresh checker with `run_in_background: false` and send it the whole answer and the new table. Its answer "no missing rows" ends step 1; take no action that states or leans to a design before it.

Then:

- Follow every best practice unless a fact of this case rules it out, and write that fact in the design. Less work, less money, no new credential, or a precedent here that does it another way is not such a fact.
- Design the world-class answer. Fall short of it only when a fact of this case requires it, and name each shortfall with its reason. Name a cost the person must accept as a cost, for them to decide.
- Settle a disputed fact with evidence: the documentation, the code or a probe. Ask the person only for a cost to accept or a choice of priority.
- In a spec, open the `Design` part with a `### Best practice and world-class` entry copied from step 1's table: each best practice with its source, why it applies to this case and whether the design follows it, the world-class answer, and each place the design falls short with its reason. The approver then sees the comparison without asking for it.

## 2. Before each approval request

Start two fresh subagents, one per brief, and wait for each answer in the same turn, as in step 1. A fresh subagent starts with no history and gets only the text you send it. In each first-round reviewer's first message, give only the problem: what must work, for whom, the constraints and numbers, the person's words that describe the problem, and the setup. Do not include the proposal's title, design, options, decisions, recommendation or conclusion, or a person's words that state a proposal: a reviewer that reads the design first tends to accept it. After it answers, paste the whole proposal's text into your next message to that same subagent and ask it to compare. A path, a file name, a summary, or a note in place of a table or section is not the proposal, even when the reviewer can read files: paste every part as you wrote it. If your harness cannot message a finished subagent and wait for its answer, give a second fresh subagent the first reviewer's answer and the whole proposal's text, and ask it only to make that comparison; in Claude Code, a message to a finished subagent resumes it in the background, so start that second subagent with `run_in_background: false` instead. Where your harness offers a subagent on a different model family, use it for these reviews, because a reviewer on the author's model shares its blind spots.

Best practice:

```text
First message:
<The problem only.>

From this problem alone, list every best practice for this kind of problem: each practice that people who build such things know they should follow and often skip, with a source you read. Do not inspect or ask for a design.

Follow-up:
<The whole proposal's text, pasted.>

List each best practice the proposal does not follow, and whether it states a fact of this case that rules the practice out. Skipping a practice to save work, time, money or a credential is a finding. End with FOLLOWS BEST PRACTICE, or the findings.
```

World-class:

```text
First message:
<The problem only.>

Assume time and money are no object. World-class is not over-engineering and is not built for a scale the work does not have; it runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. From this problem alone, write what a world-class solution would look like. Do not inspect or ask for a design.

Follow-up:
<The whole proposal's text, pasted.>

List each place it accepts less than that answer (a manual step, a workaround, a second system, a narrower scope, a temporary fix), and whether the proposal gives a fact of this case that requires it. End with WORLD-CLASS, or the findings.
```

- Fix each finding from either review, or record it in the `### Best practice and world-class` entry with the fact or the cost that requires it. Never request approval while a finding is neither. A NOT WORLD-CLASS verdict whose findings are all fixed or recorded does not block the request.
- Write the final round's verdicts of both reviewers in that entry, not in another section: for each reviewer, its own verdict, its model (such as `anthropic/claude-opus-5`, never its role or agent name), the id of its run (the agent id or session id your harness gave it; the two reviewers are two runs with two ids) and the clock time (UTC) its final answer arrived. Take that time from your harness at the moment the answer arrives: run `date -u` as your next action, write the time and the run id to a file at once, and copy the entry's times from that file. Never write the time later from your own clock, from the time you edit the entry, or from text the reviewer or you wrote. Name the verdicts in the approval request, so the approver sees that the reviews ran and what they found.
- Run step 2 again before a later approval request when the design, the problem, its constraints or the evidence changed since the last review.
- Stop after three rounds of step 2 on one proposal: list each change made after the last round and each finding still open, with its cost, in the entry. Review loops between agents do not end by themselves.
- When a reviewer names a practice or an answer you did not consider, go back to step 1 with it instead of defending the design.
- When a person questions the approach, in any words, run step 1 again before you answer.
